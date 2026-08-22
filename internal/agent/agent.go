package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"reasonix/internal/event"
	"reasonix/internal/jobs"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
	"reasonix/internal/toolpolicy"
)

// maxToolOutputBytes caps a single tool result before it goes into the model's
// context. ~32KB is roughly 8K tokens — enough for a full file read or a busy
// grep, while preventing one accidental "read this 5 MB log" from blowing the
// window before the next compaction runs.
const maxToolOutputBytes = 32 * 1024

// Renderer redraws the assistant's final-answer text as styled output. It is
// applied only after a turn's text stream completes, so the user sees raw
// markdown stream live, then a single redraw replaces it with formatted
// output. The renderer is intentionally interface-shaped so the agent stays
// independent of the cli's markdown library choice. Consumed by TextSink.
type Renderer interface {
	Render(text string) string
}

// Asker puts structured multiple-choice questions to the user and blocks for the
// answers. The agent consults it for the `ask` tool. It is interface-shaped so
// the agent stays independent of the frontend; a nil asker means no interactive
// user (headless runs), where `ask` returns a "decide for yourself" result. The
// interactive frontends wire the controller in as the Asker.
type Asker interface {
	Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error)
}

// callContextKey carries the executing tool call's identity into Execute.
type callContextKey struct{}

// callContext is the per-call context a tool can read. parentID is the call being
// executed and sink is the agent's event sink (the `task` tool uses both to nest
// a sub-agent's events under this call); asker lets the `ask` tool reach the user.
type callContext struct {
	parentID string
	sink     event.Sink
	asker    Asker
}

// withCallContext stamps ctx with the executing call's ID, the agent's sink, and
// the asker. executeOne sets this before every Execute; `task` reads it (via
// CallContext) to nest sub-agent events, and `ask` reads the asker to prompt.
func withCallContext(ctx context.Context, parentID string, sink event.Sink, asker Asker) context.Context {
	return context.WithValue(ctx, callContextKey{}, callContext{parentID: parentID, sink: sink, asker: asker})
}

// CallContext returns the executing call's ID, the agent's sink, and the asker,
// if the context was set by an agent's executeOne. ok is false for a plain
// context (headless tool tests, calls made outside the run loop).
func CallContext(ctx context.Context) (parentID string, sink event.Sink, asker Asker, ok bool) {
	cc, ok := ctx.Value(callContextKey{}).(callContext)
	if !ok {
		return "", nil, nil, false
	}
	return cc.parentID, cc.sink, cc.asker, true
}

// Gate decides, per tool call, whether it may run. It is an alias for the policy
// package's type, not a second declaration: the gate is product policy, and the
// engine only knows it through toolpolicy. The alias keeps the long-standing
// agent.Gate name working for every caller.
type Gate = toolpolicy.Gate

// ToolHooks fires user-configured shell hooks around each tool call. PreToolUse
// runs before the call and may block it (block=true; message is the reason fed
// back to the model); PostToolUse runs after and only surfaces output to the
// user (it can't block). It is interface-shaped so the agent stays independent
// of the hook package — a nil hooks field disables hook firing entirely.
type ToolHooks interface {
	PreToolUse(ctx context.Context, name string, args json.RawMessage) (block bool, message string)
	PostToolUse(ctx context.Context, name string, args json.RawMessage, result string)
	// PostLLMCall fires after each model turn completes (streaming finishes)
	// but before reasoning_content is stored. It returns the (possibly
	// translated) reasoning string — the original when no hook is configured.
	// HasPostLLMCall reports whether such a hook exists, so the agent keeps
	// streaming reasoning live when none is wired up.
	PostLLMCall(ctx context.Context, reasoning string, turn int) string
	HasPostLLMCall() bool
	// SubagentStop fires when a `task` sub-agent finishes (foreground). PreCompact
	// fires just before a compaction pass and returns extra summary guidance (its
	// hooks' stdout) to fold into the summary prompt; "" when no hook contributes.
	SubagentStop(ctx context.Context, last string)
	PreCompact(ctx context.Context, trigger string) string
}

