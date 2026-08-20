package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rpcFake 覆盖 App 上出现过的全部方法签名形态，用来单测反射分发/参数解码/返回值折叠，
// 不必构造真 controller。
type rpcFake struct{}

func (rpcFake) Echo(s string) string                 { return s }
func (rpcFake) Add(a, b int) int                     { return a + b }
func (rpcFake) Void()                                {}
func (rpcFake) Fail() error                          { return errors.New("炸了") }
func (rpcFake) Ok() error                            { return nil }
func (rpcFake) Struct(n int) (map[string]int, error) { return map[string]int{"n": n}, nil }
func (rpcFake) StructErr() (map[string]int, error)   { return nil, errors.New("坏了") }
func (rpcFake) Boom()                                { panic("kaboom") }
func (rpcFake) unexported() string                   { return "hidden" } //nolint:unused

func methodSet(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func newFakeRPCServer() *rpcServer {
	return newRPCServerWithMethods(rpcFake{}, methodSet(
		"Echo", "Add", "Void", "Fail", "Ok", "Struct", "StructErr", "Boom",
	))
}

func postRPC(t *testing.T, s *rpcServer, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rpc/"+method, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func decodeResult(t *testing.T, w *httptest.ResponseRecorder) any {
	t.Helper()
	var out struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, w.Body.String())
	}
	return out.Result
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Error
}

func TestRPCDecodesPositionalArgs(t *testing.T) {
	s := newFakeRPCServer()

	w := postRPC(t, s, "Echo", `["你好"]`)
	if w.Code != 200 {
		t.Fatalf("Echo: 状态 %d, body %s", w.Code, w.Body)
	}
	if got := decodeResult(t, w); got != "你好" {
		t.Fatalf("Echo: 拿到 %#v", got)
	}

	w = postRPC(t, s, "Add", `[2,40]`)
	if w.Code != 200 {
		t.Fatalf("Add: 状态 %d", w.Code)
	}
	if got := decodeResult(t, w); got != float64(42) {
		t.Fatalf("Add: 拿到 %#v", got)
	}
}

func TestRPCReturnSignatures(t *testing.T) {
	s := newFakeRPCServer()

	w := postRPC(t, s, "Void", `[]`)
	if w.Code != 200 || decodeResult(t, w) != nil {
		t.Fatalf("Void: 状态 %d body %s", w.Code, w.Body)
	}

	w = postRPC(t, s, "Ok", `[]`)
	if w.Code != 200 || decodeResult(t, w) != nil {
		t.Fatalf("Ok: 状态 %d body %s", w.Code, w.Body)
	}

	w = postRPC(t, s, "Fail", `[]`)
	if w.Code != 500 || decodeErr(t, w) != "炸了" {
		t.Fatalf("Fail: 状态 %d body %s", w.Code, w.Body)
	}

	w = postRPC(t, s, "Struct", `[7]`)
	if w.Code != 200 {
		t.Fatalf("Struct: 状态 %d body %s", w.Code, w.Body)
	}
	m, ok := decodeResult(t, w).(map[string]any)
	if !ok || m["n"] != float64(7) {
		t.Fatalf("Struct: 拿到 %s", w.Body)
	}

	w = postRPC(t, s, "StructErr", `[]`)
	if w.Code != 500 || decodeErr(t, w) != "坏了" {
		t.Fatalf("StructErr: 状态 %d body %s", w.Code, w.Body)
	}
}

func TestRPCEmptyBodyMeansNoArgs(t *testing.T) {
	s := newFakeRPCServer()
	w := postRPC(t, s, "Void", "")
	if w.Code != 200 {
		t.Fatalf("空 body 应等价于 []: 状态 %d body %s", w.Code, w.Body)
	}
}

func TestRPCUnknownMethod404(t *testing.T) {
	s := newFakeRPCServer()
	if w := postRPC(t, s, "NoSuchThing", `[]`); w.Code != 404 {
		t.Fatalf("未知方法应 404,拿到 %d", w.Code)
	}
	if w := postRPC(t, s, "unexported", `[]`); w.Code != 404 {
		t.Fatalf("非导出方法应 404,拿到 %d", w.Code)
	}
}

