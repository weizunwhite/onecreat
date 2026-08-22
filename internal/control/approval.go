package control

// approvalBroker owns every "stop and ask the human" interaction of a session:
// tool-approval prompts (the permission gate's Approver), the `ask` tool's
// questions, the session grants a "yes, for this session" answer leaves behind,
// and the two escape hatches that skip prompting entirely — YOLO/bypass mode and
// the just-approved-plan window.
//
// It is the first piece split out of control.Controller (Plan 07). All of this
// state used to sit on the Controller under the same mutex as the run state, the
// plan-mode flag and the memory snapshot — four unrelated domains behind one
// lock. Nothing ever needed them to be atomic together: every critical section
// here is short and touches only these maps.
//
// The one ordering rule that matters is preserved: promptMu serialises
// outstanding prompts and is held across the blocking wait, so the answer path
// (Resolve / AnswerQuestion) must never take it — otherwise answering would
// deadlock against the prompt it is answering.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"

	"reasonix/internal/event"
	"reasonix/internal/hook"
	"reasonix/internal/permission"
)

type approvalReply struct {
	allow   bool
	session bool
}

type approvalBroker struct {
	sink   event.Sink
	policy permission.Policy
	// hooks fires the Notification hook when a run blocks on the user; nil-safe.
	hooks *hook.Runner

	// promptMu serialises approval prompts so at most one is outstanding at a
	// time (parallel read-only tool calls don't normally gate, writers run
	// serially — but this keeps the contract explicit). Held across the blocking
	// wait, so it must never be taken by the answer path.
	promptMu sync.Mutex

	mu        sync.Mutex
	approvals map[string]chan approvalReply
	asks      map[string]chan []event.AskAnswer
	// pendingApprovals/pendingAsks 保存「已发出、尚未应答」的提示原始载荷,供切回标签时
	// 重放(桌面多标签:后台标签的审批事件在它无人订阅时发出,切回来需要补发)(A2)。
	pendingApprovals map[string]event.Approval
	pendingAsks      map[string]event.Ask
	granted          map[string]bool
	nextID           int
	// autoApprove auto-allows writer tool calls without prompting. Set only while
	// executing a just-approved plan: approving the plan is the go-ahead, so the
	// model shouldn't re-prompt for every write of the work it just got cleared to
	// do. Deny rules still bite (those never reach the approver). Reset when the
	// execution turn returns.
	autoApprove bool
	// bypass is "YOLO" mode: while set, every approval prompt is auto-allowed for
	// the rest of the session (writers and bash run without asking). It is a
	// deliberate, session-scoped opt-in (the --dangerously-skip-permissions flag or
	// a runtime toggle), never persisted. Deny rules are unaffected — they're
	// resolved before the approver, so a denied tool is still blocked in YOLO mode.
	bypass bool
}

func newApprovalBroker(sink event.Sink, policy permission.Policy, hooks *hook.Runner) *approvalBroker {
	return &approvalBroker{
		sink:             sink,
		policy:           policy,
		hooks:            hooks,
		approvals:        map[string]chan approvalReply{},
		asks:             map[string]chan []event.AskAnswer{},
		pendingApprovals: map[string]event.Approval{},
		pendingAsks:      map[string]event.Ask{},
		granted:          map[string]bool{},
	}
}

// Gate builds the permission gate that routes "ask" decisions through this
// broker. Interactive frontends install it on the executor; the headless run
// keeps the silent gate it was built with.
func (b *approvalBroker) Gate() *permission.Gate {
	return permission.NewGate(b.policy, b)
}

// Resolve answers a pending approval by id. Unknown/expired ids are ignored.
// It must not take promptMu — the prompt it answers is holding it.
func (b *approvalBroker) Resolve(id string, allow, session bool) {
	b.mu.Lock()
	reply := b.approvals[id]
	delete(b.approvals, id)
	delete(b.pendingApprovals, id)
	b.mu.Unlock()
	if reply != nil {
		reply <- approvalReply{allow: allow, session: session} // buffered, never blocks
	}
}

// PendingApprovals 返回当前已发出、尚未应答的审批请求(用于切回标签时重放),按 id 升序。
func (b *approvalBroker) PendingApprovals() []event.Approval {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]event.Approval, 0, len(b.pendingApprovals))
	for _, a := range b.pendingApprovals {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return idLess(out[i].ID, out[j].ID) })
	return out
}

// PendingAsks 返回当前已发出、尚未应答的 ask 请求(用于切回标签时重放),按 id 升序。
func (b *approvalBroker) PendingAsks() []event.Ask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]event.Ask, 0, len(b.pendingAsks))
	for _, a := range b.pendingAsks {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return idLess(out[i].ID, out[j].ID) })
	return out
}

