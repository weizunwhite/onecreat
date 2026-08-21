package eventwire

import (
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"reasonix/internal/event"
)

// Stamper turns domain events into wire frames for one stream, adding the
// envelope that makes the stream *auditable*: a monotonic, gap-free sequence
// number, a unique event id, a timestamp, and the identity of the session (and
// desktop tab) the frame belongs to.
//
// The sequence is what a client needs to notice loss at all. A transport is
// allowed to drop ephemeral frames under pressure (see event.Delivery), and
// without a sequence the client cannot distinguish "nothing happened" from "I
// missed three deltas". With it, a gap is visible, and the client can decide
// whether it matters — a gap that could only have held ephemeral frames is
// cosmetic, while any other gap means re-sync from /history.
//
// One Stamper per stream. It is safe for concurrent use.
type Stamper struct {
	sessionID string
	tabID     string
	seq       atomic.Uint64
	// now is injectable so tests get deterministic timestamps.
	now func() time.Time
}

// NewStamper starts a stream. sessionID and tabID may be empty for transports
// that have only one of each (the HTTP server has no tabs).
func NewStamper(sessionID, tabID string) *Stamper {
	return &Stamper{sessionID: sessionID, tabID: tabID, now: time.Now}
}

// Wire encodes one event and stamps it with the next sequence number.
func (s *Stamper) Wire(e event.Event) Event {
	w := Encode(e)
	w.SchemaVersion = SchemaVersion
	w.Durable = e.Kind.Durable()
	n := s.seq.Add(1)
	w.Sequence = n
	w.EventID = s.eventID(n)
	w.Timestamp = s.now().UTC().Format(time.RFC3339Nano)
	w.SessionID = s.sessionID
	w.TabID = s.tabID
	return w
}

// Sequence 返回这条流已经发出的最后一个序号(尚未发出任何事件时为 0)。
//
// 权威快照要带上它:客户端拿到快照后,就知道"这份状态对应到第 N 号事件为止",于是
// 之后收到的第一条事件是不是接得上、中间有没有洞,一目了然(AR-R07)。
func (s *Stamper) Sequence() uint64 { return s.seq.Load() }

// StreamID 返回这条流的身份(sessionID[:tabID]),让客户端能认出"这已经是另一条流了,
// 手里的序号别再拿来比对"。与 eventID 用同一份拼法,免得两处漂移。
func (s *Stamper) StreamID() string { return s.streamBase() }

// Encode stamps and marshals in one step, for transports that only ever need
// the bytes.
func (s *Stamper) Encode(e event.Event) ([]byte, error) {
	return json.Marshal(s.Wire(e))
}

// eventID is unique within the stream and readable in a log: the stream's
// identity plus the sequence number. There is no need for a random UUID — the
// sequence is already unique per stream, and a derived id keeps the two
// obviously consistent.
func (s *Stamper) eventID(n uint64) string {
	base := s.streamBase()
	if base == "" {
		return strconv.FormatUint(n, 10)
	}
	return base + "#" + strconv.FormatUint(n, 10)
}

// streamBase 是流身份的拼法,eventID 与 StreamID 共用一份。
func (s *Stamper) streamBase() string {
	base := s.sessionID
	if s.tabID != "" {
		if base != "" {
			base += ":"
		}
		base += s.tabID
	}
	return base
}