func TestRPCAllowlistRejectsUnlistedExportedMethod(t *testing.T) {
	s := newRPCServerWithMethods(rpcFake{}, methodSet("Echo"))
	if w := postRPC(t, s, "Add", `[1,2]`); w.Code != http.StatusNotFound {
		t.Fatalf("导出但未 allowlist 的方法必须 404,拿到 %d", w.Code)
	}
	if w := postRPC(t, s, "Echo", `["ok"]`); w.Code != http.StatusOK {
		t.Fatalf("allowlist 方法应可调用,拿到 %d", w.Code)
	}
}

func TestRPCBadArgs400(t *testing.T) {
	s := newFakeRPCServer()
	if w := postRPC(t, s, "Add", `[1]`); w.Code != 400 {
		t.Fatalf("参数个数不对应 400,拿到 %d", w.Code)
	}
	if w := postRPC(t, s, "Add", `[1,2,3]`); w.Code != 400 {
		t.Fatalf("参数过多应 400,拿到 %d", w.Code)
	}
	if w := postRPC(t, s, "Echo", `[123]`); w.Code != 400 {
		t.Fatalf("参数类型不符应 400,拿到 %d", w.Code)
	}
	if w := postRPC(t, s, "Echo", `{"a":1}`); w.Code != 400 {
		t.Fatalf("body 非数组应 400,拿到 %d", w.Code)
	}
}

func TestRPCRejectsNonPost(t *testing.T) {
	s := newFakeRPCServer()
	req := httptest.NewRequest(http.MethodGet, "/rpc/Void", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405,拿到 %d", w.Code)
	}
}

func TestRPCPanicBecomes500(t *testing.T) {
	s := newFakeRPCServer()
	w := postRPC(t, s, "Boom", `[]`)
	if w.Code != 500 {
		t.Fatalf("panic 应转成 500,拿到 %d", w.Code)
	}
	if !strings.Contains(decodeErr(t, w), "kaboom") {
		t.Fatalf("错误信息应带 panic 原因: %s", w.Body)
	}
}

// TestAppMethodsAreRPCCompatible validates the explicit browser API rather than
// every exported App method. An exported helper can now exist without becoming an
// HTTP endpoint; every allowlisted method must exist and use a supported signature.
func TestAppMethodsAreRPCCompatible(t *testing.T) {
	tp := reflect.TypeOf(NewApp())
	if len(rpcPublicMethods) < 100 {
		t.Fatalf("RPC allowlist 只有 %d 项,疑似被意外截断", len(rpcPublicMethods))
	}
	for name := range rpcPublicMethods {
		m, ok := tp.MethodByName(name)
		if !ok {
			t.Errorf("RPC allowlist 方法 %s 不存在于 *App", name)
			continue
		}
		ft := m.Type
		if ft.IsVariadic() {
			t.Errorf("%s 是可变参数方法,RPC 分发不支持", m.Name)
		}
		switch ft.NumOut() {
		case 0, 1:
		case 2:
			if ft.Out(1) != errType {
				t.Errorf("%s 的第二个返回值不是 error", m.Name)
			}
		default:
			t.Errorf("%s 有 %d 个返回值,RPC 分发不支持", m.Name, ft.NumOut())
		}
	}
}

func TestRPCSurfaceMatchesFrontendBindings(t *testing.T) {
	b, err := os.ReadFile("frontend/src/lib/bridge.ts")
	if err != nil {
		t.Fatalf("读取 bridge.ts: %v", err)
	}
	text := string(b)
	const marker = "export interface AppBindings {"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatal("bridge.ts 缺少 AppBindings 接口")
	}
	body := text[start+len(marker):]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	} else {
		t.Fatal("bridge.ts 的 AppBindings 接口未闭合")
	}
	re := regexp.MustCompile(`(?m)^\s{2}([A-Z][A-Za-z0-9_]*)\(`)
	matches := re.FindAllStringSubmatch(body, -1)
	bridge := make([]string, 0, len(matches))
	for _, m := range matches {
		bridge = append(bridge, m[1])
	}
	sort.Strings(bridge)

	allowed := make([]string, 0, len(rpcPublicMethods))
	for name := range rpcPublicMethods {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	if !reflect.DeepEqual(bridge, allowed) {
		t.Fatalf("Web RPC allowlist 与 AppBindings 漂移\nbridge=%v\nallowlist=%v", bridge, allowed)
	}
}
