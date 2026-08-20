// Package control is the transport-agnostic session driver. A Controller owns
// the agent run loop and session lifecycle, takes commands (Send/Cancel/Approve/
// SetPlanMode/Compact/NewSession/…), and emits everything that happens —
// reasoning, tool calls, approvals, turn completion — as a typed event stream to
// a single event.Sink.
//
// The point is one orchestration layer behind every frontend: a terminal TUI, a
// desktop webview, or an HTTP/SSE server each drive the Controller identically
// (issue commands, render events) and none of them re-implement turn lifecycle,
// cancellation, or approval. The Controller depends on no frontend.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/checkpoint"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/hook"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/nilutil"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
)

// Controller drives one chat session. Construct with New; drive with the command
// methods; observe through the Sink passed in Options.
type Controller struct {
	runner   agent.Runner
	executor *agent.Agent
	sink     event.Sink

	// approvals owns every "stop and ask the human" interaction — tool-approval
	// prompts, the `ask` tool's questions, session grants, YOLO/bypass and the
	// just-approved-plan window (see approval.go). The Controller only forwards.
	approvals *approvalBroker

	label        string
	systemPrompt string
	// session owns where the conversation persists and the auto-save that keeps
	// it there (see session_store.go). It does not own the message log — that is
	// the agent's Session, single-writer by design.
	session  *sessionStore
	commands []command.Command
	skills   []skill.Skill
	hooks    *hook.Runner // session hook runner; nil-safe (no hooks configured)
	// memory owns the session's memory snapshot and the notes queued to ride the
	// next turn (see memory.go). A mid-session write must never touch the
	// cache-stable system prefix.
	memory     *memoryService
	cleanup    func()
	autoPlan   string
	classifier autoPlanClassifier
	// startedOnce guards the one-shot SessionStart hook on the first turn. It is a
	// CompareAndSwap rather than sync.Once on purpose: the original semantics let a
	// second caller return immediately instead of blocking until the hook finishes.
	startedOnce atomic.Bool

	// balanceURL/balanceKey target the active provider's optional wallet-balance
	// endpoint (empty when the provider declares none). Captured at build so a
	// model/key switch — which rebuilds the controller — refreshes them.
	balanceURL    string
	balanceKey    string
	balanceClient *http.Client

	// jobs is the session-scoped background-job manager. The agent's background
	// tools spawn into it; Compose drains its completion notes into the next turn;
	// Close cancels its still-running jobs.
	jobs *jobs.Manager

	// mcp owns this session's MCP servers: the plugin host, the live tool registry
	// the executor reads each turn, and the context hot-added stdio servers bind
	// their subprocesses to (see mcp.go). Connecting a server mid-session makes its
	// tools available on the next turn.
	mcp *mcpService
	// reg is kept for ToolNames (a read-only status readout).
	reg *tool.Registry

	// ckpt owns snapshot-based rewind: the per-session checkpoint store, the
	// monotonic turn counter and the turn→message-index boundaries (see
	// checkpoints.go). wsRoot is this session's workspace root — the checkpoint
	// service confines restore writes to it, and it is also the project directory
	// workspace-scoped subprocesses (CodeGraph) are pinned to.
	ckpt   *checkpointService
	wsRoot string

	// turn arbitrates who may touch the conversation right now (a model turn vs an
	// exclusive log-rewriting operation) and carries the per-turn runtime flags —
	// plan mode and the coaching persona, injected at Compose time and never part
	// of the cached system prefix (see turn_state.go).
	turn *turnState
}