// idLess 按数值比较审批/ask 的字符串 id(它们是自增计数的十进制串),回退到字典序。
func idLess(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}

// Ask implements agent.Asker: it emits an AskRequest and blocks until
// AnswerQuestion(ID, …) answers or ctx is cancelled. promptMu serialises it
// against tool-approval prompts so at most one user prompt is outstanding.
func (b *approvalBroker) Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error) {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()

	b.mu.Lock()
	b.nextID++
	id := strconv.Itoa(b.nextID)
	reply := make(chan []event.AskAnswer, 1)
	b.asks[id] = reply
	ask := event.Ask{ID: id, Questions: questions}
	b.pendingAsks[id] = ask // 记录未应答的 ask,供切回标签时重放(A2)
	b.mu.Unlock()

	b.sink.Emit(event.Event{Kind: event.AskRequest, Ask: ask})

	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.asks, id)
		delete(b.pendingAsks, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// AnswerQuestion resolves a pending AskRequest by ID with the user's selections.
// Unknown/expired IDs are ignored.
func (b *approvalBroker) AnswerQuestion(id string, answers []event.AskAnswer) {
	b.mu.Lock()
	reply := b.asks[id]
	delete(b.asks, id)
	delete(b.pendingAsks, id)
	b.mu.Unlock()
	if reply != nil {
		reply <- answers // buffered, never blocks
	}
}

// SetBypass turns YOLO/bypass mode on or off for the session.
func (b *approvalBroker) SetBypass(on bool) {
	b.mu.Lock()
	b.bypass = on
	b.mu.Unlock()
}

// Bypass reports whether YOLO/bypass mode is on, for the status-bar indicator
// and for the auto-plan heuristic (bypass means "don't stop to ask", so drafting
// a plan and gating on it is the opposite of what the user opted into).
func (b *approvalBroker) Bypass() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bypass
}

// SetAutoApprove opens or closes the just-approved-plan window.
func (b *approvalBroker) SetAutoApprove(on bool) {
	b.mu.Lock()
	b.autoApprove = on
	b.mu.Unlock()
}

// Approve implements permission.Approver — the direction the gate calls in. It is
// distinct from the command-side Resolve (different signature, different
// direction): the gate asks "may this run?", the frontend answers "yes/no to
// prompt N".
func (b *approvalBroker) Approve(ctx context.Context, tool, subject string, _ json.RawMessage) (bool, bool, error) {
	return b.Request(ctx, tool, subject)
}

// Request emits an ApprovalRequest and blocks until Resolve(ID, …) answers or ctx
// is cancelled. A prior session grant for the same tool+subject short-circuits.
// promptMu serialises outstanding prompts.
func (b *approvalBroker) Request(ctx context.Context, tool, subject string) (bool, bool, error) {
	key := tool + "\x00" + subject

	b.mu.Lock()
	// YOLO/bypass and the just-approved-plan window auto-allow every approval
	// without prompting; the plan gate routes through here too, so this is what
	// stops a bypass session from blocking on plan approval. Deny rules bit upstream.
	if b.bypass || b.autoApprove || b.granted[key] {
		b.mu.Unlock()
		return true, false, nil
	}
	b.mu.Unlock()

	b.promptMu.Lock()
	defer b.promptMu.Unlock()

	// Re-check the grant: a session grant may have landed while we queued behind
	// another prompt for the same subject.
	b.mu.Lock()
	if b.granted[key] {
		b.mu.Unlock()
		return true, false, nil
	}
	b.nextID++
	id := strconv.Itoa(b.nextID)
	reply := make(chan approvalReply, 1)
	b.approvals[id] = reply
	approval := event.Approval{ID: id, Tool: tool, Subject: subject}
	b.pendingApprovals[id] = approval // 记录未应答的审批,供切回标签时重放(A2)
	b.mu.Unlock()

	b.sink.Emit(event.Event{Kind: event.ApprovalRequest, Approval: approval})
	// The agent now needs the user's attention; a Notification hook can ping an
	// external channel (desktop notice, phone) while the run blocks on the reply.
	msg := "approval needed: " + tool
	if subject != "" {
		msg += " " + subject
	}
	go b.hooks.Notification(ctx, msg)

	select {
	case r := <-reply:
		// Plan approvals are one-shot — never persist a session grant for them, or
		// every future plan would auto-approve.
		if r.allow && r.session && tool != planApprovalTool {
			b.mu.Lock()
			b.granted[key] = true
			b.mu.Unlock()
		}
		// remember=false: session grants live here, not in the on-disk policy.
		return r.allow, false, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.approvals, id)
		delete(b.pendingApprovals, id)
		b.mu.Unlock()
		return false, false, ctx.Err()
	}
}
