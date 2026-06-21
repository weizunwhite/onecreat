package main

import (
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

// M4 + H3 回归:RefreshAccountSession 的网络调用期间用户切了档,刷新回写绝不能把新档冲回
// 旧值。旧代码:refresh 读 tier=1 → 网络 → 回写整个结构体(含进入时的 tier=1),覆盖掉中途
// 的 SetOnecreatTier(3) —— UI 显示新档、下次实际按旧档送模型 + 计费。
func TestRefreshDoesNotClobberConcurrentTierChange(t *testing.T) {
	seedTempConfigDir(t)
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

// M4:applyGatewayEnvFromSession 按会话设/清网关 env —— 登出清空、登录按档位写 tier-N。
func TestApplyGatewayEnvFromSession(t *testing.T) {
	seedTempConfigDir(t)
	t.Setenv(gatewayEnvURL, "")
	t.Setenv(gatewayEnvToken, "")
	t.Setenv(gatewayEnvTier, "")

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
