package serve

import (
	"reasonix/internal/event"
	"reasonix/internal/eventstream"
	"reasonix/internal/eventwire"
)

// Broadcaster is the event.Sink the controller emits to in server mode. It
// stamps each event with the stream envelope (schema version, sequence, id,
// timestamp) and fans it out to every connected SSE subscriber.
//
// Delivery is not uniform, and that is the point (Plan 10 / A11). It used to be:
// every subscriber got a 64-frame channel, and anything that arrived while that
// channel was full was dropped — an ApprovalRequest as readily as a text delta.
// A browser that stalled for a moment could leave the agent blocked forever on a
// prompt nobody would ever see. Now the QoS comes from the event kind: render
// deltas are dropped under pressure, state-bearing frames are queued, and a
// client that will not consume them at all is disconnected rather than served a
// stream with invisible holes in it.
type Broadcaster struct {
	hub   *eventstream.Hub
	stamp *eventwire.Stamper
}

// NewBroadcaster returns an empty Broadcaster ready to accept subscribers.
// sessionID identifies the stream on the wire; it may be empty.
func NewBroadcaster() *Broadcaster { return NewBroadcasterFor("") }

// NewBroadcasterFor is NewBroadcaster with an explicit session id stamped on
// every frame, so a client holding more than one stream can tell them apart.
func NewBroadcasterFor(sessionID string) *Broadcaster {
	return NewBroadcasterWithLimits(sessionID, eventstream.DefaultLimits)
}

// NewBroadcasterWithLimits is NewBroadcasterFor with an explicit backlog policy.
//
// 溢出路径(慢客户端被断流 → 重新对齐)只有在**真的溢出**时才走得到,而默认上限是
// 4096 条状态帧 —— 端到端地把它灌满既慢又不确定。给上限一个显式入口,溢出就能被
// 确定性地构造出来,而不是靠 socket 缓冲的运气。零值退回 DefaultLimits。
func NewBroadcasterWithLimits(sessionID string, l eventstream.Limits) *Broadcaster {
	return &Broadcaster{
		hub:   eventstream.New(l),
		stamp: eventwire.NewStamper(sessionID, ""),
	}
}

// Emit stamps and marshals the event, then hands it to the hub. It never blocks:
// it runs on the agent's run-loop goroutine, and a stalled browser must not
// stall the agent. A marshal failure drops that one frame — a bad event should
// not stall the stream — but it still consumes a sequence number, so the gap is
// visible to the client rather than silent.
func (b *Broadcaster) Emit(e event.Event) {
	data, err := b.stamp.Encode(e)
	if err != nil {
		return
	}
	b.hub.Publish(eventstream.Frame{Data: data, Durable: e.Kind.Durable()})
}

// Subscribe registers a new SSE client.
func (b *Broadcaster) Subscribe() *eventstream.Sub { return b.hub.Subscribe() }

// Unsubscribe releases a client (the handler defers it).
func (b *Broadcaster) Unsubscribe(s *eventstream.Sub) { b.hub.Unsubscribe(s) }

// Sequence 返回这条流已发出的最后一个序号,StreamID 返回它的身份。权威快照要带上
// 它们,客户端才能判断"我手里的状态截止到哪儿"以及"这还是不是同一条流"(AR-R07)。
func (b *Broadcaster) Sequence() uint64 { return b.stamp.Sequence() }

// StreamID 见 Sequence。
func (b *Broadcaster) StreamID() string { return b.stamp.StreamID() }

// Subscribers reports the current connection count (for diagnostics/tests).
func (b *Broadcaster) Subscribers() int { return b.hub.Subscribers() }