// Options carries the already-built pieces setup assembles. Lifecycle metadata
// lets the controller mint and rotate session files; Host/Commands are surfaced
// to frontends that resolve MCP prompts and slash commands.
type Options struct {
	Runner       agent.Runner
	Executor     *agent.Agent
	Sink         event.Sink
	Policy       permission.Policy
	Label        string
	SystemPrompt string
	SessionDir   string
	SessionPath  string
	Host         *plugin.Host
	Commands     []command.Command
	Skills       []skill.Skill
	Hooks        *hook.Runner
	Memory       *memory.Set
	Cleanup      func()
	// BalanceURL/BalanceKey wire the active provider's optional wallet-balance
	// endpoint and bearer key; empty when the provider declares no balance_url.
	BalanceURL    string
	BalanceKey    string
	BalanceClient *http.Client
	// Jobs is the session-scoped background-job manager (nil disables background jobs).
	Jobs *jobs.Manager
	// Registry is the executor's live tool set, and PluginCtx the session-scoped
	// context; both are needed for hot-adding MCP servers via AddMCPServer.
	Registry  *tool.Registry
	PluginCtx context.Context
	// WorkspaceRoot is this session's project root: checkpoint restores are
	// confined to it ("" = no confinement) and workspace-scoped subprocesses are
	// pinned to it. boot.Build fills it from its workspace.Context, so a frontend
	// holding several projects at once gets one root per session rather than the
	// shared process working directory.
	WorkspaceRoot string
	AutoPlan      string
	Classifier    autoPlanClassifier
}

// New builds a Controller. A nil Sink is replaced with event.Discard.
func New(opts Options) *Controller {
	sink := opts.Sink
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	classifier := opts.Classifier
	if nilutil.IsNil(classifier) {
		classifier = nil
	}
	pluginCtx := opts.PluginCtx
	if pluginCtx == nil {
		pluginCtx = context.Background()
	}
	c := &Controller{
		runner:        opts.Runner,
		executor:      opts.Executor,
		sink:          sink,
		approvals:     newApprovalBroker(sink, opts.Policy, opts.Hooks),
		label:         opts.Label,
		systemPrompt:  opts.SystemPrompt,
		commands:      opts.Commands,
		skills:        opts.Skills,
		hooks:         opts.Hooks,
		memory:        newMemoryService(opts.Memory),
		cleanup:       opts.Cleanup,
		autoPlan:      normalizeAutoPlan(opts.AutoPlan),
		classifier:    classifier,
		balanceURL:    opts.BalanceURL,
		balanceKey:    opts.BalanceKey,
		balanceClient: opts.BalanceClient,
		jobs:          opts.Jobs,
		reg:           opts.Registry,
		mcp:           newMCPService(sink, opts.Host, opts.Registry, pluginCtx, opts.WorkspaceRoot),
		session:       newSessionStore(opts.SessionDir, opts.SessionPath, opts.Executor),
		turn:          newTurnState(sink, opts.Executor),
		wsRoot:        opts.WorkspaceRoot,
		ckpt:          newCheckpointService(opts.WorkspaceRoot),
	}
	// Checkpoints: bind a store to the session and route writer pre-edits into it.
	c.ckpt.Rebind(opts.SessionPath)
	if c.executor != nil {
		// 两条属于产品策略、由装配层接上的缝(见 internal/toolpolicy):写工具执行前
		// 把文件原内容快照进 checkpoint;记忆工具把「本轮生效」的注记排进下一轮。
		pol := c.executor.Policy()
		pol.PreEdit = c.ckpt.Snapshot
		pol.Memory = c.memory
		// 压缩(自动或手动)会原地重写日志、改变消息下标 → 失效 checkpoint 边界(B1)。
		c.executor.SetOnCompact(c.ckpt.Invalidate)
	}
	return c
}

// --- commands (frontend → controller) ---

// Send starts a turn with an uncomposed message. The controller applies
// auto-plan, plan-mode, memory, and background-job framing inside the async turn
// path so frontends do not block on classifier I/O.
func (c *Controller) Send(input string) {
	c.SendWithRaw(input, input)
}

// SendWithRaw starts a turn with separate model input and raw prompt text. The
// raw prompt is used only for auto-plan scoring; it deliberately excludes
// resolved @-reference payloads so referenced file contents cannot inflate the
// complexity score.
func (c *Controller) SendWithRaw(input, raw string) {
	c.turn.Guarded(func(ctx context.Context) error { return c.runTurnWithRaw(ctx, input, raw) })
}

