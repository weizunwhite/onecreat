// Package toolpolicy is OneCreat's product policy around a tool call: what may
// run, what must be recorded, and what the tool is allowed to reach.
//
// It exists because those decisions are *the product*, not the engine. A model
// loop is generic — stream, get tool calls, execute, feed results back, repeat.
// Everything OneCreat adds on top of that loop is here:
//
//   - plan mode refuses writers while the agent is still researching;
//   - the permission gate asks the user before a writer or a shell command runs;
//   - PreToolUse hooks let a project veto a call, PostToolUse observe its result;
//   - the checkpoint seam snapshots a file's pre-edit content so a turn can be
//     rewound;
//   - the evidence ledger records what actually happened, so `complete_step`
//     cannot claim a step is done without a receipt;
//   - the background-job manager and the memory queue are handed to the tools
//     that need them, through the call's context.
//
// Before Plan 08 all of this was inlined in agent.Agent.executeOne — eight
// fields and ~110 lines of policy wrapped around one `t.Execute(...)`. That made
// the loop and the product inseparable: a second engine (dsh) could not reuse
// any of it without reusing agent.Agent itself.
//
// The split is the Plan's acceptance criterion, stated as a package: a Pipeline
// knows nothing about how the calls were produced. Any engine that can say
// "I am about to run this tool call" and "here is what it returned" gets
// OneCreat's behaviour — approvals, evidence, checkpoints, hooks — unchanged.
//
// Ordering is part of the contract, not an implementation detail. Before runs
// its stages in exactly this order, and each one may stop the call:
//
//	unknown-tool → plan mode → permission gate → PreToolUse hook → checkpoint
//	snapshot → context injection
//
// The checkpoint snapshot deliberately comes *after* all gating: it records what
// a call that is cleared to run is about to change, so a blocked call never
// leaves a spurious rewind point.
package toolpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"reasonix/internal/diff"
	"reasonix/internal/evidence"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
)

// Gate is the per-call permission check. It answers "may this run?", possibly by
// prompting the user, and returns a short reason the agent feeds back to the
// model when it refuses.
type Gate interface {
	Check(ctx context.Context, toolName string, args json.RawMessage, readOnly bool) (allow bool, reason string, err error)
}

// Hooks is the tool-call half of the user's configured shell hooks. It is
// deliberately narrower than the full hook surface (which also brackets model
// turns and compaction): this package only needs what wraps a call.
type Hooks interface {
	PreToolUse(ctx context.Context, name string, args json.RawMessage) (block bool, message string)
	PostToolUse(ctx context.Context, name string, args json.RawMessage, result string)
	SubagentStop(ctx context.Context, result string)
}

// Call is one tool invocation as the engine sees it, plus the two facts policy
// needs about the tool itself.
type Call struct {
	ID   string
	Name string
	Args json.RawMessage
	// ReadOnly classifies the tool. Writers are what plan mode refuses, what the
	// gate prompts for, and what the checkpoint seam snapshots.
	ReadOnly bool
	// Preview, when non-nil, describes the change a writer is about to make. It
	// is nil for tools that cannot say (bash, whose targets are unknowable), and
	// those are simply never checkpointed.
	Preview func(json.RawMessage) (diff.Change, error)
}

// Block is a refused call, in the words the model will see. Output is fed back
// as the tool result; Reason is the short form for the turn's error bookkeeping.
type Block struct {
	Output string
	Reason string
}

// Pipeline carries the policy for one session. Every field is optional: a nil
// Gate disables gating, a nil Ledger disables evidence, and so on — which is how
// a headless run, a sub-agent, or a test gets a bare engine without ceremony.
type Pipeline struct {
	// Gate is the permission check. nil disables gating entirely.
	Gate Gate
	// Hooks fires the user's PreToolUse / PostToolUse / SubagentStop shell hooks.
	// nil disables hook firing.
	Hooks Hooks
	// PreEdit receives a writer's previewed change just before it runs — the seam
	// the checkpoint store uses to snapshot pre-edit content. nil disables it.
	PreEdit func(diff.Change)
	// Evidence is the per-turn ledger of host-observed tool receipts, which is
	// what lets complete_step verify a claim against what actually happened.
	Evidence *evidence.Ledger
	// Jobs is the session's background-job manager, reached by the background
	// tools through the call context. nil leaves those tools to degrade.
	Jobs *jobs.Manager
	// Memory lets the remember/forget tools queue a turn-tail note, so a memory
	// change applies this session without touching the cache-stable prompt prefix.
	Memory memory.Queue

	// planMode, when set, refuses any tool call whose ReadOnly() is false. The
	// system prompt and tool list never change with the toggle, so the
	// prompt-cache prefix stays valid; the gating happens at execute time and the
	// model sees a "blocked" result it can adapt to.
	planMode atomic.Bool
}

// New builds a pipeline with its own per-turn evidence ledger. The ledger is
// created here rather than handed in because it has no meaning outside this
// policy: it is what makes complete_step verifiable.
func New(gate Gate, hooks Hooks, bg *jobs.Manager) *Pipeline {
	return &Pipeline{Gate: gate, Hooks: hooks, Jobs: bg, Evidence: evidence.NewLedger()}
}

// ResetTurn clears the per-user-turn evidence ledger. The engine calls it when a
// new user turn begins, so a claim can only cite receipts from this turn.
func (p *Pipeline) ResetTurn() {
	if p == nil || p.Evidence == nil {
		return
	}
	p.Evidence.Reset()
}

