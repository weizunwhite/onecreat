package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// rpcFake 覆盖 App 上出现过的全部方法签名形态,用来单测反射分发/参数解码/返回值折叠,
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
	s := newRPCServer(rpcFake{})

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
	s := newRPCServer(rpcFake{})

	// 无返回值 → result null
	w := postRPC(t, s, "Void", `[]`)
	if w.Code != 200 || decodeResult(t, w) != nil {
		t.Fatalf("Void: 状态 %d body %s", w.Code, w.Body)
	}

	// error(nil) → result null
	w = postRPC(t, s, "Ok", `[]`)
	if w.Code != 200 || decodeResult(t, w) != nil {
		t.Fatalf("Ok: 状态 %d body %s", w.Code, w.Body)
	}

	// error(非 nil) → 500 + error
	w = postRPC(t, s, "Fail", `[]`)
	if w.Code != 500 || decodeErr(t, w) != "炸了" {
		t.Fatalf("Fail: 状态 %d body %s", w.Code, w.Body)
	}

	// (T, error) 成功
	w = postRPC(t, s, "Struct", `[7]`)
	if w.Code != 200 {
		t.Fatalf("Struct: 状态 %d body %s", w.Code, w.Body)
	}
	m, ok := decodeResult(t, w).(map[string]any)
	if !ok || m["n"] != float64(7) {
		t.Fatalf("Struct: 拿到 %s", w.Body)
	}

	// (T, error) 失败
	w = postRPC(t, s, "StructErr", `[]`)
	if w.Code != 500 || decodeErr(t, w) != "坏了" {
		t.Fatalf("StructErr: 状态 %d body %s", w.Code, w.Body)
	}
}

func TestRPCEmptyBodyMeansNoArgs(t *testing.T) {
	s := newRPCServer(rpcFake{})
	w := postRPC(t, s, "Void", "")
	if w.Code != 200 {
		t.Fatalf("空 body 应等价于 []: 状态 %d body %s", w.Code, w.Body)
	}
}

func TestRPCUnknownMethod404(t *testing.T) {
	s := newRPCServer(rpcFake{})
	if w := postRPC(t, s, "NoSuchThing", `[]`); w.Code != 404 {
		t.Fatalf("未知方法应 404,拿到 %d", w.Code)
	}
	// 非导出方法不进方法表
	if w := postRPC(t, s, "unexported", `[]`); w.Code != 404 {
		t.Fatalf("非导出方法应 404,拿到 %d", w.Code)
	}
}

func TestRPCBadArgs400(t *testing.T) {
	s := newRPCServer(rpcFake{})
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
	s := newRPCServer(rpcFake{})
	req := httptest.NewRequest(http.MethodGet, "/rpc/Void", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405,拿到 %d", w.Code)
	}
}

func TestRPCPanicBecomes500(t *testing.T) {
	s := newRPCServer(rpcFake{})
	w := postRPC(t, s, "Boom", `[]`)
	if w.Code != 500 {
		t.Fatalf("panic 应转成 500,拿到 %d", w.Code)
	}
	if !strings.Contains(decodeErr(t, w), "kaboom") {
		t.Fatalf("错误信息应带 panic 原因: %s", w.Body)
	}
}

// TestAppMethodsAreRPCCompatible 守住「143 个方法零手写路由」这个前提:App 上不能
// 出现反射分发处理不了的签名(可变参数 / 三个及以上返回值 / 第二返回值不是 error)。
// 新增方法时如果破了这个约定,这里会红。
func TestAppMethodsAreRPCCompatible(t *testing.T) {
	tp := reflect.TypeOf(NewApp())
	checked := 0
	for i := 0; i < tp.NumMethod(); i++ {
		m := tp.Method(i)
		if rpcExcluded[m.Name] {
			continue
		}
		checked++
		ft := m.Type
		if ft.IsVariadic() {
			t.Errorf("%s 是可变参数方法,RPC 分发不支持", m.Name)
		}
		// ft 含接收者,故返回值数量看 NumOut。
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
	if checked < 100 {
		t.Fatalf("只扫到 %d 个 App 方法,反射方法表疑似坏了", checked)
	}
	t.Logf("App 暴露给 RPC 的方法数: %d", checked)
}