// planApprovalTool is the Tool name on the ApprovalRequest the controller emits
// to gate a proposed plan. Frontends key their plan-approval UI on it (the
// desktop renders a plan card; the chat TUI a plan banner).
const planApprovalTool = "exit_plan_mode"

// planApprovedMessage is the follow-up turn sent once the user approves a plan —
// the in-context nudge to execute and keep the (already-seeded) task list honest.
const planApprovedMessage = "Plan approved — plan mode is off; you're cleared to make the changes without asking again. Implement the plan now. Keep the task list current with todo_write, preserving its two-level shape (phases at level 0, their sub-steps at level 1): mark the sub-step you start as in_progress, one in_progress at a time. Sign off each finished sub-step with complete_step, attaching the evidence it's done — the verification you ran, the diff/files you changed, or a manual check. Don't claim a step is done without evidence."

// runTurn runs one model turn, then applies the plan-approval gate. This is the
// single, frontend-agnostic plan flow: in plan mode the model just researches
// (writers are blocked) and writes its plan as a normal answer — no special tool.
// When the turn ends with a text proposal, the controller asks the user to
// approve (reusing the ApprovalRequest channel both frontends already render);
// on approval it exits plan mode, seeds the task list from the plan, and
// continues straight into execution; on rejection it stays in plan mode so the
// next turn can revise. Plan mode is only ever set interactively, so the headless
// `Run` path (which doesn't call this) never blocks on a prompt.
func (c *Controller) runTurn(ctx context.Context, input string) error {
	return c.runTurnWithRaw(ctx, input, input)
}

func (c *Controller) runTurnWithRaw(ctx context.Context, input, raw string) error {
	c.maybeSessionStart(ctx)
	c.maybeAutoPlan(ctx, raw)
	input = c.Compose(input)
	startMessages := c.session.MessageCount()
	defer c.session.SaveActivityIfChanged(startMessages)
	// Open a checkpoint for this turn before the user message is appended, so the
	// recorded message boundary precedes it and pre-edit snapshots land here.
	c.beginTurnCheckpoint(input)
	// UserPromptSubmit / Stop hooks bracket the whole turn (incl. the plan
	// research + approved-execution sub-turns below): a gating UserPromptSubmit
	// aborts before any model call; Stop fires once when the turn returns.
	if c.hooks.Enabled() {
		turn := c.turn.NextTurn()
		if block, _ := c.hooks.PromptSubmit(ctx, input, turn); block {
			return nil // the hook's notify callback already surfaced the reason
		}
		defer func() { c.hooks.Stop(ctx, lastAssistantText(c.session.History()), turn) }()
	}
	if err := c.runner.Run(ctx, input); err != nil {
		return err
	}
	if !c.turn.PlanMode() {
		return nil
	}
	proposal := lastAssistantText(c.session.History())
	if proposal == "" {
		return nil // no substantive proposal to gate
	}
	// The plan is already visible as the assistant's answer, so the request
	// carries no subject — it's purely the gate.
	allow, _, err := c.approvals.Request(ctx, planApprovalTool, "")
	if err != nil {
		return err
	}
	if !allow {
		return nil // keep planning; plan mode stays on
	}
	c.turn.SetPlanMode(false)
	c.seedPlanTodos(proposal)
	// The plan is the go-ahead: don't re-prompt for each write of the approved
	// work. Auto-approve writers for the duration of this execution turn only.
	c.approvals.SetAutoApprove(true)
	defer c.approvals.SetAutoApprove(false)
	return c.runner.Run(ctx, planApprovedMessage)
}

