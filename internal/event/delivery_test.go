package event

import "testing"

// TestOnlyRenderDeltasAreEphemeral pins the classification. The three ephemeral
// kinds share one property: a later event makes the missed one irrelevant (the
// next delta, or the Message / ToolResult that supersedes the stream). Anything
// else is state a consumer cannot re-derive by waiting.
func TestOnlyRenderDeltasAreEphemeral(t *testing.T) {
	for _, k := range []Kind{Reasoning, Text, ToolProgress} {
		if k.Durable() {
			t.Errorf("kind %d should be ephemeral — it is superseded by a later event", k)
		}
	}
	// The ones the roadmap names explicitly, plus the turn boundaries a UI keys on.
	for _, k := range []Kind{
		ApprovalRequest, AskRequest, Message, ToolResult, TurnDone,
		TurnStarted, ToolDispatch, Usage, Notice, Phase,
		CompactionStarted, CompactionDone, MCPSurfaceReady,
	} {
		if !k.Durable() {
			t.Errorf("kind %d must be durable — dropping it loses state the client cannot re-derive", k)
		}
	}
}

// TestUnclassifiedKindsFailSafe: a kind nobody thought about must default to
// durable. Getting this backwards would make a *new* state-bearing event
// silently droppable, which is exactly the bug class Plan 10 closes.
func TestUnclassifiedKindsFailSafe(t *testing.T) {
	const madeUp = Kind(9999)
	if !madeUp.Durable() {
		t.Fatal("an unclassified kind must default to durable")
	}
}
