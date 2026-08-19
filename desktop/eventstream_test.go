package main

import (
	"encoding/json"
	"testing"
)

func TestBroadcasterFansOutToAllSubscribers(t *testing.T) {
	b := newEventBroadcaster()
	_, a := b.subscribe()
	_, c := b.subscribe()
	if b.subscriberCount() != 2 {
		t.Fatalf("应有 2 个订阅者,拿到 %d", b.subscriberCount())
	}

	b.Emit("serial:data", "hello")
	for i, ch := range []chan []byte{a, c} {
		select {
		case data := <-ch:
			var f struct {
				Channel string `json:"channel"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatalf("订阅者 %d 收到的不是 JSON: %v", i, err)
			}
			if f.Channel != "serial:data" || f.Payload != "hello" {
				t.Fatalf("订阅者 %d 收到 %+v", i, f)
			}
		default:
			t.Fatalf("订阅者 %d 没收到帧", i)
		}
	}
}

func TestBroadcasterUnsubscribeStopsDelivery(t *testing.T) {
	b := newEventBroadcaster()
	id, ch := b.subscribe()
	b.unsubscribe(id)
	if b.subscriberCount() != 0 {
		t.Fatalf("注销后应为 0,拿到 %d", b.subscriberCount())
	}
	b.Emit("agent:event:main", map[string]string{"kind": "token"})
	select {
	case <-ch:
		t.Fatal("注销后不应再收到帧")
	default:
	}
}

// 慢客户端(不读自己的 channel)只会丢自己的帧,绝不能阻塞 Emit —— Emit 跑在 agent
// 运行循环上,阻塞就等于把整个会话卡死。
func TestBroadcasterDropsForSlowClientWithoutBlocking(t *testing.T) {
	b := newEventBroadcaster()
	_, slow := b.subscribe()
	_, fast := b.subscribe()

	total := sseBufferedFrames + 50
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			b.Emit("agent:event:main", i) // slow 从不读
			<-fast                        // fast 同步跟读,保证不是靠缓冲侥幸
		}
		close(done)
	}()
	<-done // 不加超时:真阻塞了这里就是死锁,go test 的超时会打印全部栈,比断言更好定位

	if got := len(slow); got != sseBufferedFrames {
		t.Fatalf("慢客户端缓冲应打满在 %d,拿到 %d", sseBufferedFrames, got)
	}
}

func TestBroadcasterSkipsUnmarshalablePayload(t *testing.T) {
	b := newEventBroadcaster()
	_, ch := b.subscribe()
	b.Emit("agent:event:main", make(chan int)) // channel 不能 JSON 序列化
	select {
	case <-ch:
		t.Fatal("不可序列化的 payload 应被丢弃,而不是发出半截帧")
	default:
	}
}