// lastAssistantText returns the content of the most recent assistant message with
// non-empty text — the model's final answer for the turn (its plan, in plan mode).
func lastAssistantText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// Run executes a turn synchronously, returning the agent's error. Used by the
// headless `reasonix run` path, where the Sink renders to stdout and the caller
// just needs the exit status — no TurnDone event, no cancel bookkeeping.
func (c *Controller) Run(ctx context.Context, input string) error {
	c.maybeSessionStart(ctx)
	startMessages := c.session.MessageCount()
	defer c.session.SaveActivityIfChanged(startMessages)
	if c.hooks.Enabled() {
		turn := c.turn.NextTurn()
		if block, _ := c.hooks.PromptSubmit(ctx, input, turn); block {
			return nil
		}
		defer func() { c.hooks.Stop(ctx, lastAssistantText(c.session.History()), turn) }()
	}
	return c.runner.Run(ctx, input)
}

// EnableInteractiveApproval swaps the executor's gate for one that routes "ask"
// decisions to the frontend via ApprovalRequest events, and wires the controller
// in as the executor's Asker so the `ask` tool can question the user. Interactive
// frontends (chat, desktop) call this; the headless run keeps the silent gate and
// a nil asker from setup.
func (c *Controller) EnableInteractiveApproval() {
	if c.executor != nil {
		c.executor.SetGate(c.approvals.Gate())
		c.executor.SetAsker(c)
	}
}

// Compact runs one compaction pass on the executor's session on demand.
// instructions is optional `/compact <focus>` guidance steering what to keep.
func (c *Controller) Compact(ctx context.Context, instructions string) error {
	if c.executor == nil {
		return nil
	}
	// turn 进行中、或另一个会话重写 op 进行中,都拒绝:compact() 会 Snapshot→多秒摘要
	// 网络调用→整体 Replace,期间并发的 turn(Session.Add)会被基于旧快照的 Replace
	// 整轮覆盖丢掉(B2 双向)。守卫覆盖整个 Snapshot→summarize→Replace 跨度。
	if !c.turn.TryBeginExclusive() {
		return c.busyNotice("正在运行中,请等当前轮结束再压缩")
	}
	defer c.turn.EndExclusive()
	return c.executor.CompactNow(ctx, instructions)
}

// busyNotice 在有 turn 运行时拒绝「会重写整个会话」的操作:发一条 Warn Notice(前端
// 即便吞掉返回的 error 也能看到原因),并返回该错误(B2)。
func (c *Controller) busyNotice(msg string) error {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: msg})
	return fmt.Errorf("%s", msg)
}

// maybeSessionStart fires the SessionStart hook exactly once per session, lazily
// on the first turn — by then the sink/notify is wired, and a resumed session
// fires it too (its first post-resume turn).
func (c *Controller) maybeSessionStart(ctx context.Context) {
	if !c.startedOnce.CompareAndSwap(false, true) {
		return
	}
	c.hooks.SessionStart(ctx)
}

// NewSession snapshots the current conversation, rotates to a fresh file, and
// resets the executor to a clean session carrying the same system prompt. It
// ends the old session and starts the new one for lifecycle hooks.
func (c *Controller) NewSession() error {
	if c.executor == nil {
		return nil
	}
	// turn 进行中、或另一个会话重写 op 进行中,都拒绝:Snapshot→SetSession 换会话指针,
	// 期间并发的 turn(Session.Add)会落到混合状态/被丢掉(B2 双向)。
	if !c.turn.TryBeginExclusive() {
		return c.busyNotice("正在运行中,请等当前轮结束再新建会话")
	}
	defer c.turn.EndExclusive()
	if err := c.session.Save(); err != nil {
		return err
	}
	c.hooks.SessionEnd(context.Background())
	if c.session.dir != "" {
		c.session.SetPath(agent.NewSessionPath(c.session.dir, c.label))
	}
	c.executor.SetSession(agent.NewSession(c.systemPrompt))
	c.ckpt.Rebind(c.session.Path())
	c.startedOnce.Store(true) // NewSession fires SessionStart itself; don't re-fire on the next turn
	c.hooks.SessionStart(context.Background())
	return nil
}

// RewindScope selects what a Rewind restores.
type RewindScope int