// Agent drives a single task: a Provider, a tool Registry, and a Session wired
// into the main loop.
type Agent struct {
	prov        provider.Provider
	tools       *tool.Registry
	session     *Session
	sessMu      sync.Mutex // guards the session pointer for external Session()/SetSession
	maxSteps    int
	temperature float64
	pricing     *provider.Pricing

	// sink receives the turn's typed event stream (reasoning/text deltas, tool
	// dispatch/results, usage, notices). The agent no longer formats output
	// itself — a frontend's Sink decides how to render. Never nil; New defaults
	// it to event.Discard.
	sink event.Sink

	// lastUsage caches the most recent per-turn telemetry the provider reported so
	// the CLI can expose a context gauge without re-scraping the usage line. The
	// run loop writes it while a frontend's status line reads it, so it is atomic.
	lastUsage atomic.Pointer[provider.Usage]

	// sessCacheHit/sessCacheMiss accumulate cache tokens across every API call
	// this session, so frontends can show the aggregate hit-rate (Σhit/Σ(hit+miss))
	// — a steadier, cost-oriented number than the single-turn rate. They are NOT
	// reset on compaction (compaction only rewrites session.Messages), so the
	// aggregate never craters when the prefix is summarized away. Atomic: the run
	// loop accumulates them while the status line reads them.
	sessCacheHit  atomic.Int64
	sessCacheMiss atomic.Int64

	// policy is OneCreat's product policy around each tool call — plan mode, the
	// permission gate, PreToolUse/PostToolUse hooks, the checkpoint seam, the
	// evidence ledger, and the job/memory handles a tool reaches through its
	// context. It lives in its own package (internal/toolpolicy) precisely so it
	// is not welded to this loop: a second engine gets the same behaviour by
	// calling Before/After around its own execution (Plan 08 / A05).
	policy *toolpolicy.Pipeline

	// hooks, when non-nil, fires the hooks that bracket a *model turn* rather than
	// a tool call — PostLLMCall and PreCompact. The tool-call hooks are the
	// pipeline's; both point at the same runner. nil disables hook firing.
	hooks ToolHooks

	// asker, when non-nil, lets the `ask` tool put questions to the user. nil in
	// headless runs (no interactive user). Set via SetAsker.
	asker Asker

	// onCompact, when non-nil, fires after compact() rewrites the message log
	// in place (auto-compaction or manual /compact). The controller uses it to
	// invalidate its checkpoint boundaries (cpBound), since a Replace() shifts
	// message indices and stale boundaries would make rewind/fork cut at the
	// wrong place. Set via SetOnCompact.
	onCompact func()

	// Context management: when a turn's prompt nears contextWindow, the older
	// middle of the session is summarized away, keeping a token-bounded recent
	// tail verbatim (recentKeep is the message floor) and archiving the originals
	// under archiveDir. compactStuck latches when compaction can't get the prompt
	// under the window (consecutiveCompacts crosses the limit), so auto-compaction
	// pauses instead of looping.
	contextWindow       int
	compactRatio        float64
	recentKeep          int
	archiveDir          string
	compactStuck        bool
	consecutiveCompacts int

	// stormSig / stormCount track a run of turns that keep failing the same way so
	// the loop can break a death-spiral. The signature is each call's (tool, error)
	// in order, NOT (tool, args): a stuck model reliably reworks the arguments
	// cosmetically (a re-worded essay, a reordered object) while the call fails
	// identically every time — keying on args misses the loop entirely (observed
	// live against truncated tool-call arguments). Because errors that embed their
	// subject (e.g. "file not found: /x") differ per target, genuine varied probing
	// does not collapse to one signature. Reset whenever a turn does anything else
	// (a different failure shape, or any success). See applyStormBreaker.
	stormSig   string
	stormCount int
}

// SetPlanMode flips the read-only gate. While true, executeOne refuses any
// non-ReadOnly tool the model calls and returns a "blocked" result instead of
// running it. The cache-friendly bits — system prompt, tools schema, message
// history — are left untouched, so the toggle costs nothing in cache hits.
func (a *Agent) SetPlanMode(v bool) { a.policy.SetPlanMode(v) }

// SetGate installs the per-call permission gate. Used by `reasonix chat` to swap the
// headless gate built in setup for an interactive one that prompts the user;
// nil disables gating. Safe to call before the run loop starts.
func (a *Agent) SetGate(g Gate) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.policy.Gate = g
}

// SetAsker installs the asker the `ask` tool uses to question the user.
// Interactive frontends wire one in; headless runs leave it nil.
func (a *Agent) SetAsker(as Asker) { a.asker = as }

