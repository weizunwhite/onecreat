package main

import (
	"encoding/json"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/eventstream"
	"reasonix/internal/eventwire"
)

func frameOf(t *testing.T, s *eventstream.Sub) (channel string, payload json.RawMessage) {
	t.Helper()
	f, ok := s.TryNext()
	if !ok {
		t.Fatal("订阅者没收到帧")
	}
	var wrapped struct {
		Channel string          `json:"channel"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(f.Data, &wrapped); err != nil {
		t.Fatalf("收到的不是 JSON: %v", err)
	}
	return wrapped.Channel, wrapped.Payload
}

func TestBroadcasterFansOutToAllSubscribers(t *testing.T) {
	b := newEventBroadcaster()
	a := b.subscribe()
	c := b.subscribe()
	if b.subscriberCount() != 2 {
		t.Fatalf("应有 2 个订阅者,拿到 %d", b.subscriberCount())
	}

	b.Emit("serial:data", "hello")
	for i, sub := range []*eventstream.Sub{a, c} {
		ch, payload := frameOf(t, sub)
		if ch != "serial:data" || string(payload) != `"hello"` {
			t.Fatalf("订阅者 %d 收到 %s / %s", i, ch, payload)
		}
	}
}

func TestBroadcasterUnsubscribeStopsDelivery(t *testing.T) {
	b := newEventBroadcaster()
	sub := b.subscribe()
	b.unsubscribe(sub)
	if b.subscriberCount() != 0 {
		t.Fatalf("注销后应为 0,拿到 %d", b.subscriberCount())
	}
	b.Emit("agent:event:main", map[string]string{"kind": "token"})
	if _, ok := sub.TryNext(); ok {
		t.Fatal("注销后不应再收到帧")
	}
}

// 慢客户端(从不读)只会丢自己的【渲染增量】,绝不能阻塞 Emit —— Emit 跑在 agent
// 运行循环上,阻塞就等于把整个会话卡死。
func TestBroadcasterDropsForSlowClientWithoutBlocking(t *testing.T) {
	b := newEventBroadcaster()
	slow := b.subscribe()
	fast := b.subscribe()

	stamp := eventwire.NewStamper("", "main")
	total := eventstream.DefaultLimits.Ephemeral + 50
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			b.Emit("agent:event:main", stamp.Wire(event.Event{Kind: event.Text, Text: "x"})) // slow 从不读
			if _, ok := fast.Next(); !ok {                                                   // fast 同步跟读,保证不是靠缓冲侥幸
				return
			}
		}
		close(done)
	}()
	<-done // 不加超时:真阻塞了这里就是死锁,go test 的超时会打印全部栈,比断言更好定位

	if slow.DroppedEphemeral() == 0 {
		t.Fatal("慢客户端应开始丢渲染增量")
	}
	if slow.Overflowed() {
		t.Fatal("光是增量洪水不该把订阅判死")
	}
}

// Plan 10 的验收:慢客户端可以丢增量,但绝不能丢掉审批 / turn_done —— 丢了会让 agent
// 永远卡在没人看得见的提示上、UI 永远转圈。
func TestSlowClientStillGetsApprovalAndTurnDone(t *testing.T) {
	b := newEventBroadcaster()
	slow := b.subscribe()
	defer b.unsubscribe(slow)

	stamp := eventwire.NewStamper("", "main")
	for i := 0; i < eventstream.DefaultLimits.Ephemeral*4; i++ {
		b.Emit("agent:event:main", stamp.Wire(event.Event{Kind: event.Text, Text: "x"}))
	}
	b.Emit("agent:event:main", stamp.Wire(event.Event{Kind: event.ApprovalRequest, Approval: event.Approval{ID: "1", Tool: "bash"}}))
	b.Emit("agent:event:main", stamp.Wire(event.Event{Kind: event.TurnDone}))

	var sawApproval, sawTurnDone bool
	for {
		f, ok := slow.TryNext()
		if !ok {
			break
		}
		var wrapped struct {
			Payload eventwire.Event `json:"payload"`
		}
		if err := json.Unmarshal(f.Data, &wrapped); err != nil {
			continue
		}
		switch wrapped.Payload.Kind {
		case "approval_request":
			sawApproval = true
		case "turn_done":
			sawTurnDone = true
		}
	}
	if !sawApproval || !sawTurnDone {
		t.Fatalf("状态帧被丢了:approval=%v turn_done=%v", sawApproval, sawTurnDone)
	}
}

// 非 agent 通道(串口数据、ready 通知)没有 QoS 标记,一律按状态帧处理 —— 默认不丢
// 是安全的方向。
func TestUnknownChannelsAreTreatedAsDurable(t *testing.T) {
	if !frameIsDurable("some serial bytes") {
		t.Error("未知通道应按状态帧处理")
	}
	if !frameIsDurable(eventwire.NewStamper("", "").Wire(event.Event{Kind: event.TurnDone})) {
		t.Error("turn_done 必须是状态帧")
	}
	if frameIsDurable(eventwire.NewStamper("", "").Wire(event.Event{Kind: event.Text})) {
		t.Error("文本增量应可丢")
	}
}

func TestBroadcasterSkipsUnmarshalablePayload(t *testing.T) {
	b := newEventBroadcaster()
	sub := b.subscribe()
	b.Emit("agent:event:main", make(chan int)) // channel 不能 JSON 序列化
	if _, ok := sub.TryNext(); ok {
		t.Fatal("不可序列化的 payload 应被丢弃,而不是发出半截帧")
	}
}