const (
	RewindCode         RewindScope = iota // files only
	RewindConversation                    // message log only
	RewindBoth                            // both
)

// rewindFail emits the error as a Warn notice (so a frontend that swallows the
// returned error — e.g. the desktop bridge's .catch — still shows the user why
// the rewind did nothing) and returns it.
func (c *Controller) rewindFail(err error) error {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: err.Error()})
	return err
}

// Rewind restores the session to the start of `turn`: Code reverts every file that
// turn (or a later one) changed to its pre-turn content; Conversation truncates the
// message log back to that turn; Both does both. Refused while a turn is running.
// Conversation rewind relies on the live boundary recorded at turn start, so it is
// unavailable for turns inherited from a resumed session (code rewind still works).
// Frontends re-render their transcript from History after the call.
func (c *Controller) Rewind(turn int, scope RewindScope) error {
	if !c.ckpt.Available() || c.executor == nil {
		return c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if !c.turn.TryBeginExclusive() {
		return c.rewindFail(fmt.Errorf("cannot rewind while another operation is running"))
	}
	defer c.turn.EndExclusive()
	boundary, hasBound := c.ckpt.Bound(turn)

	if scope == RewindCode || scope == RewindBoth {
		written, deleted, err := c.ckpt.RestoreCode(turn)
		if err != nil {
			return c.rewindFail(fmt.Errorf("rewind code: %w", err))
		}
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
			Text: fmt.Sprintf("rewound code to turn %d — %d file(s) restored, %d removed", turn, len(written), len(deleted))})
	}
	if scope == RewindConversation || scope == RewindBoth {
		if !hasBound {
			return c.rewindFail(fmt.Errorf("conversation rewind unavailable for turn %d (resumed session)", turn))
		}
		// 持锁截断;boundary 越界(如压缩后边界已失效)时 Truncate 返回 false——此时
		// 必须报失败而不是假装成功,否则用户以为回退了、上下文却原封不动(B3/B4)。
		if !c.executor.Session().Truncate(boundary) {
			return c.rewindFail(fmt.Errorf("无法回退到 turn %d:会话已被压缩或边界失效", turn))
		}
		c.ckpt.TruncateFrom(turn)
		if err := c.session.Save(); err != nil {
			slog.Warn("controller: snapshot after rewind", "err", err)
		}
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
			Text: fmt.Sprintf("rewound conversation to turn %d", turn)})
	}
	return nil
}

// SummarizeFrom compresses the conversation from turn onward into one summary;
// SummarizeUpTo compresses everything before it. Both are Claude Code's "summarize
// from/up to here" — they restructure the message log (keeping code untouched), so
// afterwards the per-turn boundaries no longer map and conversation rewind/fork
// report "unavailable" until new turns rebuild them (code rewind, file-based, is
// unaffected). Refused while a turn runs; need the live boundary.
func (c *Controller) SummarizeFrom(ctx context.Context, turn int) error {
	return c.summarizeAt(ctx, turn, true)
}

func (c *Controller) SummarizeUpTo(ctx context.Context, turn int) error {
	return c.summarizeAt(ctx, turn, false)
}

