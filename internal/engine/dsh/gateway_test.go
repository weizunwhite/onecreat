package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// 网关模式:wire model 必须是用户当前选中的档位 —— 平台网关就是按 tier-N 映射
// 真实模型与计费的。下发占位符会让"切档"完全不生效(甚至被网关拒)。
func TestWireModelGatewayUsesTier(t *testing.T) {
	t.Setenv(gatewayTierEnv, "tier-3")
	eng, err := New(Options{Gateway: true, Cfg: config.DSHConfig{ModelPlaceholder: "onecreat"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "tier-3" {
		t.Fatalf("网关模式应下发档位 tier-3,得到 %q", got)
	}
}

// 没设档位(过渡期 / 旧会话)时回退占位符,绝不回退成真实模型名。
func TestWireModelGatewayFallsBackToPlaceholder(t *testing.T) {
	t.Setenv(gatewayTierEnv, "")
	eng, err := New(Options{Gateway: true, Cfg: config.DSHConfig{
		ModelPlaceholder: "onecreat", DirectModel: "deepseek-v4-flash",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "onecreat" {
		t.Fatalf("未设档位应回退占位符,得到 %q", got)
	}
	// 占位符也没配时的兜底同样不能是真实模型名。
	eng2, _ := New(Options{Gateway: true})
	if got := eng2.wireModel(); got != "onecreat" {
		t.Fatalf("占位符缺省兜底错误: %q", got)
	}
}

// 直连模式(用户自己的 key)不受 ONECREAT_TIER 影响:那时下发的就是真实模型 id。
func TestWireModelDirectIgnoresTier(t *testing.T) {
	t.Setenv(gatewayTierEnv, "tier-2")
	eng, err := New(Options{Gateway: false, Cfg: config.DSHConfig{DirectModel: "deepseek-v4-flash"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "deepseek-v4-flash" {
		t.Fatalf("直连模式不该读档位,得到 %q", got)
	}
}

// 档位占位符不能被脱敏器误擦(擦了就等于把 model 字段打成 "OneCreat",网关认不出)。
func TestScrubberLeavesTierAlone(t *testing.T) {
	s := NewScrubber("OneCreat", "deepseek-official", "llm-deepseek", "DeepSeek", "deepseek",
		"api.deepseek.com", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", "https://t.example.com/api/onecreat/v1")
	for _, tier := range []string{"tier-1", "tier-2", "tier-3"} {
		if got := s.Text(tier); got != tier {
			t.Fatalf("档位占位符被误擦: %q → %q", tier, got)
		}
	}
}

// credFake 是一个最小的假 sidecar:回 session.load / credentials.set / session/prompt,
// 并在每次 prompt 后推 running→idle 让一轮收敛。它把收到的请求方法按顺序记进 seen。
type credFake struct {
	seen  chan string
	creds chan CredentialsParams
}

func newCredFake(t *testing.T, conn net.Conn) *credFake {
	t.Helper()
	f := &credFake{seen: make(chan string, 32), creds: make(chan CredentialsParams, 8)}
	go func() {
		sc := bufio.NewScanner(conn)
		enc := json.NewEncoder(conn)
		reply := func(id *int64, v any) {
			raw, _ := json.Marshal(v)
			_ = enc.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: id, Result: raw})
		}
		status := func(s string) {
			raw, _ := json.Marshal(SessionStatusNotification{SessionID: "s1", Status: s})
			_ = enc.Encode(rpcRequest{JSONRPC: jsonrpcVersion, Method: NotifySessionStatus, Params: json.RawMessage(raw)})
		}
		for sc.Scan() {
			var req rpcFrame
			if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
				continue
			}
			f.seen <- req.Method
			switch req.Method {
			case MethodSessionLoad:
				reply(req.ID, SessionLoadResult{})
			case MethodCredentialsSet:
				var p CredentialsParams
				_ = json.Unmarshal(req.Params, &p)
				f.creds <- p
				reply(req.ID, map[string]bool{"ok": true})
			case MethodSessionPrompt:
				reply(req.ID, SessionPromptResult{MessageID: "m1"})
				status("running")
				status("idle")
			default:
				reply(req.ID, map[string]any{})
			}
		}
	}()
	return f
}

// next 取下一个被 sidecar 收到的请求方法名。
func (f *credFake) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-f.seen:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("等 sidecar 收到请求超时")
		return ""
	}
}

// 令牌轮换:平台 token 约 50 分钟过期,刷新只改父进程 env,而子进程环境是 spawn 时
// 的快照。引擎必须在每轮 prompt 前重取凭证,变了就先补发 credentials.set;没变不重发。
func TestCredentialsRotatedBeforePrompt(t *testing.T) {
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	fake := newCredFake(t, serverConn)

	eng, err := New(Options{
		Gateway:    true,
		Sink:       event.Discard,
		Ledger:     evidence.NewLedger(),
		BaseURL:    "https://t.example.com/api/onecreat/v1",
		APIKeyFunc: func() string { return os.Getenv("ONECREAT_GATEWAY_TOKEN") },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.rpc = NewLineClient(clientConn, clientConn, eng.onNotify)
	eng.started = true
	// 模拟 spawn:childEnv 会把当时的连接事实拷进子进程环境并记下快照。
	eng.snapshotCredentials(eng.currentBaseURL(), eng.currentAPIKey())

	ctx := context.Background()

	// 第一轮:凭证与 spawn 快照相同 → 不该发 credentials.set。
	if err := eng.Run(ctx, "第一轮"); err != nil {
		t.Fatalf("第一轮 Run: %v", err)
	}
	if m := fake.next(t); m != MethodSessionLoad {
		t.Fatalf("第一轮首个请求应是 session.load,得到 %s", m)
	}
	if m := fake.next(t); m != MethodSessionPrompt {
		t.Fatalf("凭证没变却发了 %s(应直接 prompt)", m)
	}

	// token 被后台刷新 → 第二轮必须先补发新凭证,再发 prompt。
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t2")
	if err := eng.Run(ctx, "第二轮"); err != nil {
		t.Fatalf("第二轮 Run: %v", err)
	}
	if m := fake.next(t); m != MethodCredentialsSet {
		t.Fatalf("令牌轮换后第二轮应先发 credentials.set,得到 %s", m)
	}
	select {
	case p := <-fake.creds:
		if p.APIKey != "t2" {
			t.Fatalf("下发的凭证不对: %q", p.APIKey)
		}
		if p.BaseURL != "" {
			t.Fatalf("base URL 没变却被下发: %q", p.BaseURL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没收到 credentials.set 载荷")
	}
	if m := fake.next(t); m != MethodSessionPrompt {
		t.Fatalf("credentials.set 之后应是 prompt,得到 %s", m)
	}

	// 第三轮:凭证没再变 → 不重发。
	if err := eng.Run(ctx, "第三轮"); err != nil {
		t.Fatalf("第三轮 Run: %v", err)
	}
	if m := fake.next(t); m != MethodSessionPrompt {
		t.Fatalf("凭证未变却重发了 %s", m)
	}
}
