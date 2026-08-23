package numbering

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fakeToken = "cluster-internal-token-不该出现在任何错误里"

// fakeNAS 起一个假 NAS,返回发号器与最后一次收到的请求快照。
type capturedRequest struct {
	method string
	path   string
	line   string
	auth   string
	accept string
}

func fakeNAS(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*NAS, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.line = r.URL.Query().Get("line")
		got.auth = r.Header.Get("Authorization")
		got.accept = r.Header.Get("Accept")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewNAS(srv.URL+"/", fakeToken, srv.Client()), got
}

// 正常预告:端点、方法、query、Bearer 头都要与 main.py:4669 / photo-uploader-client.ts:371 对齐。
func TestNASPreviewHitsInternalEndpointWithBearer(t *testing.T) {
	nas, got := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok": true, "code": "P26C-032", "line": "C"}`)
	})

	code, err := nas.Preview(context.Background(), " c ") // 顺带验一下大小写/空格标准化
	if err != nil {
		t.Fatalf("Preview 失败: %v", err)
	}
	if code != "P26C-032" {
		t.Fatalf("code = %s，想要 P26C-032", code)
	}
	if got.method != http.MethodGet {
		t.Fatalf("method = %s，想要 GET", got.method)
	}
	if got.path != PreviewPath {
		t.Fatalf("path = %s，想要 %s", got.path, PreviewPath)
	}
	if got.line != "C" {
		t.Fatalf("line = %q，想要 C", got.line)
	}
	if got.auth != "Bearer "+fakeToken {
		t.Fatalf("Authorization 头不对: %q", got.auth)
	}
	if got.accept != "application/json" {
		t.Fatalf("Accept 头 = %q", got.accept)
	}
}

// 非 200:要把对端 detail 带出来,但绝不能把 token 带出来。
func TestNASPreviewSurfacesDetailWithoutLeakingToken(t *testing.T) {
	nas, _ := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// 故意让对端把 token 回显出来,验证兜底擦除。
		fmt.Fprintf(w, `{"detail": "bad internal token %s"}`, fakeToken)
	})

	_, err := nas.Preview(context.Background(), "C")
	if err == nil {
		t.Fatal("想要错误，却成功了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "bad internal token") {
		t.Fatalf("错误信息没带上状态码/detail: %s", msg)
	}
	if strings.Contains(msg, fakeToken) {
		t.Fatalf("token 泄进了错误体: %s", msg)
	}
}

// 对端 5xx / 空体也要给出人话,不能 panic。
func TestNASPreviewHandlesEmptyErrorBody(t *testing.T) {
	nas, _ := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := nas.Preview(context.Background(), "C")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("想要带 503 的错误，得到 %v", err)
	}
}

// 返回体不合严口径(比如给了个历史号)一律拒收:号是三端身份证,别让它流进建档链。
func TestNASPreviewRejectsMalformedOrMismatchedCode(t *testing.T) {
	cases := map[string]string{
		"历史号":      `{"ok": true, "code": "P26-032", "line": "C"}`,
		"业务线对不上":   `{"ok": true, "code": "P26B-032", "line": "B"}`,
		"ok=false": `{"ok": false, "code": "P26C-032", "line": "C"}`,
		"不是JSON":   `<html>502</html>`,
	}
	for name, body := range cases {
		nas, _ := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, body)
		})
		if _, err := nas.Preview(context.Background(), "C"); err == nil {
			t.Fatalf("%s:想要错误，却成功了", name)
		}
	}
}

// 业务线本地就该拦下,别浪费一次网络往返。
func TestNASPreviewValidatesLineBeforeRequest(t *testing.T) {
	nas, got := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("不该发出请求")
	})
	if _, err := nas.Preview(context.Background(), "X"); !errors.Is(err, ErrBadLine) {
		t.Fatalf("想要 ErrBadLine，得到 %v", err)
	}
	if got.method != "" {
		t.Fatal("非法业务线仍然发了请求")
	}
}

// 没配 token 直接 fail-closed(对齐 NAS 侧 _require_internal 的 503 口径)。
func TestNASPreviewFailsClosedWithoutToken(t *testing.T) {
	nas := NewNAS("http://127.0.0.1:1", "", nil)
	if _, err := nas.Preview(context.Background(), "C"); err == nil ||
		!strings.Contains(err.Error(), "token") {
		t.Fatalf("想要「token 未配置」错误，得到 %v", err)
	}
	if _, err := NewNAS("", fakeToken, nil).Preview(context.Background(), "C"); err == nil {
		t.Fatal("地址为空时应当报错")
	}
}

// 超时与 ctx 取消都要如实报错,不能挂死。
func TestNASPreviewTimesOut(t *testing.T) {
	slow := make(chan struct{})
	t.Cleanup(func() { close(slow) })
	nas, _ := fakeNAS(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slow:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	nas.client.Timeout = 80 * time.Millisecond

	start := time.Now()
	if _, err := nas.Preview(context.Background(), "C"); err == nil {
		t.Fatal("想要超时错误，却成功了")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时没生效，等了 %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	nas.client.Timeout = 5 * time.Second
	if _, err := nas.Preview(ctx, "C"); err == nil {
		t.Fatal("ctx 已取消，应当报错")
	}
}

// 迁移期 NAS.Issue 一律拒绝:真实立项仍走 NASApp 单入口(D-1 / §2.2 E29)。
func TestNASIssueIsNotEnabledDuringMigration(t *testing.T) {
	nas, got := fakeNAS(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Issue 不该打 NAS")
	})
	_, err := nas.Issue(context.Background(), Request{Line: "C", ShortName: "题", ExpectedCode: "P26C-032"})
	if !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("想要 ErrNotEnabled，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "NASApp") {
		t.Fatalf("错误里得说清该去哪:%v", err)
	}
	if got.method != "" {
		t.Fatal("Issue 竟然发出了 HTTP 请求")
	}
	// 入参仍然要体检,别让坏参数在 M4 打开那天才第一次被发现。
	if _, err := nas.Issue(context.Background(), Request{Line: "X", ShortName: "题"}); !errors.Is(err, ErrBadLine) {
		t.Fatalf("想要 ErrBadLine，得到 %v", err)
	}
}