func (c *Controller) summarizeAt(ctx context.Context, turn int, from bool) error {
	if c.executor == nil {
		return c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if !c.turn.TryBeginExclusive() {
		return c.rewindFail(fmt.Errorf("cannot summarize while another operation is running"))
	}
	defer c.turn.EndExclusive()
	boundary, hasBound := c.ckpt.Bound(turn)
	if !hasBound {
		return c.rewindFail(fmt.Errorf("summarize unavailable for turn %d (resumed session)", turn))
	}
	var err error
	if from {
		err = c.executor.SummarizeFrom(ctx, boundary)
	} else {
		err = c.executor.SummarizeUpTo(ctx, boundary)
	}
	if err != nil {
		return c.rewindFail(err)
	}
	// 日志被原地重写,现存边界的 MsgIndex 全部失真。走与压缩完全相同的失效路径:清内存
	// cpBound 的同时把"边界失效"持久化到磁盘(boundsMin),否则 resume 后 rebindCheckpoints
	// 会从磁盘 checkpoint 的陈旧 MsgIndex 无条件回填 cpBound,对 summarize 前的 turn 做对话
	// rewind 会静默切到错误偏移(悬空 tool_calls → 请求 OpenAI 兼容端点 400)。
	c.InvalidateCheckpoints()
	if err := c.session.Save(); err != nil {
		slog.Warn("controller: post-summarize snapshot", "err", err)
	}
	return nil
}

// ContextSnapshot returns (promptTokens, contextWindow) from the most recent
// turn. Both zero means no data yet — a gauge hides itself.
func (c *Controller) ContextSnapshot() (int, int) {
	if c.executor == nil {
		return 0, 0
	}
	u := c.executor.LastUsage()
	if u == nil {
		return 0, c.executor.ContextWindow()
	}
	return u.PromptTokens, c.executor.ContextWindow()
}

// CompactRatio returns the auto-compaction threshold as a fraction of the window
// (0 when the executor is unset). The status line shows headroom against it.
func (c *Controller) CompactRatio() float64 {
	if c.executor == nil {
		return 0
	}
	return c.executor.CompactRatio()
}

// LastUsage returns the most recent turn's token telemetry (nil before the first
// turn), so frontends can derive the prompt cache-hit rate for the status line.
func (c *Controller) LastUsage() *provider.Usage {
	if c.executor == nil {
		return nil
	}
	return c.executor.LastUsage()
}

// SessionCache returns cumulative cache hit/miss prompt tokens for the session,
// so a frontend can render the aggregate (session-wide) cache-hit rate — steadier
// than the single-turn rate and unaffected by compaction.
func (c *Controller) SessionCache() (hit, miss int) {
	if c.executor == nil {
		return 0, 0
	}
	return c.executor.SessionCache()
}

// Balance queries the active provider's wallet balance, or (nil, nil) when the
// provider declares no balance_url — so a caller treats "not configured" and
// "fetched" the same and just omits the readout when nil.
func (c *Controller) Balance(ctx context.Context) (*billing.Balance, error) {
	if strings.TrimSpace(c.balanceURL) == "" {
		return nil, nil
	}
	return billing.FetchWithClient(ctx, c.balanceClient, c.balanceURL, c.balanceKey)
}

// Commands returns the loaded custom slash commands.
func (c *Controller) Commands() []command.Command { return c.commands }

// Skills returns the discoverable skills (for the slash menu and `/skill`).
func (c *Controller) Skills() []skill.Skill { return c.skills }

// ToolNames lists the tools this session can call. It makes the composition
// root's decisions observable — which optional services (LSP, CodeGraph, plugin
// tiers) were actually wired into this session — without exposing the mutable
// registry itself.
func (c *Controller) ToolNames() []string {
	if c.reg == nil {
		return nil
	}
	return c.reg.Names()
}

// HookRunner returns the session's hook runner (nil-safe; may hold zero hooks),
// so a frontend can list the active hooks via `/hooks`.
func (c *Controller) HookRunner() *hook.Runner { return c.hooks }

// Label returns the human-readable model label, e.g. "deepseek-flash".
func (c *Controller) Label() string { return c.label }

// SystemPrompt returns the resolved system prompt this controller was built with
// (used to verify assembly, e.g. that the gateway path folded in ModelPrivacyPolicy).
func (c *Controller) SystemPrompt() string { return c.systemPrompt }

// Close stops plugin subprocesses and releases resources. A session that ever
// started fires SessionEnd so a teardown hook runs.
//
// Close first cancels any in-flight turn: the turn's context is derived from
// context.Background() (see runGuarded), not the controller lifetime, so without
// this a controller torn down mid-turn (tab close, model switch, workspace switch,
// app shutdown) would leave an orphan goroutine that keeps streaming from the
// provider, executing tools against the workspace, and burning gateway credits with
// no handle left to stop it.
func (c *Controller) Close() {
	c.Cancel()
	if c.startedOnce.Load() {
		c.hooks.SessionEnd(context.Background())
	}
	if c.jobs != nil {
		c.jobs.Close() // cancel any still-running background jobs
	}
	if c.cleanup != nil {
		c.cleanup()
	}
}

// Jobs returns the still-running background jobs for the status bar (nil when
// background jobs are disabled).
func (c *Controller) Jobs() []jobs.View {
	if c.jobs == nil {
		return nil
	}
	return c.jobs.Running()
}

// --- memory ---
//
// c.mem is treated as an immutable snapshot guarded by c.mu: reads take the lock
// and return the pointer; writes mutate disk then swap in a freshly discovered
// snapshot. A turn-tail note is queued for each write so the change applies this
// session without disturbing the cache-stable system prefix (it folds into the
// prefix on the next session). All of these are no-ops returning "" when memory
// is disabled.

// --- approval bridge (agent gate → events) ---

// --- approvals / asks (forwarded to approvalBroker; see approval.go) ---

// Approve answers a pending approval prompt by id.
func (c *Controller) Approve(id string, allow, session bool) { c.approvals.Resolve(id, allow, session) }

// PendingApprovals 返回当前已发出、尚未应答的审批请求(用于切回标签时重放)。
func (c *Controller) PendingApprovals() []event.Approval { return c.approvals.PendingApprovals() }

// PendingAsks 返回当前已发出、尚未应答的 ask 请求(用于切回标签时重放)。
func (c *Controller) PendingAsks() []event.Ask { return c.approvals.PendingAsks() }

// Ask implements agent.Asker so the `ask` tool can question the user.
func (c *Controller) Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error) {
	return c.approvals.Ask(ctx, questions)
}

