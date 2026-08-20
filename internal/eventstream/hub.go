// Package eventstream is the delivery layer between an agent's event stream and
// a network client that may not keep up.
//
// The problem it solves (A11): both SSE transports used to fan out into a
// fixed-size channel per subscriber and, when that channel was full, drop the
// frame — *any* frame. A browser that stalled for a second lost text deltas,
// which is fine, but it could equally lose an ApprovalRequest, leaving the agent
// blocked on a prompt the user will never see, or a TurnDone, leaving the UI
// spinning forever. The loss was also silent: nothing on either end could tell
// afterwards that it had happened.
//
// The fix is to stop treating all frames alike. Each frame carries the QoS its
// event kind declares (see event.Delivery):
//
//   - **Ephemeral** frames are dropped when a subscriber is behind. That is the
//     old behaviour, and it is correct for render deltas — the next one
//     supersedes them.
//   - **Durable** frames are never dropped. They queue without bound up to a
//     hard cap; a subscriber that blows through the cap is *disconnected*, which
//     is a loud failure the client recovers from by reconnecting and re-syncing.
//     Silently continuing without them is the one thing this package will not do.
//
// Publish never blocks. That constraint is not negotiable: it runs on the
// agent's run-loop goroutine, and a stalled browser must not stall the agent.
package eventstream

import (
	"sync"
)

// Frame is one encoded event ready to write, plus the QoS the delivery layer
// needs. Data is already-marshalled JSON — the hub never inspects it.
type Frame struct {
	Data    []byte
	Durable bool
}

// Limits bound a single subscriber's backlog.
type Limits struct {
	// Ephemeral is how many frames may be queued before ephemeral frames start
	// being dropped. A generous value keeps a normally-lagging client lossless;
	// only a truly stuck one starts losing deltas.
	Ephemeral int
	// Durable is the hard cap on queued durable frames. Reaching it means the
	// client is not consuming at all; the subscription is failed rather than
	// quietly trimmed.
	Durable int
}

// DefaultLimits are sized for one long answer's worth of token deltas, and for
// far more durable frames than a live session can produce before a client that
// is merely slow catches up.
var DefaultLimits = Limits{Ephemeral: 512, Durable: 4096}

// Hub fans frames out to every subscriber.
type Hub struct {
	limits Limits

	mu   sync.Mutex
	subs map[*Sub]struct{}
}

// New returns an empty hub. Zero or negative limits fall back to DefaultLimits.
func New(l Limits) *Hub {
	if l.Ephemeral <= 0 {
		l.Ephemeral = DefaultLimits.Ephemeral
	}
	if l.Durable <= 0 {
		l.Durable = DefaultLimits.Durable
	}
	return &Hub{limits: l, subs: map[*Sub]struct{}{}}
}

// Publish delivers a frame to every subscriber. It never blocks.
func (h *Hub) Publish(f Frame) {
	h.mu.Lock()
	subs := make([]*Sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.push(f)
	}
}

// Subscribe registers a client. The returned Sub must be closed when the client
// goes away (the handler defers it).
func (h *Hub) Subscribe() *Sub {
	s := &Sub{limits: h.limits, wake: make(chan struct{}, 1), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Unsubscribe removes a subscriber and closes it.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	s.Close()
}

// Subscribers reports the current connection count (diagnostics and tests).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Sub is one client's backlog.
type Sub struct {
	limits Limits

	mu       sync.Mutex
	q        []Frame
	durables int
	dropped  int // ephemeral frames dropped, for diagnostics
	overflow bool
	closed   bool

	wake chan struct{}
	done chan struct{}
}

// push enqueues a frame under this subscriber's policy. Never blocks.
func (s *Sub) push(f Frame) {
	s.mu.Lock()
	switch {
	case s.closed || s.overflow:
		// Already failed or gone: nothing more is owed to this client.
	case f.Durable:
		if s.durables >= s.limits.Durable {
			// The client is not consuming. Fail the subscription instead of
			// dropping state it can never recover: the handler sees Overflowed
			// and disconnects, and the client re-syncs on reconnect.
			s.overflow = true
			break
		}
		s.q = append(s.q, f)
		s.durables++
	case len(s.q) >= s.limits.Ephemeral:
		s.dropped++ // a render delta; the next one supersedes it
	default:
		s.q = append(s.q, f)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *Sub) signal() {
	select {
	case s.wake <- struct{}{}:
	default: // a wake-up is already pending; one is enough
	}
}

// Next returns the next frame, blocking until one arrives, the subscriber
// overflows, or it is closed. ok is false when the caller should stop: check
// Overflowed to tell a failure from an ordinary disconnect.
func (s *Sub) Next() (Frame, bool) {
	for {
		s.mu.Lock()
		if len(s.q) > 0 {
			f := s.q[0]
			s.q = s.q[1:]
			if f.Durable {
				s.durables--
			}
			s.mu.Unlock()
			return f, true
		}
		failed := s.overflow || s.closed
		s.mu.Unlock()
		if failed {
			return Frame{}, false
		}
		select {
		case <-s.wake:
		case <-s.done:
		}
	}
}

// Wake returns the channel that signals new frames, for a caller that must also
// select on its own context (an HTTP handler with a heartbeat ticker).
func (s *Sub) Wake() <-chan struct{} { return s.wake }

// TryNext pops a frame if one is queued, without blocking.
func (s *Sub) TryNext() (Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.q) == 0 {
		return Frame{}, false
	}
	f := s.q[0]
	s.q = s.q[1:]
	if f.Durable {
		s.durables--
	}
	return f, true
}

// Overflowed reports that durable frames could not be queued, so this
// subscription must be torn down rather than continued.
func (s *Sub) Overflowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflow
}

// DroppedEphemeral counts render deltas dropped because the client was behind.
// Only ever ephemeral frames — durable ones overflow instead.
func (s *Sub) DroppedEphemeral() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close releases the subscriber. Idempotent.
func (s *Sub) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.done)
	s.signal()
}
