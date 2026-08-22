package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"reasonix/internal/eventstream"
	"reasonix/internal/eventwire"
)

// Web 模式的事件流:Wails 下 App 的事件走 runtime.EventsEmit 直推 webview,浏览器
// 模式下没有这条通道,改成「一条 SSE 长连接 + channel 字段分发」。前端只开一个
// EventSource,按 payload 里的 channel 把事件路由给 onEvent/onReady/onSerialData…
//
// 每条帧的 data 是 {"channel":"agent:event:main","payload":{…}}。

// sseFrame 是一条广播帧的线格式。
type sseFrame struct {
	Channel string `json:"channel"`
	Payload any    `json:"payload"`
}

// eventBroadcaster 把 Shell.Emit 扇出给所有已连接的 SSE 客户端。
//
// 关键约束:Emit 跑在 agent 的运行循环 goroutine 上,绝不能被慢客户端阻塞。
// 但「不阻塞」不等于「什么都能丢」:渲染增量丢了只是画面抖一下,审批请求丢了会让
// agent 永远卡在一个没人看得见的提示上,turn_done 丢了会让 UI 永远转圈。所以 QoS
// 由事件本身决定,交给 internal/eventstream 统一实现(Plan 10 / A11):
// 增量在积压时丢,状态帧只排队,完全不消费的客户端会被断开而不是被静默漏发。
type eventBroadcaster struct {
	hub *eventstream.Hub
}

func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{hub: eventstream.New(eventstream.DefaultLimits)}
}

// Emit 序列化一帧并非阻塞地扇出。序列化失败(payload 含不可 JSON 化的值)时丢弃该帧。
//
// durable 从 payload 推:agent 事件自带 QoS 标记(Stamper 盖的),其它通道
// (serial:data、agent:ready:<tab>)一律按状态帧处理 —— 默认不丢是安全的方向。
func (b *eventBroadcaster) Emit(channel string, payload any) {
	data, err := json.Marshal(sseFrame{Channel: channel, Payload: payload})
	if err != nil {
		return
	}
	b.hub.Publish(eventstream.Frame{Data: data, Durable: frameIsDurable(payload)})
}

// frameIsDurable 判断一帧能不能在客户端落后时丢掉。
func frameIsDurable(payload any) bool {
	if w, ok := payload.(eventwire.Event); ok {
		return w.Durable
	}
	return true // 不认识的通道:按状态帧处理
}

// subscribe 注册一个新客户端。
func (b *eventBroadcaster) subscribe() *eventstream.Sub { return b.hub.Subscribe() }

// unsubscribe 注销客户端(断线时调用),之后 Emit 不再往它写。
func (b *eventBroadcaster) unsubscribe(s *eventstream.Sub) { b.hub.Unsubscribe(s) }

// subscriberCount 供测试断言清理是否到位。
func (b *eventBroadcaster) subscriberCount() int { return b.hub.Subscribers() }

// sseHeartbeat 是心跳间隔:发一行 SSE 注释,让中间层/浏览器知道连接还活着,
// 同时让 Write 失败(客户端已消失但 ctx 还没取消)尽早暴露。
const sseHeartbeat = 25 * time.Second

// serveSSE 是 GET /events 的处理函数:挂一个订阅,把帧写成 SSE,直到客户端断开。
func (b *eventBroadcaster) serveSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 先冲一个注释,让 EventSource 立刻进入 open 状态(而不是等第一条事件)。
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	sub := b.subscribe()
	defer b.unsubscribe(sub)

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		// 一次把积压全部写出去,而不是每帧唤醒一次。
		for {
			f, ok := sub.TryNext()
			if !ok {
				break
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", f.Data); err != nil {
				return
			}
			flusher.Flush()
		}
		// 完全不消费状态帧的客户端:断开它,让它重连后重新对齐,而不是继续推一条
		// 悄悄缺了东西的流(Plan 10 唯一排除的选项就是「静默继续」)。
		if sub.Overflowed() {
			fmt.Fprint(w, "event: stream_reset\ndata: {\"reason\":\"client too slow; reconnect to re-sync\"}\n\n")
			flusher.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-sub.Wake():
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