// AnswerQuestion resolves a pending AskRequest by ID with the user's selections.
func (c *Controller) AnswerQuestion(id string, answers []event.AskAnswer) {
	c.approvals.AnswerQuestion(id, answers)
}

// SetBypass turns YOLO/bypass mode on or off for the session.
func (c *Controller) SetBypass(on bool) { c.approvals.SetBypass(on) }

// Bypass reports whether YOLO/bypass mode is on, for the status-bar indicator.
func (c *Controller) Bypass() bool { return c.approvals.Bypass() }

// --- checkpoints (forwarded to checkpointService; see checkpoints.go) ---

// InvalidateCheckpoints drops the turn→message-index boundaries after something
// rewrote the message log (compaction, summarize), so conversation rewind and
// fork report "unavailable" instead of truncating to a stale index.
func (c *Controller) InvalidateCheckpoints() { c.ckpt.Invalidate() }

// Checkpoints lists the session's rewind points (one per user turn), oldest first.
func (c *Controller) Checkpoints() []checkpoint.Meta { return c.ckpt.List() }

// beginTurnCheckpoint opens a checkpoint for the turn about to run, recording the
// current message count as the conversation-rewind boundary. Called at the top of
// runTurn, before the user message is appended.
func (c *Controller) beginTurnCheckpoint(input string) {
	if c.executor == nil {
		return
	}
	c.ckpt.Begin(input, len(c.executor.Session().Messages))
}

// --- MCP servers (forwarded to mcpService; see mcp.go) ---

// Host returns the running MCP host (nil when no plugins), for frontends that
// list servers / resolve MCP prompts.
func (c *Controller) Host() *plugin.Host { return c.mcp.Host() }

// AddMCPServer connects an MCP server live and persists it to the user config.
func (c *Controller) AddMCPServer(e config.PluginEntry) (int, error) { return c.mcp.AddAndSave(e) }

// ConnectMCPServer connects an MCP server for this session only (no config write).
func (c *Controller) ConnectMCPServer(e config.PluginEntry) (int, error) { return c.mcp.Connect(e) }

// ConfiguredMCPNames lists the servers declared in config.
func (c *Controller) ConfiguredMCPNames() []string { return c.mcp.ConfiguredNames() }