// Policy exposes the session's tool-call policy so the composition root can wire
// the seams it owns — the checkpoint pre-edit hook and the memory queue. The
// pipeline is created with the agent and never replaced, so a caller sets fields
// on it at construction, before any turn runs.
func (a *Agent) Policy() *toolpolicy.Pipeline { return a.policy }

// SetOnCompact installs the post-compaction callback (see onCompact). Set once at
// construction; not safe to change concurrently with a running turn.
func (a *Agent) SetOnCompact(fn func()) { a.onCompact = fn }

// Session returns the agent's current conversation, useful for persistence
// hooks that need to read the message log between turns. sessMu serialises this
// pointer read against SetSession, so a frontend (serve's concurrent /history and
// /new handlers) can't race the swap. The run loop touches a.session directly and
// only swaps it via SetSession while idle, so its reads need no lock.
func (a *Agent) Session() *Session {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	return a.session
}

// SetSession replaces the agent's conversation wholesale. Used by
// `reasonix chat --resume` to load a saved JSONL transcript before the first turn,
// so the model picks up exactly where it left off. Callers serialise it against a
// running turn (it only fires while idle); sessMu guards the pointer swap itself.
func (a *Agent) SetSession(s *Session) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	a.session = s
}

// LastUsage returns the most recent per-turn token telemetry the provider
// reported (nil if no turn has run yet). The TUI uses it to show a context
// gauge alongside the prompt; the actual cache decisions still live inside
// maybeCompact.
func (a *Agent) LastUsage() *provider.Usage { return a.lastUsage.Load() }

// SessionCache returns the cumulative cache hit/miss prompt tokens across every
// API call this session — the basis for the status line's aggregate hit-rate.
func (a *Agent) SessionCache() (hit, miss int) {
	return int(a.sessCacheHit.Load()), int(a.sessCacheMiss.Load())
}

// ContextWindow returns the configured context-window size in tokens. 0
// means compaction is disabled for this agent.
func (a *Agent) ContextWindow() int { return a.contextWindow }

// CompactRatio returns the fraction of the window at which auto-compaction
// fires (e.g. 0.8). The status line uses it to show headroom to the next compact.
func (a *Agent) CompactRatio() float64 { return a.compactRatio }

// CompactNow runs one compaction pass immediately, regardless of the
// usage-ratio threshold maybeCompact normally honours. Used by the chat
// TUI's `/compact` command so the user can reset the prefix before it
// naturally fills up.
func (a *Agent) CompactNow(ctx context.Context, instructions string) error {
	return a.compact(ctx, "manual", instructions)
}

// Options configures an Agent.
type Options struct {
	MaxSteps    int
	Temperature float64
	Pricing     *provider.Pricing // optional, for per-turn cost display

	// Gate is the per-call permission gate. nil disables gating.
	Gate Gate

	// Hooks fires PreToolUse / PostToolUse shell hooks around tool calls. nil
	// disables hook firing.
	Hooks ToolHooks

	// Jobs is the session's background-job manager (nil disables background tools).
	Jobs *jobs.Manager

	// Context management. ContextWindow <= 0 disables compaction. CompactRatio
	// is the trigger fraction; RecentKeep is the minimum recent messages kept
	// verbatim (the tail is otherwise token-bounded). Both fall back to defaults.
	ContextWindow int
	CompactRatio  float64
	RecentKeep    int
	ArchiveDir    string
}

// New constructs an Agent. MaxSteps <= 0 means no cap — the run loop continues
// until the model gives a final answer, the context is cancelled, or the
// provider errors (compaction keeps the context bounded). A nil sink is replaced
// with event.Discard so the agent can always emit unconditionally.
func New(prov provider.Provider, tools *tool.Registry, session *Session, opts Options, sink event.Sink) *Agent {
	if opts.CompactRatio <= 0 {
		opts.CompactRatio = defaultCompactRatio
	}
	if opts.RecentKeep <= 0 {
		opts.RecentKeep = minRecentKeep
	}
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	gate := opts.Gate
	if nilutil.IsNil(gate) {
		gate = nil
	}
	hooks := opts.Hooks
	if nilutil.IsNil(hooks) {
		hooks = nil
	}
	var callHooks toolpolicy.Hooks
	if hooks != nil {
		callHooks = hooks
	}
	return &Agent{
		prov:        prov,
		tools:       tools,
		session:     session,
		maxSteps:    opts.MaxSteps,
		temperature: opts.Temperature,
		pricing:     opts.Pricing,
		sink:        sink,
		hooks:       hooks,
		// Every product-side decision about a tool call lives in this one object,
		// so the loop below only has to ask "may I?" and "here is what happened".
		policy:        toolpolicy.New(gate, callHooks, opts.Jobs),
		contextWindow: opts.ContextWindow,
		compactRatio:  opts.CompactRatio,
		recentKeep:    opts.RecentKeep,
		archiveDir:    opts.ArchiveDir,
	}
}

