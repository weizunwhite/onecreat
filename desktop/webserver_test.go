package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// /healthz 无鉴权可达,回 {ok,version};受 Host 守卫。
func TestHealthz(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3700/healthz", nil)
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz 应 200,拿到 %d", w.Code)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 healthz: %v (%s)", err, w.Body)
	}
	if !out.OK || out.Version != "v0.0.0-test" {
		t.Fatalf("healthz 内容不对: %s", w.Body)
	}
	// 外部 Host 仍应被挡。
	req = httptest.NewRequest(http.MethodGet, "http://evil.example.com/healthz", nil)
	req.Host = "evil.example.com"
	if w := do(t, h, req); w.Code != http.StatusForbidden {
		t.Fatalf("外部 Host 的 healthz 应 403,拿到 %d", w.Code)
	}
}

// 构造一个 multipart 上传请求体。
func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// /upload 需要 token。
func TestUploadRequiresToken(t *testing.T) {
	h := newTestServer(t).Handler()
	body, ct := multipartBody(t, "files", "a.txt", []byte("hello"))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/upload", body)
	req.Header.Set("Content-Type", ct)
	if w := do(t, h, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 上传应 401,拿到 %d", w.Code)
	}
}

// /upload 带 token 落盘并返回绝对路径,内容一致。
func TestUploadSavesFile(t *testing.T) {
	h := newTestServer(t).Handler()
	content := []byte("参考资料内容")
	body, ct := multipartBody(t, "files", "note.md", content)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/upload", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("上传应 200,拿到 %d (%s)", w.Code, w.Body)
	}
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Paths) != 1 {
		t.Fatalf("上传响应应含 1 个路径,拿到 %s", w.Body)
	}
	got, err := os.ReadFile(out.Paths[0])
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("落盘内容不一致: %q", got)
	}
	if filepath.Base(out.Paths[0]) != "note.md" {
		t.Fatalf("落盘文件名应保留原名,拿到 %s", out.Paths[0])
	}
	_ = os.RemoveAll(filepath.Dir(out.Paths[0]))
}

// 目录穿越的文件名只取 basename,不写到别处。
func TestUploadSanitizesFilename(t *testing.T) {
	h := newTestServer(t).Handler()
	body, ct := multipartBody(t, "files", "../../etc/evil.conf", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3700/upload", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("上传应 200,拿到 %d (%s)", w.Code, w.Body)
	}
	var out struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Paths) != 1 || filepath.Base(out.Paths[0]) != "evil.conf" {
		t.Fatalf("文件名应被清成 basename,拿到 %v", out.Paths)
	}
	_ = os.RemoveAll(filepath.Dir(out.Paths[0]))
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
