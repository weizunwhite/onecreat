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
	base := s.sessionID
	if s.tabID != "" {
		if base != "" {
			base += ":"
		}
		base += s.tabID
	}
	if base == "" {
		return strconv.FormatUint(n, 10)
	}
	return base + "#" + strconv.FormatUint(n, 10)
}
