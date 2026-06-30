package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedTempConfigDir 把账号会话目录(os.UserConfigDir 派生)重定向到临时 HOME,使测试不碰
// 真实用户配置。
func seedTempConfigDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
}

func enablePlatformAccountMode(t *testing.T) {
	t.Helper()
	t.Setenv(accountModeEnv, "platform")
}

// M4 + H3 回归:RefreshAccountSession 的网络调用期间用户切了档,刷新回写绝不能把新档冲回
// 旧值。旧代码:refresh 读 tier=1 → 网络 → 回写整个结构体(含进入时的 tier=1),覆盖掉中途
// 的 SetOnecreatTier(3) —— UI 显示新档、下次实际按旧档送模型 + 计费。
func TestRefreshDoesNotClobberConcurrentTierChange(t *testing.T) {
	seedTempConfigDir(t)
	enablePlatformAccountMode(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond) // 制造一个能被 SetOnecreatTier 抢入的窗口
		_ = json.NewEncoder(w).Encode(map[string]any{"loggedIn": true, "points": 42.0})
	}))
	defer srv.Close()
	t.Setenv("ONECREAT_PLATFORM_URL", srv.URL)

	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Account: "u", Token: "tok", SelectedTier: 1})
	sessionFileMu.Unlock()

	a := &App{} // ctx 为 nil:rebuildAllTabs 早返回,无需真 controller

	done := make(chan struct{})
	go func() { a.RefreshAccountSession(); close(done) }()
	time.Sleep(20 * time.Millisecond) // 确保 refresh 已读到 tier=1 并进入网络等待
	a.SetOnecreatTier(3)
	<-done

	sessionFileMu.Lock()
	final, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok {
		t.Fatal("会话应仍存在")
	}
	if final.SelectedTier != 3 {
		t.Fatalf("refresh 覆盖了并发切档:SelectedTier=%d,想要 3", final.SelectedTier)
	}
	if final.Points == nil || *final.Points != 42 {
		t.Fatalf("refresh 应合并服务端 points,得到 %v", final.Points)
	}
}

// M4:档位钳制 + 持久化(越界忽略)。
func TestSetOnecreatTierClampsAndPersists(t *testing.T) {
	seedTempConfigDir(t)
	enablePlatformAccountMode(t)
	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Token: "tok", SelectedTier: 1})
	sessionFileMu.Unlock()
	a := &App{}

	a.SetOnecreatTier(2)
	if got := a.AccountSessionInfo().SelectedTier; got != 2 {
		t.Fatalf("切档后 tier=%d,想要 2", got)
	}
	a.SetOnecreatTier(9) // 越界:忽略,保持 2
	if got := a.AccountSessionInfo().SelectedTier; got != 2 {
		t.Fatalf("越界档位(9)应被忽略,tier=%d,想要 2", got)
	}
	a.SetOnecreatTier(0) // 越界:忽略,保持 2
	if got := a.AccountSessionInfo().SelectedTier; got != 2 {
		t.Fatalf("越界档位(0)应被忽略,tier=%d,想要 2", got)
	}
}

func TestLocalAccountModeIgnoresPersistedPlatformSession(t *testing.T) {
	seedTempConfigDir(t)
	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Account: "old", Token: "tok", SelectedTier: 2})
	sessionFileMu.Unlock()
	t.Setenv(gatewayEnvURL, "https://t.example.com/api/onecreat/v1")
	t.Setenv(gatewayEnvToken, "tok")
	t.Setenv(gatewayEnvTier, "tier-2")

	applyGatewayEnvFromSession()

	if gatewayActive() {
		t.Fatal("默认本地 API 模式不应激活网关")
	}
	if os.Getenv(gatewayEnvURL) != "" || os.Getenv(gatewayEnvToken) != "" || os.Getenv(gatewayEnvTier) != "" {
		t.Fatalf("默认本地 API 模式应清空网关 env,url=%q token=%q tier=%q", os.Getenv(gatewayEnvURL), os.Getenv(gatewayEnvToken), os.Getenv(gatewayEnvTier))
	}
	s := (&App{}).AccountSessionInfo()
	if s.LoggedIn {
		t.Fatalf("默认本地 API 模式不应显示已登录平台账号:%+v", s)
	}
}

// H4(客户端侧):网关模式下,本地改 provider / 模型 / key 必须被后端拒绝 —— 否则登录用户
// 可加一个自带 key 的非网关 provider 完全绕开网关与计量。前端已隐藏 UI,这是后端兜底。
func TestGatewayModeBlocksProviderMutations(t *testing.T) {
	seedTempConfigDir(t)
	// 未登录:守卫不激活。
	t.Setenv(gatewayEnvURL, "")
	if gatewayActive() {
		t.Fatal("无网关 env 时 gatewayActive 应为 false")
	}
	// 网关模式(登录):provider / 模型 / key 改动全部被拒。
	enablePlatformAccountMode(t)
	t.Setenv(gatewayEnvURL, "https://t.example.com/api/onecreat/v1")
	a := &App{ctx: context.Background()}
	checks := map[string]error{
		"SaveProvider":    a.SaveProvider(ProviderView{Name: "evil", Kind: "anthropic"}),
		"SetProviderKey":  a.SetProviderKey("ANTHROPIC_API_KEY", "sk-x"),
		"SetDefaultModel": a.SetDefaultModel("anthropic/claude"),
		"SetPlannerModel": a.SetPlannerModel("anthropic/claude"),
		"DeleteProvider":  a.DeleteProvider("deepseek"),
		"SetModel":        a.SetModel("anthropic/claude"),
	}
	for name, err := range checks {
		if err != errGatewayManaged {
			t.Errorf("%s 网关模式应返回 errGatewayManaged,得到 %v", name, err)
		}
	}
}

