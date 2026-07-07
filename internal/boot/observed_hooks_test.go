package boot

import (
	"context"
	"encoding/json"
	"testing"

	"reasonix/internal/agent"
)

// stubHooks records what the inner ToolHooks saw, and returns canned results, so
// the wrapper's delegation can be asserted.
type stubHooks struct {
	preCalled  bool
	preBlock   bool
	preMsg     string
	postCalled bool
	hasPostLLM bool
	llmOut     string
	preCompact string
	subStop    bool
}

func (s *stubHooks) PreToolUse(ctx context.Context, name string, args json.RawMessage) (bool, string) {
	s.preCalled = true
	return s.preBlock, s.preMsg
}
func (s *stubHooks) PostToolUse(ctx context.Context, name string, args json.RawMessage, result string) {
	s.postCalled = true
}
func (s *stubHooks) PostLLMCall(ctx context.Context, reasoning string, turn int) string {
	return s.llmOut
}
func (s *stubHooks) HasPostLLMCall() bool                          { return s.hasPostLLM }
func (s *stubHooks) SubagentStop(ctx context.Context, last string) { s.subStop = true }
func (s *stubHooks) PreCompact(ctx context.Context, trigger string) string {
	return s.preCompact
}

var _ agent.ToolHooks = (*stubHooks)(nil)

// The observer must run before the tool executes — i.e. before the inner
// PreToolUse — and the wrapper must surface the inner runner's verdict unchanged.
func TestObservedHooksPreToolUseFiresObserverThenDelegates(t *testing.T) {
	inner := &stubHooks{preBlock: true, preMsg: "denied by hook"}
	var order []string
	observe := func(ctx context.Context, name string, args json.RawMessage) {
		if inner.preCalled {
			t.Fatal("observer ran after inner PreToolUse; it must run first (before the tool)")
		}
		order = append(order, "observe:"+name)
	}
	w := observedHooks{observe: observe, inner: inner}

	block, msg := w.PreToolUse(context.Background(), "arduino_upload", json.RawMessage(`{}`))

	if len(order) != 1 || order[0] != "observe:arduino_upload" {
		t.Fatalf("observer not invoked with tool name; got %v", order)
	}
	if !inner.preCalled {
		t.Fatal("inner PreToolUse was not delegated to")
	}
	if !block || msg != "denied by hook" {
		t.Fatalf("wrapper must return inner's verdict; got block=%v msg=%q", block, msg)
	}
}

// Every non-PreToolUse method is pure delegation — the observer never interferes.
func TestObservedHooksDelegatesEverythingElse(t *testing.T) {
	inner := &stubHooks{hasPostLLM: true, llmOut: "translated", preCompact: "keep-plan"}
	observed := false
	w := observedHooks{
		observe: func(context.Context, string, json.RawMessage) { observed = true },
		inner:   inner,
	}

	if !w.HasPostLLMCall() {
		t.Error("HasPostLLMCall must delegate to inner (streaming decision hinges on it)")
	}
	if got := w.PostLLMCall(context.Background(), "raw", 1); got != "translated" {
		t.Errorf("PostLLMCall = %q, want inner's %q", got, "translated")
	}
	if got := w.PreCompact(context.Background(), "auto"); got != "keep-plan" {
		t.Errorf("PreCompact = %q, want inner's %q", got, "keep-plan")
	}
	w.PostToolUse(context.Background(), "bash", nil, "ok")
	w.SubagentStop(context.Background(), "done")
	if !inner.postCalled || !inner.subStop {
		t.Error("PostToolUse/SubagentStop must delegate to inner")
	}
	if observed {
		t.Error("observer must only fire on PreToolUse, not on the delegated methods")
	}
}