// Run appends the user input and drives the tool loop until the model returns a
// final answer (no tool calls), the context is cancelled, or the provider errors.
// With maxSteps <= 0 the loop is unbounded — the natural termination is the model
// finishing, and the real safety bounds are user cancellation and compaction, not
// a round count. A positive maxSteps imposes an optional hard guard, surfaced as
// a resumable notice when hit.
func (a *Agent) Run(ctx context.Context, input string) error {
	a.policy.ResetTurn()
	a.sink.Emit(event.Event{Kind: event.TurnStarted})
	a.session.Add(provider.Message{Role: provider.RoleUser, Content: input})

	// todoReminded latches after the one turn-end todo reconcile reminder this
	// run is allowed: if the model still ends with unfinished items after being
	// nudged once, accept its answer instead of nagging in a loop.
	todoReminded := false

	for step := 0; a.maxSteps <= 0 || step < a.maxSteps; step++ {
		text, reasoning, signature, calls, usage, err := a.stream(ctx, step+1)
		if err != nil {
			return err
		}
		if usage != nil && usage.TotalTokens > 0 {
			a.sink.Emit(event.Event{Kind: event.Usage, Usage: usage, Pricing: a.pricing,
				SessionHit: int(a.sessCacheHit.Load()), SessionMiss: int(a.sessCacheMiss.Load())})
		}
		if msg, ok := finishReasonMessage(usage); ok {
			a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: msg})
		}

		// Keep reasoning_content on the assistant turn for display and session
		// archive. It is NOT re-uploaded to the API: the openai provider drops it
		// when building the request, since re-sent reasoning is billable prompt
		// input for no cache or coherence gain.
		a.session.Add(provider.Message{
			Role:               provider.RoleAssistant,
			Content:            text,
			ReasoningContent:   reasoning,
			ReasoningSignature: signature,
			ToolCalls:          calls,
		})

		if len(calls) == 0 {
			// Turn-end todo reconcile: if this turn updated the todo list and is
			// now ending with items still pending/in_progress, the UI would freeze
			// on a stale snapshot (the panel renders the last todo_write call, with
			// no independent state). Inject one reminder and give the model another
			// round to either finish the work or true-up the list. Must happen
			// inside this loop — a fresh Run() resets the evidence ledger, which
			// would strip the complete_step receipts the true-up needs.
			if reminder, ok := a.policy.EndOfTurnReminder(todoReminded); ok {
				todoReminded = true
				a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
					Text: "待办清单还有未完成项 — 已提醒模型对账后再收尾"})
				a.session.Add(provider.Message{Role: provider.RoleUser, Content: reminder})
				continue
			}
			return nil // model gave a final answer
		}

		results := a.executeBatch(ctx, calls)
		for i, call := range calls {
			a.session.Add(provider.Message{
				Role:       provider.RoleTool,
				Content:    results[i],
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}

		// The prompt only grows from here; compact before the next turn so it
		// stays within the model's window.
		a.maybeCompact(ctx, usage)
	}
	// Only reached when a positive maxSteps guard is configured. The work so far
	// is already in the session, so the user can just send another message to pick
	// up where it left off.
	return fmt.Errorf("paused after %d tool-call rounds (agent.max_steps) — the work so far is saved; send another message to continue, or set max_steps higher or to 0 for no limit", a.maxSteps)
}

