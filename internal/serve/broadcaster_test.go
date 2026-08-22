package serve

import (
	"encoding/json"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/eventstream"
)

func nextWire(t *testing.T, s *eventstream.Sub) wireEvent {
	t.Helper()
	f, ok := s.Next()
	if !ok {
		t.Fatal("subscriber stopped before delivering a frame")
	}
	var w wireEvent
	if err := json.Unmarshal(f.Data, &w); err != nil {
		t.Fatalf("frame is not valid JSON: %v", err)
	}
	return w
}

func TestBroadcasterFanOut(t *testing.T) {
	b := NewBroadcaster()
	a := b.Subscribe()
	d := b.Subscribe()
	defer b.Unsubscribe(a)
	defer b.Unsubscribe(d)

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("subscribers = %d, want 2", got)
	}

	b.Emit(event.Event{Kind: event.Text, Text: "hi"})

	for i, sub := range []*eventstream.Sub{a, d} {
		w := nextWire(t, sub)
		if w.Kind != "text" || w.Text != "hi" {
			t.Errorf("subscriber %d got %+v", i, w)
		}
	}
}

func TestBroadcasterUnsubscribe(t *testing.T) {
	b := NewBroadcaster()
	sub := b.Subscribe()
	if b.Subscribers() != 1 {
		t.Fatalf("want 1 subscriber")
	}
	b.Unsubscribe(sub)
	if b.Subscribers() != 0 {
		t.Fatalf("unsubscribe should drop to 0, got %d", b.Subscribers())
	}
	// Emitting with no subscribers must not panic.
	b.Emit(event.Event{Kind: event.TurnDone})
}

// TestBroadcasterKeepsStateForASlowSubscriber is the transport-level statement
// of Plan 10's acceptance: flood a subscriber that never reads with far more
// text deltas than its buffer holds, then emit the two events a UI cannot do
// without. The deltas may be dropped; the approval and the turn_done may not.
//
// Before Plan 10 this test could not pass: the 64-slot channel dropped whatever
// arrived while it was full, so the approval was as likely to vanish as a delta
// — and the agent would then block forever on a prompt nobody ever saw.
func TestBroadcasterKeepsStateForASlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	for i := 0; i < 5000; i++ { // far past the ephemeral limit; Emit must not block
		b.Emit(event.Event{Kind: event.Text, Text: "x"})
	}
	b.Emit(event.Event{Kind: event.ApprovalRequest, Approval: event.Approval{ID: "7", Tool: "bash"}})
	b.Emit(event.Event{Kind: event.TurnDone})

	if sub.Overflowed() {
		t.Fatal("an ephemeral flood must not fail the subscription")
	}
	var sawApproval, sawTurnDone bool
	for {
		f, ok := sub.TryNext()
		if !ok {
			break
		}
		var w wireEvent
		if err := json.Unmarshal(f.Data, &w); err != nil {
			t.Fatalf("bad frame: %v", err)
		}
		switch w.Kind {
		case "approval_request":
			sawApproval = true
		case "turn_done":
			sawTurnDone = true
		}
	}
	if !sawApproval {
		t.Error("approval_request was dropped for a slow subscriber — the agent would block forever")
	}
	if !sawTurnDone {
		t.Error("turn_done was dropped for a slow subscriber — the UI would spin forever")
	}
	if sub.DroppedEphemeral() == 0 {
		t.Error("expected text deltas to be dropped under pressure")
	}
}

// TestBroadcasterStampsTheEnvelope: every frame is identifiable and the sequence
// is gap-free, which is what lets a client notice loss at all.
func TestBroadcasterStampsTheEnvelope(t *testing.T) {
	b := NewBroadcasterFor("session-1")
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.Emit(event.Event{Kind: event.Text, Text: "a"})
	b.Emit(event.Event{Kind: event.TurnDone})

	first := nextWire(t, sub)
	second := nextWire(t, sub)
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequence = %d, %d; want 1, 2", first.Sequence, second.Sequence)
	}
	if first.SchemaVersion == 0 || first.EventID == "" || first.Timestamp == "" {
		t.Fatalf("envelope incomplete: %+v", first)
	}
	if first.SessionID != "session-1" {
		t.Errorf("sessionId = %q, want session-1", first.SessionID)
	}
	if first.Durable {
		t.Error("a text delta should be marked ephemeral on the wire")
	}
	if !second.Durable {
		t.Error("turn_done should be marked durable on the wire")
	}
}