// DisconnectedMCPNames lists configured servers not connected this session.
func (c *Controller) DisconnectedMCPNames() []string { return c.mcp.DisconnectedNames() }

// ConnectConfiguredMCPServer connects a configured-but-disconnected server by name.
func (c *Controller) ConnectConfiguredMCPServer(name string) (int, error) {
	return c.mcp.ConnectConfigured(name)
}

// RemoveMCPServer disconnects a server and removes it from config.
func (c *Controller) RemoveMCPServer(name string) (bool, error) { return c.mcp.RemoveAndSave(name) }

// DisconnectMCPServer disconnects a server for this session only.
func (c *Controller) DisconnectMCPServer(name string) bool { return c.mcp.Disconnect(name) }

// --- memory (forwarded to memoryService; see memory.go) ---

// QuickAdd appends a one-line note to the doc-memory file for scope — the write
// side of "#<note>". Returns the file written.
func (c *Controller) QuickAdd(scope memory.Scope, note string) (string, error) {
	return c.memory.QuickAdd(scope, note)
}

// SaveDoc overwrites a recognized memory doc with body. Returns the file written.
func (c *Controller) SaveDoc(path, body string) (string, error) { return c.memory.SaveDoc(path, body) }

// ForgetMemory deletes a saved auto-memory by name.
func (c *Controller) ForgetMemory(name string) error { return c.memory.Forget(name) }

// QueueMemory implements memory.Queue for callers that hold a *Controller.
func (c *Controller) QueueMemory(note string) { c.memory.QueueMemory(note) }

// Memory returns the loaded memory snapshot (nil when memory is disabled). The
// returned *Set is immutable — mutations go through QuickAdd / SaveDoc.
func (c *Controller) Memory() *memory.Set { return c.memory.Set() }

// --- session persistence (forwarded to sessionStore; see session_store.go) ---

// Resume seeds the session from a loaded transcript and pins the active file to
// its path so auto-save keeps appending there.
func (c *Controller) Resume(s *agent.Session, path string) {
	c.session.Adopt(s, path)
	c.ckpt.Rebind(path)
}

// SetSessionPath pins where auto-save lands (a fresh session file minted by the
// caller when no resume path applies).
func (c *Controller) SetSessionPath(p string) {
	c.session.SetPath(p)
	c.ckpt.Rebind(p)
}

// Snapshot writes the executor's conversation to the active session file.
func (c *Controller) Snapshot() error { return c.session.Save() }

// SnapshotActivity writes the conversation and marks the session recently active.
func (c *Controller) SnapshotActivity() error { return c.session.SaveActivity() }

// SessionDir reports the directory new session files land in ("" disables
// persistence), so the caller can decide whether to mint a path.
func (c *Controller) SessionDir() string { return c.session.Dir() }

// SessionPath reports the file the current conversation auto-saves to ("" when
// persistence is disabled), so a history view can mark the active session.
func (c *Controller) SessionPath() string { return c.session.Path() }

// History returns the executor's current message log (for repopulating a
// resumed frontend's view).
func (c *Controller) History() []provider.Message { return c.session.History() }

// --- turn arbitration (forwarded to turnState; see turn_state.go) ---

// Cancel stops the in-flight turn, if any.
func (c *Controller) Cancel() { c.turn.Cancel() }

// Running reports whether a turn is in flight.
func (c *Controller) Running() bool { return c.turn.Running() }

// SetPlanMode flips the executor's read-only gate without touching the
// cache-stable prompt prefix, and remembers the state so Compose can prepend the
// plan-mode marker to outgoing turns.
func (c *Controller) SetPlanMode(v bool) { c.turn.SetPlanMode(v) }

// PlanMode reports whether plan mode is on.
func (c *Controller) PlanMode() bool { return c.turn.PlanMode() }

// SetCoachMode sets the session's coaching persona ("" clears it).
func (c *Controller) SetCoachMode(preamble string) { c.turn.SetCoach(preamble) }