// stream runs one completion, emitting reasoning and text deltas as typed
// events and collecting complete tool calls. A Message event closes the text
// stream so a sink can re-render the streamed raw text as styled markdown. The
// accumulated text and reasoning are also returned so the caller can round-trip
// reasoning on the next turn.
func (a *Agent) stream(ctx context.Context, turn int) (string, string, string, []provider.ToolCall, *provider.Usage, error) {
	ch, err := a.prov.Stream(ctx, provider.Request{
		Messages:    a.session.Messages,
		Tools:       a.tools.Schemas(),
		Temperature: a.temperature,
	})
	if err != nil {
		return "", "", "", nil, nil, err
	}

	// A PostLLMCall hook rewrites the whole reasoning block, so when one is wired
	// up we buffer reasoning silently and emit the transformed text once after the
	// stream. With no such hook the reasoning streams live, chunk by chunk, as
	// before — the common case must not lose its live "thinking…" display.
	transformReasoning := a.hooks != nil && a.hooks.HasPostLLMCall()

	var text, reasoning strings.Builder
	var signature string // provider-issued proof for the reasoning (Anthropic thinking)
	var calls []provider.ToolCall
	var usage *provider.Usage
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkReasoning:
			reasoning.WriteString(chunk.Text)
			if chunk.Signature != "" {
				signature = chunk.Signature
			}
			if chunk.Text != "" && !transformReasoning {
				a.sink.Emit(event.Event{Kind: event.Reasoning, Text: chunk.Text})
			}
		case provider.ChunkText:
			text.WriteString(chunk.Text)
			a.sink.Emit(event.Event{Kind: event.Text, Text: chunk.Text})
		case provider.ChunkToolCallStart:
			// Surface the tool card as soon as the call begins — before its
			// (possibly large) arguments finish streaming — so the user sees it
			// working instead of a stall. executeBatch emits the full dispatch
			// (with args) once the call completes; the frontend merges by ID.
			if tc := chunk.ToolCall; tc != nil {
				a.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: event.Tool{
					ID: tc.ID, Name: tc.Name, ReadOnly: a.toolReadOnly(tc.Name), Partial: true,
				}})
			}
		case provider.ChunkToolCall:
			calls = append(calls, *chunk.ToolCall)
		case provider.ChunkUsage:
			usage = chunk.Usage
			a.lastUsage.Store(chunk.Usage)
			a.sessCacheHit.Add(int64(chunk.Usage.CacheHitTokens))
			a.sessCacheMiss.Add(int64(chunk.Usage.CacheMissTokens))
		case provider.ChunkError:
			return "", "", "", nil, nil, chunk.Err
		}
	}
	// With a PostLLMCall hook, the live stream was suppressed above; transform the
	// full reasoning now and emit it once so the sink never sees the untranslated
	// text. Without a hook this is skipped — the chunk-by-chunk events already fired.
	original := reasoning.String()
	display := original
	if transformReasoning && original != "" {
		display = a.hooks.PostLLMCall(ctx, original, turn)
		if display != "" {
			a.sink.Emit(event.Event{Kind: event.Reasoning, Text: display})
		}
	}
	// Store the transformed reasoning — except when a provider signature pins it to
	// the original text (Anthropic extended thinking). That signed thinking block is
	// replayed verbatim on the next tool-call turn; re-uploading transformed text
	// under the original signature is rejected, so keep the original for storage
	// while the user still sees the transformed version live.
	stored := display
	if signature != "" {
		stored = original
	}
	// Close the text stream: a sink may re-render the streamed raw text as
	// styled markdown now that it is complete. Reasoning rides along so the sink
	// has the full chain if it wants it.
	if text.Len() > 0 || display != "" {
		a.sink.Emit(event.Event{Kind: event.Message, Text: text.String(), Reasoning: display})
	}
	return text.String(), stored, signature, calls, usage, nil
}

