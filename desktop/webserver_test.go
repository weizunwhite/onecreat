package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<html><head><title>t</title></head><body></body></html>")},
		"assets/app.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/style.css": &fstest.MapFile{Data: []byte("body{}")},
	}
}

func newTestServer(t *testing.T) *webServer {
	t.Helper()
	app := NewApp()
	srv, err := newWebServer(app, testAssets(), newEventBroadcaster(), testToken, "127.0.0.1", 3700, false, "v0.0.0-test")
	if err != nil {
		t.Fatalf("newWebServer: %v", err)
	}
	return srv
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestWebServerRequiresToken(t *testing.T) {
	h := newTestServer(t).Handler()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/rpc/Version", strings.NewReader("[]"))
	if w := do(t, h, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401,拿到 %d (%s)", w.Code, w.Body)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/rpc/Version", strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer wrong-token")
	if w := do(t, h, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401,拿到 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/rpc/Version", strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("带 token 应 200,拿到 %d (%s)", w.Code, w.Body)
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Result == "" {
		t.Fatalf("Version 应返回版本字符串,拿到 %s", w.Body)
	}
}

func TestWebServerRejectsForeignHost(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodPost, "http://evil.example.com/rpc/Version", strings.NewReader("[]"))
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+testToken)
	if w := do(t, h, req); w.Code != http.StatusForbidden {
		t.Fatalf("外部 Host(DNS rebinding)应 403,拿到 %d", w.Code)
	}

	// 端口不对也要挡(同一台机器上的另一个本地服务不能被借道)。
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9999/rpc/Version", strings.NewReader("[]"))
	req.Host = "127.0.0.1:9999"
	req.Header.Set("Authorization", "Bearer "+testToken)
	if w := do(t, h, req); w.Code != http.StatusForbidden {
		t.Fatalf("端口不符应 403,拿到 %d", w.Code)
	}
}

func TestWebServerRejectsCrossOrigin(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/rpc/Version", strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://evil.example.com")
	if w := do(t, h, req); w.Code != http.StatusForbidden {
		t.Fatalf("跨源 Origin 应 403,拿到 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/rpc/Version", strings.NewReader("[]"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "http://127.0.0.1:3700")
	if w := do(t, h, req); w.Code != http.StatusOK {
		t.Fatalf("同源 Origin 应放行,拿到 %d (%s)", w.Code, w.Body)
	}
}

func TestWebServerServesIndexWithWebMarker(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/", nil)
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/ 应 200,拿到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "__ONECREAT_WEB__") {
		t.Fatalf("index.html 应注入 Web 模式标记: %s", w.Body)
	}

	// 静态资源直出;未命中的路径回落到 index(SPA)。
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/assets/app.js", nil)
	if w := do(t, h, req); w.Code != 200 || !strings.Contains(w.Body.String(), "console.log") {
		t.Fatalf("静态资源应直出,拿到 %d %s", w.Code, w.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/some/deep/route", nil)
	if w := do(t, h, req); w.Code != 200 || !strings.Contains(w.Body.String(), "__ONECREAT_WEB__") {
		t.Fatalf("未命中路径应回落到 index,拿到 %d", w.Code)
	}
}

func TestWebServerSSERequiresTokenAndStreams(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	// 无 token 的 SSE 直接 401
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/events", nil)
	if w := do(t, h, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 的 /events 应 401,拿到 %d", w.Code)
	}

	// 带 ?token= 的 SSE 能建立,并收到广播帧
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/events?token="+testToken, nil).WithContext(ctx)
	rec := &syncRecorder{hdr: http.Header{}}

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	waitFor(t, func() bool { return srv.events.subscriberCount() == 1 })
	srv.events.Emit("agent:ready:main", map[string]string{"hi": "there"})
	waitFor(t, func() bool { return strings.Contains(rec.body(), "agent:ready:main") })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("客户端断开后 SSE handler 没退出")
	}
	waitFor(t, func() bool { return srv.events.subscriberCount() == 0 })

	body := rec.body()
	if !strings.HasPrefix(body, ": ok\n\n") {
		t.Fatalf("SSE 应先发一个注释让 EventSource 立刻 open: %q", body)
	}
	if !strings.Contains(body, `data: {"channel":"agent:ready:main"`) {
		t.Fatalf("SSE 帧格式不对: %q", body)
	}
}

// syncRecorder 是加锁版的 ResponseRecorder:SSE 测试里 handler 在另一个 goroutine
// 里持续写,主测试 goroutine 同时读,httptest.ResponseRecorder 会被 -race 判定为竞态。
type syncRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (r *syncRecorder) Header() http.Header { return r.hdr }

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *syncRecorder) Flush() {}

func (r *syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