// EndOfTurnReminder decides whether the run may end, given the todo list's state.
// It returns (reminder, true) when this turn's latest successful todo_write still
// has pending/in_progress items and no reminder was sent yet; the engine injects
// the reminder as a user message and runs one more round.
//
// This is product honesty, not loop mechanics — which is why it lives here and
// not in the engine. Scoped to this turn's ledger on purpose: a turn that never
// touched the todo list made no progress claim, so ending it with an old list on
// screen is the user's call to follow up on, not the harness's to nag about.
func (p *Pipeline) EndOfTurnReminder(alreadyReminded bool) (string, bool) {
	if p == nil || alreadyReminded || p.Evidence == nil {
		return "", false
	}
	todos, ok := p.Evidence.LatestTodos()
	if !ok {
		return "", false
	}
	var unfinished []string
	for i, t := range todos {
		if t.Status == "completed" {
			continue
		}
		unfinished = append(unfinished, fmt.Sprintf("  %d. [%s] %s", i+1, nonEmptyStatus(t.Status), t.Content))
	}
	if len(unfinished) == 0 {
		return "", false
	}
	return fmt.Sprintf(`[系统对账提醒] 你准备结束回合,但 todo 清单还有 %d 项未标记完成:
%s

逐项核对实际进度,然后再收尾:
- 已经真正做完的项:先用 complete_step 签收(引用本轮真实运行过的验证命令或改动过的文件作为证据),再用 todo_write 把该项标为 completed。
- 还没做的项:现在继续完成它;如果本轮确实无法完成或已无必要,保持 pending,并在最终回复里向用户说明原因。
不要在没有证据的情况下批量标记 completed。对完账后给出最终回复。`,
		len(unfinished), strings.Join(unfinished, "\n")), true
}

// nonEmptyStatus maps the schema's "empty means pending" onto a display word.
func nonEmptyStatus(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

// SetPlanMode toggles the read-only research mode.
func (p *Pipeline) SetPlanMode(v bool) {
	if p == nil {
		return
	}
	p.planMode.Store(v)
}

// PlanMode reports whether plan mode is on.
func (p *Pipeline) PlanMode() bool { return p != nil && p.planMode.Load() }

// Before runs every policy stage that precedes the call. It returns the context
// the tool should execute under (enriched with the ledger, the job manager and
// the memory queue), or a non-nil Block if the call must not run.
func (p *Pipeline) Before(ctx context.Context, c Call) (context.Context, *Block) {
	if p == nil {
		return ctx, nil
	}
	if p.planMode.Load() && !c.ReadOnly {
		return ctx, &Block{
			Output: fmt.Sprintf("blocked: %q is a writer tool and plan mode is read-only. Keep exploring with read-only tools, then write your plan as your reply — the user will be asked to approve it before any changes are made.", c.Name),
			Reason: "blocked: plan mode is read-only",
		}
	}
	if p.Gate != nil {
		allow, reason, err := p.Gate.Check(ctx, c.Name, c.Args, c.ReadOnly)
		if err != nil {
			return ctx, &Block{
				Output: fmt.Sprintf("blocked: %s (%v)", reason, err),
				Reason: fmt.Sprintf("blocked: %v", err),
			}
		}
		if !allow {
			return ctx, &Block{Output: "blocked: " + reason, Reason: "blocked by permission policy"}
		}
	}
	// PreToolUse hooks run after permission is granted but before the call: a
	// gating hook (exit 2) refuses it, surfaced to the model like a gate denial.
	if p.Hooks != nil {
		if block, msg := p.Hooks.PreToolUse(ctx, c.Name, c.Args); block {
			if msg == "" {
				msg = "blocked by a PreToolUse hook"
			}
			return ctx, &Block{Output: "blocked: " + msg, Reason: "blocked by PreToolUse hook"}
		}
	}
	// Checkpoint the file this writer is about to change, so the turn can be
	// rewound. Fires after all gating (the edit is cleared to run) and only for
	// tools that can describe their change; a Preview error means the edit will
	// likely fail anyway, so we skip rather than snapshot a stale state.
	if p.PreEdit != nil && !c.ReadOnly && c.Preview != nil {
		if change, err := c.Preview(c.Args); err == nil {
			p.PreEdit(change)
		}
	}
	if p.Evidence != nil {
		ctx = evidence.WithLedger(ctx, p.Evidence)
	}
	if p.Jobs != nil {
		ctx = jobs.WithManager(ctx, p.Jobs)
	}
	if p.Memory != nil {
		ctx = memory.WithQueue(ctx, p.Memory)
	}
	return ctx, nil
}

// After records what the call did and lets observers react. It runs whether the
// call succeeded or errored — the tool did run either way.
func (p *Pipeline) After(ctx context.Context, c Call, result string, err error) {
	if p == nil {
		return
	}
	if p.Evidence != nil {
		// complete_step is the claim, not the evidence: only record it when it
		// succeeded, so a rejected claim never becomes its own receipt.
		if c.Name != "complete_step" || err == nil {
			p.Evidence.Record(evidence.ReceiptFromToolCall(c.Name, c.Args, err == nil, c.ReadOnly))
		}
	}
	// PostToolUse hooks observe the result (they can't block).
	if p.Hooks != nil {
		p.Hooks.PostToolUse(ctx, c.Name, c.Args, result)
	}
}

// SubagentStopped fires the SubagentStop hook when a foreground `task` sub-agent
// finishes. (A backgrounded one returns a "Started…" string and stops later in a
// job, so it never reaches here.)
func (p *Pipeline) SubagentStopped(ctx context.Context, result string) {
	if p == nil || p.Hooks == nil {
		return
	}
	p.Hooks.SubagentStop(ctx, result)
}
