package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
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
// 因此每个订阅者是一个有缓冲 channel,写满就丢帧(丢的是渲染增量,不是状态;
// 前端在 ready/turn 边界会重新拉 Meta/History 对齐)。
type eventBroadcaster struct {
	mu   sync.Mutex
	seq  int
	subs map[int]chan []byte
}

// sseBufferedFrames 是单个客户端的积压上限。一轮长回答的 token 增量事件很密,
// 给足缓冲让正常客户端不丢帧;真卡死的客户端才开始丢。
const sseBufferedFrames = 512

func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{subs: map[int]chan []byte{}}
}

// Emit 序列化一帧并非阻塞地扇出。序列化失败(payload 含不可 JSON 化的值)时丢弃该帧。
func (b *eventBroadcaster) Emit(channel string, payload any) {
	data, err := json.Marshal(sseFrame{Channel: channel, Payload: payload})
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- data:
		default: // 慢客户端:丢这一帧,绝不阻塞 agent goroutine
		}
	}
}

// subscribe 注册一个新客户端,返回订阅 id 和它的帧通道。
func (b *eventBroadcaster) subscribe() (int, chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := b.seq
	ch := make(chan []byte, sseBufferedFrames)
	b.subs[id] = ch
	return id, ch
}

// unsubscribe 注销客户端(断线时调用),之后 Emit 不再往它写。
func (b *eventBroadcaster) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}

// subscriberCount 供测试断言清理是否到位。
func (b *eventBroadcaster) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

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

	id, ch := b.subscribe()
	defer b.unsubscribe(id)

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