// executeBatch dispatches one model turn's tool calls. A ToolDispatch event is
// emitted for every call up front, in call order, so a frontend can show the
// timeline chronologically. Contiguous known ReadOnly calls fan out across
// goroutines; unknown and writer calls run as single-call serial segments so
// write/read ordering stays provider-ordered. ToolResult events are emitted
// after the batch in call order, so emission stays serial even when execution
// parallelised.
func (a *Agent) executeBatch(ctx context.Context, calls []provider.ToolCall) []string {
	for _, c := range calls {
		t, ok := a.tools.Get(c.Name)
		ev := event.Tool{ID: c.ID, Name: c.Name, Args: c.Arguments, ReadOnly: ok && t.ReadOnly()}
		if ok {
			if ch, ok := tool.PreviewChange(t, json.RawMessage(c.Arguments)); ok {
				ev.FileDiff = event.FileDiff{Diff: ch.Diff, Added: ch.Added, Removed: ch.Removed}
			}
		}
		a.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: ev})
	}

	results := make([]string, len(calls))
	outcomes := make([]toolOutcome, len(calls))
	run := func(i int) {
		outcomes[i] = a.executeOne(ctx, calls[i])
		results[i] = outcomes[i].output
	}

	for _, batch := range partitionToolCalls(a.tools, calls) {
		if batch.parallel && batch.end-batch.start > 1 {
			runParallel(batch.start, batch.end, run)
			continue
		}
		for i := batch.start; i < batch.end; i++ {
			run(i)
		}
	}

	for i, c := range calls {
		o := outcomes[i]
		t, ok := a.tools.Get(c.Name)
		a.sink.Emit(event.Event{Kind: event.ToolResult, Tool: event.Tool{
			ID:        c.ID,
			Name:      c.Name,
			Args:      c.Arguments,
			Output:    o.output,
			Err:       o.errMsg,
			ReadOnly:  ok && t.ReadOnly(),
			Truncated: o.truncated,
		}})
		if o.truncated && o.truncMsg != "" {
			a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: o.truncMsg})
		}
	}
	a.applyStormBreaker(calls, outcomes, results)
	return results
}

type toolCallBatch struct {
	start    int
	end      int
	parallel bool
}

// partitionToolCalls keeps provider order while letting contiguous known
// read-only tools run together. Unknown and writer tools are single-call serial
// batches so they cannot reorder around reads or produce surprising errors.
// complete_step and todo_write are read-only but never join a parallel run: they
// read the turn's evidence ledger, so every prior call's receipt must be recorded
// before they run.
func partitionToolCalls(r *tool.Registry, calls []provider.ToolCall) []toolCallBatch {
	var batches []toolCallBatch
	for i := 0; i < len(calls); {
		if parallelisable(r, calls[i].Name) {
			start := i
			i++
			for i < len(calls) && parallelisable(r, calls[i].Name) {
				i++
			}
			batches = append(batches, toolCallBatch{start: start, end: i, parallel: true})
			continue
		}
		batches = append(batches, toolCallBatch{start: i, end: i + 1})
		i++
	}
	return batches
}

func parallelisable(r *tool.Registry, name string) bool {
	if name == "complete_step" || name == "todo_write" {
		return false
	}
	t, ok := r.Get(name)
	return ok && t.ReadOnly()
}

func runParallel(start, end int, run func(int)) {
	const maxParallel = 8
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i := start; i < end; i++ {
		i := i
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			run(i)
		}()
	}
	wg.Wait()
}

// stormBreakThreshold is how many times in a row the same tool may fail the same
// way before the loop stops echoing the raw error back and instead returns a
// directive to change approach. Two natural self-corrections are healthy; the
// third identical failure is a death-spiral — the dominant case being a tool call
// whose arguments are truncated at the output-token ceiling, which the model then
// re-emits (re-worded but still over-long), truncating the same way again.
const stormBreakThreshold = 3

// applyStormBreaker detects a run of identically-failing turns and, past the
// threshold, rewrites the model-facing result (results[0]) into a directive to
// change approach. It keys on each call's (tool, error) — not its args — because a
// stuck model reworks the arguments cosmetically while failing identically (see
// the stormSig field doc). A turn is a fixation candidate only when every one of
// its calls errored and none was merely blocked by plan mode / permissions (those
// carry a clear, distinct message the model can already act on). Any success, any
// block, or a different batch shape is varied work, so it resets the counter. This
// covers both the single-call spiral and a repeated multi-call batch. The hard
// maxSteps guard remains the ultimate backstop; this just keeps the loop from
// burning that whole budget bouncing off the same failure.
func (a *Agent) applyStormBreaker(calls []provider.ToolCall, outcomes []toolOutcome, results []string) {
	sig, ok := batchStormSignature(calls, outcomes)
	if !ok {
		a.stormSig, a.stormCount = "", 0
		return
	}
	if sig != a.stormSig {
		a.stormSig, a.stormCount = sig, 1
		return
	}
	a.stormCount++
	if a.stormCount < stormBreakThreshold {
		return
	}
	subject := fmt.Sprintf("%q", calls[0].Name)
	short := calls[0].Name
	if len(calls) > 1 {
		subject = fmt.Sprintf("this batch of %d tool calls", len(calls))
		short = fmt.Sprintf("a batch of %d calls", len(calls))
	}
	results[0] = outcomes[0].output + fmt.Sprintf(
		"\n\n[loop guard] %s has now failed %d times in a row with the same error. Re-sending it — even with the wording changed — will not help: the calls keep failing the same way. Change approach: if an argument is being truncated, write less in one call and split the work into several smaller calls; otherwise fix the arguments, use a different tool, or explain the blocker in your final answer.",
		subject, a.stormCount)
	a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: fmt.Sprintf(
		"loop guard: %s failed %d× the same way — nudging the model to change approach",
		short, a.stormCount)})
}

