package eventstream

import (
	"sync"
	"testing"
)

func durable(n byte) Frame   { return Frame{Data: []byte{n}, Durable: true} }
func ephemeral(n byte) Frame { return Frame{Data: []byte{n}} }

// drain pops everything currently queued.
func drain(s *Sub) []Frame {
	var out []Frame
	for {
		f, ok := s.TryNext()
		if !ok {
			return out
		}
		out = append(out, f)
	}
}

// TestSlowClientLosesDeltasButNeverState is Plan 10's acceptance, stated as a
// test: flood a subscriber past its ephemeral limit and confirm the render
// deltas are what get dropped — while every durable frame interleaved with them
// still arrives, in order.
//
// Before this package, both transports dropped whatever happened to arrive when
// the buffer was full, so an ApprovalRequest could vanish and the agent would
// block forever on a prompt nobody saw.
func TestSlowClientLosesDeltasButNeverState(t *testing.T) {
	h := New(Limits{Ephemeral: 4, Durable: 64})
	s := h.Subscribe()
	defer h.Unsubscribe(s)

	// A burst far past the ephemeral limit, with state events sprinkled through.
	const burst = 100
	var wantDurable []byte
	for i := 0; i < burst; i++ {
		h.Publish(ephemeral(byte(i)))
		if i%10 == 0 {
			h.Publish(durable(byte(i)))
			wantDurable = append(wantDurable, byte(i))
		}
	}

	got := drain(s)
	var gotDurable []byte
	for _, f := range got {
		if f.Durable {
			gotDurable = append(gotDurable, f.Data[0])
		}
	}
	if string(gotDurable) != string(wantDurable) {
		t.Fatalf("durable frames lost or reordered: got %v, want %v", gotDurable, wantDurable)
	}
	if s.DroppedEphemeral() == 0 {
		t.Fatal("the test did not actually push the subscriber past its limit")
	}
	if s.Overflowed() {
		t.Fatal("an ephemeral flood must not fail the subscription")
	}
}

// TestDurableOverflowFailsLoudly: a client that consumes nothing at all cannot
// be served. The one unacceptable outcome is continuing to stream as if nothing
// were wrong, so the subscription is marked failed and the handler disconnects.
func TestDurableOverflowFailsLoudly(t *testing.T) {
	h := New(Limits{Ephemeral: 4, Durable: 3})
	s := h.Subscribe()
	defer h.Unsubscribe(s)

	for i := 0; i < 10; i++ {
		h.Publish(durable(byte(i)))
	}
	if !s.Overflowed() {
		t.Fatal("exceeding the durable cap must fail the subscription, not trim it")
	}
	// Next stops rather than pretending the stream is healthy.
	for {
		if _, ok := s.Next(); !ok {
			break
		}
	}
}

// TestPublishNeverBlocks: Publish runs on the agent's run-loop goroutine. A
// subscriber that never reads must not stall the agent.
func TestPublishNeverBlocks(t *testing.T) {
	h := New(Limits{Ephemeral: 1, Durable: 1})
	s := h.Subscribe()
	defer h.Unsubscribe(s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10_000; i++ {
			h.Publish(ephemeral(1))
			h.Publish(durable(1))
		}
	}()
	<-done // a blocking Publish would hang the test rather than fail it
}

// TestOrderIsPreserved: frames arrive in publish order.
func TestOrderIsPreserved(t *testing.T) {
	h := New(DefaultLimits)
	s := h.Subscribe()
	defer h.Unsubscribe(s)

	for i := 0; i < 20; i++ {
		h.Publish(Frame{Data: []byte{byte(i)}, Durable: i%2 == 0})
	}
	for i, f := range drain(s) {
		if f.Data[0] != byte(i) {
			t.Fatalf("frame %d = %d, want %d — order was not preserved", i, f.Data[0], i)
		}
	}
}

// TestCloseUnblocksReader: an ordinary disconnect stops Next without marking the
// subscription failed, so a handler can tell the two apart.
func TestCloseUnblocksReader(t *testing.T) {
	h := New(DefaultLimits)
	s := h.Subscribe()

	done := make(chan bool, 1)
	go func() {
		_, ok := s.Next()
		done <- ok
	}()
	h.Unsubscribe(s)
	if ok := <-done; ok {
		t.Fatal("Next should report stop after Close")
	}
	if s.Overflowed() {
		t.Error("a clean close is not an overflow")
	}
	h.Unsubscribe(s) // idempotent
	s.Close()
}

// TestFanOutIsIndependent: one stuck subscriber must not cost another its state.
func TestFanOutIsIndependent(t *testing.T) {
	h := New(Limits{Ephemeral: 2, Durable: 8})
	stuck := h.Subscribe()
	healthy := h.Subscribe()
	defer h.Unsubscribe(stuck)
	defer h.Unsubscribe(healthy)

	for i := 0; i < 50; i++ {
		h.Publish(ephemeral(byte(i)))
		drain(healthy) // the healthy client keeps up
	}
	h.Publish(durable(7))

	got := drain(healthy)
	if len(got) != 1 || !got[0].Durable || got[0].Data[0] != 7 {
		t.Fatalf("healthy subscriber lost the durable frame: %+v", got)
	}
	if stuck.DroppedEphemeral() == 0 {
		t.Error("the stuck subscriber should have dropped deltas")
	}
}

// TestConcurrentPublishAndConsume exercises the lock discipline under -race:
// several publishers on one hub while several readers drain their own backlogs.
func TestConcurrentPublishAndConsume(t *testing.T) {
	h := New(DefaultLimits)

	var readers sync.WaitGroup
	subs := make([]*Sub, 4)
	for i := range subs {
		s := h.Subscribe()
		subs[i] = s
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				if _, ok := s.Next(); !ok {
					return
				}
			}
		}()
	}

	var publishers sync.WaitGroup
	for i := 0; i < 8; i++ {
		publishers.Add(1)
		go func(n int) {
			defer publishers.Done()
			for j := 0; j < 200; j++ {
				h.Publish(Frame{Data: []byte{byte(n)}, Durable: j%3 == 0})
			}
		}(i)
	}
	publishers.Wait()

	for _, s := range subs {
		h.Unsubscribe(s)
	}
	readers.Wait()
	if h.Subscribers() != 0 {
		t.Fatalf("subscribers left registered: %d", h.Subscribers())
	}
}