// 自动续期:access token 快过期(<20min)时用 refresh_token 静默换新,不动 SelectedTier,
// 并把新 token 写进 ONECREAT_GATEWAY_TOKEN env。
func TestEnsureFreshTokenRefreshes(t *testing.T) {
	seedTempConfigDir(t)
	enablePlatformAccountMode(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/onecreat/refresh" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": "NEW", "refresh_token": "RT2", "expires_in": 3600})
	}))
	defer srv.Close()
	t.Setenv("ONECREAT_PLATFORM_URL", srv.URL)
	t.Setenv(gatewayEnvURL, "")
	t.Setenv(gatewayEnvToken, "")

	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Token: "OLD", RefreshToken: "RT1", ExpiresAt: time.Now().Unix() + 5*60, SelectedTier: 2})
	sessionFileMu.Unlock()

	ensureFreshToken()

	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok || p.Token != "NEW" {
		t.Fatalf("token 未续期: %+v", p)
	}
	if p.RefreshToken != "RT2" {
		t.Fatalf("refresh_token 未轮换: %q", p.RefreshToken)
	}
	if p.ExpiresAt <= time.Now().Unix()+30*60 {
		t.Fatalf("ExpiresAt 未推进: %d", p.ExpiresAt)
	}
	if p.SelectedTier != 2 {
		t.Fatalf("续期不应动 SelectedTier: %d", p.SelectedTier)
	}
	if os.Getenv(gatewayEnvToken) != "NEW" {
		t.Fatalf("env token 未更新为新值: %q", os.Getenv(gatewayEnvToken))
	}
}

// 还早(离过期 >20min)不刷;没有 refresh_token 也不刷 —— 都不该打 refresh 端点。
func TestEnsureFreshTokenSkips(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess persistedSession
	}{
		{"还早", persistedSession{Token: "OLD", RefreshToken: "RT1", ExpiresAt: time.Now().Unix() + 60*60}},
		{"无refresh_token", persistedSession{Token: "OLD", ExpiresAt: time.Now().Unix() - 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedTempConfigDir(t)
			enablePlatformAccountMode(t)
			called := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": "NEW"})
			}))
			defer srv.Close()
			t.Setenv("ONECREAT_PLATFORM_URL", srv.URL)
			sessionFileMu.Lock()
			_ = saveSessionFileLocked(tc.sess)
			sessionFileMu.Unlock()

			ensureFreshToken()
			if called {
				t.Fatal("不该调用 refresh 端点")
			}
			sessionFileMu.Lock()
			p, _ := loadSessionFileLocked()
			sessionFileMu.Unlock()
			if p.Token != "OLD" {
				t.Fatalf("不该续期,token=%q", p.Token)
			}
		})
	}
}

// M4:applyGatewayEnvFromSession 按会话设/清网关 env —— 登出清空、登录按档位写 tier-N。
func TestApplyGatewayEnvFromSession(t *testing.T) {
	seedTempConfigDir(t)
	t.Setenv(gatewayEnvURL, "")
	t.Setenv(gatewayEnvToken, "")
	t.Setenv(gatewayEnvTier, "")

	// 默认 local-first:即便磁盘上还残留旧平台会话,也必须清空网关 env。
	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Token: "old", SelectedTier: 2})
	sessionFileMu.Unlock()
	t.Setenv(gatewayEnvToken, "stale")
	applyGatewayEnvFromSession()
	if v := os.Getenv(gatewayEnvToken); v != "" {
		t.Fatalf("本地 API 模式应清空 token env,得到 %q", v)
	}

	enablePlatformAccountMode(t)
	sessionFileMu.Lock()
	if path, err := accountSessionPath(); err == nil {
		_ = os.Remove(path)
	}
	sessionFileMu.Unlock()

	// 未登录(无会话文件):清空 token env。
	t.Setenv(gatewayEnvToken, "stale")
	applyGatewayEnvFromSession()
	if v := os.Getenv(gatewayEnvToken); v != "" {
		t.Fatalf("未登录应清空 token env,得到 %q", v)
	}

	// 登录 tier 2:写网关 url + token + tier-2。
	sessionFileMu.Lock()
	_ = saveSessionFileLocked(persistedSession{Token: "tok", SelectedTier: 2})
	sessionFileMu.Unlock()
	applyGatewayEnvFromSession()
	if v := os.Getenv(gatewayEnvToken); v != "tok" {
		t.Fatalf("token env=%q,想要 tok", v)
	}
	if v := os.Getenv(gatewayEnvTier); v != "tier-2" {
		t.Fatalf("tier env=%q,想要 tier-2", v)
	}
	if v := os.Getenv(gatewayEnvURL); !strings.HasSuffix(v, "/api/onecreat/v1") {
		t.Fatalf("url env=%q,应以 /api/onecreat/v1 结尾", v)
	}
}
