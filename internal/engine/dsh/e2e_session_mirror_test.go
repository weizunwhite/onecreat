package dsh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
)

// 端到端(需要真 node + 已装好的 dsh 组合包,故默认跳过):
//
//	ONECREAT_DSH_E2E=1 go test ./internal/engine/dsh/ -run E2E -v
//
// 这两条钉的是「打开历史会话」这件事的两半:
//   - Go 侧的消息镜像(History / 会话落盘 / 前端恢复看的那份投影);
//   - dsh 侧自己的 store(模型可见历史的真源,CapResume 声明的那条)。

// newMirrorEcho 起一个假的 OpenAI 兼容上游:永远回一句纯文本,并把收到的请求体
// 存下来供断言。返回 base URL 与取请求体的闭包。
func newMirrorEcho(t *testing.T, reply string) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", reply)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// TestSessionMirrorFollowsSwapE2E 用**真 sidecar** 复现那条坑:Controller 的
// NewSession / Resume 会把 executor 里的会话换成新对象,引擎的 Go 会话镜像必须
// 每次经 SessionFunc 现取 —— 否则第二轮的 user/assistant 会继续落进已弃用的旧
// 会话,新会话的 History 只剩系统提示。
func TestSessionMirrorFollowsSwapE2E(t *testing.T) {
	if os.Getenv("ONECREAT_DSH_E2E") == "" {
		t.Skip("需要 ONECREAT_DSH_E2E=1(要真 node + `pnpm -C dsh install`)")
	}
	baseURL, _ := newMirrorEcho(t, "收到")

	oldSess := agent.NewSession("system")
	newSess := agent.NewSession("system")
	current := oldSess

	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")
	eng, err := New(Options{
		Gateway:     true,
		TierFunc:    staticTier("tier-2"),
		Cfg:         config.DSHConfig{ModelPlaceholder: "onecreat"},
		CWD:         t.TempDir(),
		Sink:        event.Discard,
		Ledger:      testRecorder(evidence.NewLedger()),
		SessionRoot: t.TempDir(),
		BaseURL:     baseURL,
		APIKeyFunc:  func() string { return os.Getenv("ONECREAT_GATEWAY_TOKEN") },
		SessionFunc: func() *agent.Session { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ctx := context.Background()
	if err := eng.Run(ctx, "第一轮提问"); err != nil {
		t.Fatalf("round1: %v", err)
	}
	// 模拟 Controller.NewSession / Resume:换掉 executor 里的会话对象。
	current = newSess
	if err := eng.Run(ctx, "第二轮提问"); err != nil {
		t.Fatalf("round2: %v", err)
	}

	if got := len(oldSess.Snapshot()); got != 3 { // system + user + assistant
		t.Fatalf("旧会话应停在第一轮的 3 条消息,实际 %d 条(镜像没跟着换会话走)", got)
	}
	msgs := newSess.Snapshot()
	if len(msgs) != 3 {
		t.Fatalf("新会话应有 system+user+assistant 共 3 条,实际 %d 条 —— History 空掉正是这个形态", len(msgs))
	}
	if msgs[1].Role != provider.RoleUser || msgs[1].Content != "第二轮提问" {
		t.Fatalf("新会话第 2 条应是第二轮的用户消息,实际 %+v", msgs[1])
	}
	if msgs[2].Role != provider.RoleAssistant || strings.TrimSpace(msgs[2].Content) == "" {
		t.Fatalf("新会话第 3 条应是 sidecar 回来的 assistant 文本,实际 %+v", msgs[2])
	}
	t.Logf("旧会话 %d 条 / 新会话 %d 条,第二轮落进了新会话", len(oldSess.Snapshot()), len(msgs))
}

// TestDSHSessionResumeRemembersE2E 钉 CapResume 的真实内容:dsh 会话 id 由 Go 会话
// 文件路径确定性派生,**换一个 Engine 实例**(等价于重开 App / 打开历史会话)只要
// BindSession 到同一路径,就能从 dsh 自己的 store 恢复出上一轮 —— 断言的是第二次
// 打开时上游请求体里带着第一轮的内容,也就是"模型还记得"。
func TestDSHSessionResumeRemembersE2E(t *testing.T) {
	if os.Getenv("ONECREAT_DSH_E2E") == "" {
		t.Skip("需要 ONECREAT_DSH_E2E=1(要真 node + `pnpm -C dsh install`)")
	}
	baseURL, bodies := newMirrorEcho(t, "记住了")
	sessionRoot := t.TempDir()
	cwd := t.TempDir()
	// Go 侧的会话文件路径:dsh 会话 id 由它派生(sessionIDFor)。
	sessPath := filepath.Join(t.TempDir(), "20260823-abc.json")

	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")
	newEngine := func() *Engine {
		e, err := New(Options{
			Gateway:     true,
			TierFunc:    staticTier("tier-2"),
			Cfg:         config.DSHConfig{ModelPlaceholder: "onecreat"},
			CWD:         cwd,
			Sink:        event.Discard,
			Ledger:      testRecorder(evidence.NewLedger()),
			SessionRoot: sessionRoot,
			BaseURL:     baseURL,
			APIKeyFunc:  func() string { return os.Getenv("ONECREAT_GATEWAY_TOKEN") },
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	ctx := context.Background()

	// 第一次打开:绑到会话路径,说一件只有这一轮知道的事。
	first := newEngine()
	first.BindSession(sessPath)
	if err := first.Run(ctx, "我的幸运数字是 4747,记住它"); err != nil {
		t.Fatalf("round1: %v", err)
	}
	firstID := first.SessionID()
	_ = first.Close()

	// 第二次打开(全新 Engine 实例,等价于重启进程后点开这条历史会话)。
	second := newEngine()
	second.BindSession(sessPath)
	if got := second.SessionID(); got != firstID {
		t.Fatalf("同一路径应派生同一个 dsh 会话 id:第一次 %q,第二次 %q", firstID, got)
	}
	if err := second.Run(ctx, "我的幸运数字是多少?"); err != nil {
		t.Fatalf("round2: %v", err)
	}
	_ = second.Close()

	all := bodies()
	if len(all) < 2 {
		t.Fatalf("上游只收到 %d 个请求", len(all))
	}
	last := all[len(all)-1]
	if !strings.Contains(last, "4747") {
		t.Fatalf("第二次打开时上游请求体里没有第一轮的内容 —— dsh 侧会话没 resume 上\n请求体: %s", last)
	}
	t.Logf("dsh 会话 id=%s;第二次打开的请求体带着第一轮的 4747,模型记得", firstID)
}
