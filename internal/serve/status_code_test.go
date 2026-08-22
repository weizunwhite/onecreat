package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reasonix/internal/control"
	"reasonix/internal/engine"
)

// AR-R02 的验收里有一条,我上一轮只做到一半:
//
//	「HTTP 返回明确 409/422;Desktop/CLI 禁用并显示原因;后端校验不能依赖 UI。」
//
// 后端确实在改任何状态之前就拒绝了,但拒绝之后 HTTP 一律回 500。500 的含义是
// **服务器坏了,可以重试** —— 而事实恰恰相反:这个引擎永远做不到这件事,重试多少次
// 都一样。一个照着状态码行事的客户端(重试队列、自动化脚本)会一直撞下去,而真正的
// 原因只在响应体的中文里。语义错误的状态码,和不返回原因差不太多。
//
// 正确的映射有两条,而且必须分开:
//   - 引擎不支持 → 422:请求本身没问题,但这个引擎处理不了,**别重试**;
//   - 有回合在跑 → 409:是状态冲突,**待会儿可以再来**。
//
// 把这两条混成一个码,等于告诉客户端「分不清该不该重试」。

// capLimitedEngine 是一个只声明 streaming 的引擎:除了跑一轮什么都不支持。
// 嵌入 engine.Set 就自带了 Capable 所需的 Supports 方法。
type capLimitedEngine struct{ engine.Set }

func (capLimitedEngine) Start(context.Context, engine.TurnRequest) (engine.TurnHandle, error) {
	return nil, nil
}
func (capLimitedEngine) EngineName() string { return "cap-limited" }

func newCapLimited() capLimitedEngine {
	return capLimitedEngine{Set: engine.Set{engine.CapStreaming: true}}
}

func newCapLimitedServer(t *testing.T) string {
	t.Helper()
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{
		Runner: fakeRunner{got: make(chan string, 8)},
		Sink:   bc,
		Engine: newCapLimited(),
	})
	srv := httptest.NewServer(New(ctrl, bc).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 引擎不支持 → 422,不是 500。
func TestUnsupportedOperationReturns422NotServerError(t *testing.T) {
	base := newCapLimitedServer(t)
	for _, tc := range []struct{ path, body string }{
		{"/fork", `{"turn":1}`},
		{"/rewind", `{"turn":1,"scope":"conversation"}`},
		{"/new", `{}`},
		{"/compact", `{}`},
	} {
		resp := postJSON(t, base+tc.path, tc.body)
		if resp.StatusCode == http.StatusInternalServerError {
			t.Errorf("POST %s 回了 500 —— 客户端会当成「服务器坏了」反复重试,而这个引擎永远做不到", tc.path)
			continue
		}
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("POST %s = %d,want 422", tc.path, resp.StatusCode)
		}
	}
}

// 快照要带能力表:前端据此禁用入口并说明原因。后端仍然独立校验 —— UI 不是安全边界,
// 这里给的是「显示原因」所需的数据,不是那道门本身。
func TestSnapshotCarriesEngineCapabilities(t *testing.T) {
	base := newCapLimitedServer(t)
	snap := getJSON(t, base+"/snapshot")
	caps, ok := snap["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("快照缺少 capabilities(拿到 %T)—— 前端只能靠撞 422 才知道哪些入口该禁用", snap["capabilities"])
	}
	if caps["fork"] != false {
		t.Errorf("capabilities.fork = %v,这个引擎并不支持", caps["fork"])
	}
	if caps["streaming"] != true {
		t.Errorf("capabilities.streaming = %v,这个引擎声明了它", caps["streaming"])
	}
	if snap["engine"] != "cap-limited" {
		t.Errorf("快照的 engine = %v,want cap-limited", snap["engine"])
	}
}

// 三条映射逐一验证。
//
// 为什么 409 那条测的是映射函数而不是端到端:`Compact` / `NewSession` 在
// `executor == nil` 时会提前返回(serve 的测试装配里没有真 executor),端到端根本走不到
// 忙碌分支。与其搭一个真 `agent.Agent` 只为了造一个状态,不如直接测我加的那段逻辑 ——
// 而「映射确实被这些端点用上了」由上面的 422 端到端用例证明。
func TestWriteOpErrorMapsEachKindToItsOwnStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"引擎不支持 → 别重试", engine.Unsupported("dsh", "fork", engine.CapFork), http.StatusUnprocessableEntity},
		{"有回合在跑 → 待会儿再来", &control.BusyError{Msg: "正在运行中"}, http.StatusConflict},
		{"其它 → 确实是服务端的问题", errors.New("disk on fire"), http.StatusInternalServerError},
	} {
		rec := httptest.NewRecorder()
		writeOpError(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%s:得到 %d,want %d", tc.name, rec.Code, tc.want)
		}
		if !strings.Contains(rec.Body.String(), tc.err.Error()) {
			t.Errorf("%s:响应体没带上原因(%q)", tc.name, rec.Body.String())
		}
	}
}

// 包装过的错误也要认出来 —— 控制层随时可能给错误加一层上下文,靠字符串匹配的映射
// 会在那一天悄悄退回 500。
func TestWriteOpErrorSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("分叉会话:%w", engine.Unsupported("dsh", "fork", engine.CapFork))
	rec := httptest.NewRecorder()
	writeOpError(rec, wrapped)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("包装后的 UnsupportedError 得到 %d,want 422", rec.Code)
	}
}
