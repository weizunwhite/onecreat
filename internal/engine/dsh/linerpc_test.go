package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// fakeServer 在 serverConn 上模拟一个 dsh SDK 运行时:逐行读请求,按 method 回响应,
// 并可在需要时主动推通知。
func fakeServer(t *testing.T, conn net.Conn) {
	t.Helper()
	sc := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req rpcFrame
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // 通知不回
		}
		switch req.Method {
		case MethodInitialize:
			result, _ := json.Marshal(map[string]any{
				"serverInfo": map[string]string{"name": WireServerName, "version": "0.1.0-rc.7"},
			})
			_ = enc.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID, Result: result})
		case MethodSessionPrompt:
			result, _ := json.Marshal(map[string]string{"messageId": "msg-123"})
			_ = enc.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID, Result: result})
			// 顺带推一条 session.event 通知(assistant 文本 delta)。
			evData, _ := json.Marshal(map[string]any{
				"chunk": map[string]string{"type": "text-delta", "text": "hi"},
			})
			ev, _ := json.Marshal(SessionEventNotification{
				SessionID: "s1",
				Event:     RawSessionEvent{Type: EvAssistantChunk, Data: evData},
			})
			_ = enc.Encode(rpcRequest{JSONRPC: jsonrpcVersion, Method: NotifySessionEvent, Params: json.RawMessage(ev)})
		case "bad":
			_ = enc.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found"}})
		}
	}
}

func TestLineClientCallAndNotify(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go fakeServer(t, serverConn)

	notifCh := make(chan string, 4)
	c := NewLineClient(clientConn, clientConn, func(method string, params json.RawMessage) {
		notifCh <- method
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// initialize 往返。
	var res InitializeResult
	if err := c.Call(ctx, MethodInitialize, InitializeParams{CWD: "/tmp", Provider: "deepseek-official", Model: "onecreat"}, &res); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.ServerInfo.Name != WireServerName {
		t.Fatalf("serverInfo.name = %q, want %q", res.ServerInfo.Name, WireServerName)
	}

	// session/prompt 往返 + 触发一条通知。
	var pr SessionPromptResult
	if err := c.Call(ctx, MethodSessionPrompt, SessionPromptParams{SessionID: "s1", ContentBlocks: []ContentBlock{TextBlock("hello")}}, &pr); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if pr.MessageID != "msg-123" {
		t.Fatalf("messageId = %q, want msg-123", pr.MessageID)
	}

	select {
	case m := <-notifCh:
		if m != NotifySessionEvent {
			t.Fatalf("notification method = %q, want %q", m, NotifySessionEvent)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 session.event 通知")
	}
}

func TestLineClientErrorResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go fakeServer(t, serverConn)

	c := NewLineClient(clientConn, clientConn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Call(ctx, "bad", nil, nil)
	if err == nil {
		t.Fatal("期望错误响应,得到 nil")
	}
	var re *rpcError
	if !asRPCError(err, &re) || re.Code != -32601 {
		t.Fatalf("期望 code -32601 的 rpcError,得到 %v", err)
	}
}

// asRPCError 是小工具:判断 err 是否 *rpcError。
func asRPCError(err error, out **rpcError) bool {
	if re, ok := err.(*rpcError); ok {
		*out = re
		return true
	}
	return false
}