// batchStormSignature returns a per-turn fixation signature — each call's
// (name, error) in order — and ok=true only when every call errored and none was
// merely blocked. ok=false (any success or block) means the turn made varied
// progress, so the caller resets the counter. Keying on the error rather than the
// args is deliberate: a stuck model reworks the arguments while failing the same
// way, so identical-args matching would miss the loop.
func batchStormSignature(calls []provider.ToolCall, outcomes []toolOutcome) (string, bool) {
	if len(calls) == 0 {
		return "", false
	}
	var sb strings.Builder
	for i := range calls {
		if outcomes[i].errMsg == "" || outcomes[i].blocked {
			return "", false
		}
		sb.WriteString(calls[i].Name)
		sb.WriteByte(0)
		sb.WriteString(outcomes[i].errMsg)
		sb.WriteByte(0)
	}
	return sb.String(), true
}

// toolOutcome is one tool call's result, split into the model-facing output and
// the display-facing notice bits. errMsg is the short failure reason (empty on
// success) — a refused call, an unknown tool, or an execution error — so a sink
// renders the result as failed ("⊘ name <errMsg>" / a red card) instead of OK;
// blocked narrows that to a refusal (plan mode / permission). truncMsg is set
// (without the "· " prefix) when the output was head+tailed.
type toolOutcome struct {
	output    string
	blocked   bool
	errMsg    string
	truncated bool
	truncMsg  string
}

// executeOne runs a single tool call. It is pure with respect to the event sink
// — the caller emits ToolDispatch/ToolResult — so it is safe to invoke from
// parallel goroutines.
func (a *Agent) executeOne(ctx context.Context, call provider.ToolCall) toolOutcome {
	t, ok := a.tools.Get(call.Name)
	if !ok {
		return toolOutcome{
			output: fmt.Sprintf("error: unknown tool %q", call.Name),
			errMsg: fmt.Sprintf("unknown tool %q", call.Name),
		}
	}
	pc := toolpolicy.Call{ID: call.ID, Name: call.Name, Args: json.RawMessage(call.Arguments), ReadOnly: t.ReadOnly()}
	// Only a tool that can describe its change is checkpointable; bash, whose
	// targets are unknowable, exposes no Previewer and is never snapshotted.
	if pv, ok := t.(tool.Previewer); ok {
		pc.Preview = pv.Preview
	}

	// Everything OneCreat adds around a call — plan mode, permissions, hooks,
	// checkpoints, the ledger, the job/memory handles — happens here.
	cctx, block := a.policy.Before(ctx, pc)
	if block != nil {
		return toolOutcome{output: block.Output, blocked: true, errMsg: block.Reason}
	}

	// The engine's own per-call plumbing: identity, the event sink a nested
	// sub-agent reports through, the asker the `ask` tool reaches, and the
	// progress callback that streams partial output.
	cctx = withCallContext(cctx, call.ID, a.sink, a.asker)
	callID := call.ID
	cctx = tool.WithProgress(cctx, func(chunk string) {
		a.sink.Emit(event.Event{Kind: event.ToolProgress, Tool: event.Tool{ID: callID, Output: chunk}})
	})

	result, err := t.Execute(cctx, json.RawMessage(call.Arguments))
	a.policy.After(ctx, pc, result, err)

	if err != nil {
		body, truncMsg := truncateToolOutput(fmt.Sprintf("error: %v\n%s", err, result), toolOutputLimit(call.Name))
		return toolOutcome{output: body, errMsg: firstLine(err.Error()), truncated: truncMsg != "", truncMsg: truncMsg}
	}
	// A foreground `task` sub-agent just finished — its result is the final answer.
	// (A backgrounded one returns a "Started…" string and stops later in a job, so
	// it doesn't fire here.)
	if call.Name == "task" && !isBackgroundTaskCall(call.Arguments) {
		a.policy.SubagentStopped(ctx, result)
	}
	body, truncMsg := truncateToolOutput(result, toolOutputLimit(call.Name))
	return toolOutcome{output: body, truncated: truncMsg != "", truncMsg: truncMsg}
}

