package control

// turnState arbitrates who may touch the conversation right now, and carries the
// per-turn runtime flags that ride each outgoing message.
//
// Split out of control.Controller in Plan 07. Two things live here because they
// are the same decision seen from two sides:
//
//   - **Mutual exclusion.** `running` marks a model turn in flight; `busy` marks
//     an exclusive log-rewriting operation (compact / summarize / new / rewind /
//     fork / branch / switch). Either one blocks the other, in both directions.
//     The reverse direction is the subtle half: without it, an op in flight
//     (especially summarize's multi-second network call) could race a turn that
//     starts through Guarded and appends via Session.Add, and then the op's
//     Replace/SetSession — built from the pre-turn snapshot — would silently
//     swallow the whole turn (B2).
//   - **Per-turn runtime flags.** Plan mode and the coaching persona are injected
//     at Compose time and are *never* part of the cached system prefix — that is
//     a DeepSeek prefix-cache rule, not a style choice. Keeping them next to the
//     run state makes it obvious they are turn-scoped, not session-prompt-scoped.

import (
	"context"
	"strings"
	"sync"

	"reasonix/internal/agent"
	"reasonix/internal/event"
)

type turnState struct {
	sink event.Sink
	exec *agent.Agent

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	busy    bool
	// turn counts model turns this session, passed to hooks in their payload.
	turn          int
	planMode      bool
	coachPreamble string
}

func newTurnState(sink event.Sink, exec *agent.Agent) *turnState {
	return &turnState{sink: sink, exec: exec}
}

// NextTurn bumps and returns this session's model-turn counter, for hook payloads.
func (t *turnState) NextTurn() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turn++
	return t.turn
}

// Coach returns the session's coaching persona ("" = none).
func (t *turnState) Coach() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.coachPreamble
}

// runGuarded runs body on a background goroutine under a fresh cancellable
// context, guarding against concurrent turns and emitting a TurnDone event when
// it finishes (Err set on failure; nil also for a user Cancel). A no-op if a
// turn is already in flight.
func (t *turnState) Guarded(body func(ctx context.Context) error) {
	t.mu.Lock()
	if t.running || t.busy {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.running = true
	t.mu.Unlock()

	go func() {
		defer cancel()
		err := body(ctx)
		t.mu.Lock()
		t.running = false
		t.cancel = nil
		t.mu.Unlock()
		t.sink.Emit(event.Event{Kind: event.TurnDone, Err: err})
	}()
}

// tryBeginExclusive 原子地尝试进入「独占重写会话日志」临界区:若已有 turn 运行(running)
// 或已有另一个独占 op(busy),返回 false;否则置 busy 返回 true。成功的调用方必须
// defer endExclusive()。配合 runGuarded 同时检查 running||busy,使 turn 与 compact/
// summarize/new/rewind/fork/branch/switch 严格互斥(见 busy 字段说明)。
func (t *turnState) TryBeginExclusive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running || t.busy {
		return false
	}
	t.busy = true
	return true
}

func (t *turnState) EndExclusive() {
	t.mu.Lock()
	t.busy = false
	t.mu.Unlock()
}

// Cancel aborts the in-flight turn. A goroutine blocked awaiting approval
// unblocks via the cancelled context.
func (t *turnState) Cancel() {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Running reports whether a turn is currently in flight.
func (t *turnState) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// SetPlanMode flips the executor's read-only gate without touching the
// cache-stable prompt prefix, and remembers the state so Compose can prepend the
// plan-mode marker to outgoing turns.
func (t *turnState) SetPlanMode(v bool) {
	t.mu.Lock()
	t.planMode = v
	t.mu.Unlock()
	if t.exec != nil {
		t.exec.SetPlanMode(v)
	}
}

// PlanMode reports whether outgoing turns currently receive the plan-mode
// marker. Frontends use it after Compose because auto-plan may flip the mode.
func (t *turnState) PlanMode() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.planMode
}

// SetCoachMode sets the session-level coaching persona (an empty string clears
// it). Compose injects the preamble as a <coaching-style> block on each turn —
// session-scoped, never the cached system prefix — so switching takes effect
// immediately without busting the prompt cache.
func (t *turnState) SetCoach(preamble string) {
	t.mu.Lock()
	t.coachPreamble = strings.TrimSpace(preamble)
	t.mu.Unlock()
}