// isBackgroundTaskCall reports whether a `task` call set run_in_background, so a
// fire-and-return dispatch isn't mistaken for a sub-agent that has stopped.
func isBackgroundTaskCall(args string) bool {
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	_ = json.Unmarshal([]byte(args), &p)
	return p.RunInBackground
}

// toolReadOnly reports a tool's ReadOnly classification by name (false for an
// unknown tool), for stamping early ToolDispatch events.
func (a *Agent) toolReadOnly(name string) bool {
	t, ok := a.tools.Get(name)
	return ok && t.ReadOnly()
}

// firstLine returns s up to its first newline — a one-line failure summary for
// the display Err, while the full error stays in the model-facing output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// skillToolOutputBytes 是 run_skill 单独的输出上限。run_skill 的结果是技能正文
// (用户本地的可信指令),教案/竞赛论文这类大技能 70KB+ 很常见;按通用 32KB 头尾
// 截断会把技能方法论拦腰砍掉,模型只看到半套指令、执行残缺。放宽到 256KB——
// 仍保留上限防病态技能,但完整覆盖现有最大技能。
const skillToolOutputBytes = 256 * 1024

// toolOutputLimit returns the per-tool result cap fed to truncateToolOutput.
func toolOutputLimit(name string) int {
	if name == "run_skill" {
		return skillToolOutputBytes
	}
	return maxToolOutputBytes
}

// truncateToolOutput head+tails s when it exceeds limit, slicing
// on rune boundaries so we never split a multibyte glyph. Returns the possibly
// trimmed body plus a one-line user-facing notice when truncation happened
// (empty when it didn't, without the "· " display prefix).
func truncateToolOutput(s string, limit int) (string, string) {
	if limit <= 0 {
		limit = maxToolOutputBytes
	}
	if len(s) <= limit {
		return s, ""
	}
	keep := limit / 2
	headEnd := runeStartAtOrAfter(s, keep)
	tailStart := runeStartAtOrBefore(s, len(s)-keep)
	// 短输出(len(s) 略超 limit)+ 边界落在多字节字符中间时,head 尾外推、tail 头外推
	// 可能重叠(headEnd >= tailStart):那样中段会重复、omitted 变负、提示显示「-N bytes」。
	// 重叠就不截断,直接返回原文(C5)。
	if headEnd >= tailStart {
		return s, ""
	}
	head := s[:headEnd]
	tail := s[tailStart:]
	omitted := len(s) - len(head) - len(tail) // headEnd < tailStart 保证 > 0
	notice := fmt.Sprintf("tool output truncated: %d of %d bytes elided", omitted, len(s))
	body := head + fmt.Sprintf("\n\n…[truncated %d of %d bytes — rerun with narrower args to see the middle]…\n\n", omitted, len(s)) + tail
	return body, notice
}

// runeStartAtOrAfter 把下标 i 向后(增大)挪到最近的 rune 起始位置(用于 head 的右界)。
func runeStartAtOrAfter(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// runeStartAtOrBefore 把下标 i 向前(减小)挪到最近的 rune 起始位置(用于 tail 的左界)。
func runeStartAtOrBefore(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// finishReasonMessage maps an abnormal finish_reason to a one-line warning,
// returning ok=false for the normal terminations ("stop", "tool_calls") and a
// nil usage. The sink renders the message; the "! " prefix is presentation.
func finishReasonMessage(u *provider.Usage) (string, bool) {
	if u == nil {
		return "", false
	}
	switch u.FinishReason {
	case "length":
		return "response truncated: hit max output tokens", true
	case "content_filter":
		return "response blocked by content filter", true
	case "repetition_truncation":
		return "response truncated: model repetition detected", true
	default:
		return "", false
	}
}
