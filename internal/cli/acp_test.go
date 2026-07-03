package cli

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/acp"
	"reasonix/internal/config"
	"reasonix/internal/event"
)

// F1 回归:ACP 会话工厂在网关模式下必须与 chat 会话行为一致——provider 改走网关(model 变
// tier-N)、sysPrompt 含 ModelPrivacyPolicy、planner 关闭;无网关变量时三者都不变。去掉
// NewSession 里补的三处保护,本测试应挂。
func TestACPFactoryGatewayParity(t *testing.T) {
	cfg := config.Default()
	cfg.DefaultModel = "deepseek-flash"
	cfg.Agent.PlannerModel = "deepseek-pro" // 用于验证网关模式下 planner 被关掉

	newSession := func(t *testing.T) (label, sysPrompt string) {
		t.Helper()
		f := &acpFactory{cfg: cfg, model: cfg.DefaultModel}
		ctrl, err := f.NewSession(context.Background(), acp.SessionParams{Sink: event.Discard})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer ctrl.Close()
		return ctrl.Label(), ctrl.SystemPrompt()
	}

	// 网关模式:设 URL/TOKEN/TIER。
	t.Run("gateway", func(t *testing.T) {
		t.Setenv("ONECREAT_GATEWAY_URL", "https://gw.example.com/api/onecreat/v1")
		t.Setenv("ONECREAT_GATEWAY_TOKEN", "tok")
		t.Setenv("ONECREAT_TIER", "tier-2")

		label, sysPrompt := newSession(t)
		if label != "tier-2" {
			t.Fatalf("网关模式 label=%q,want tier-2(model 未改写为档位 / 或 planner 未关)", label)
		}
		if strings.Contains(label, "planner") {
			t.Fatalf("网关模式下 planner 应关闭,label=%q", label)
		}
		if !strings.Contains(sysPrompt, config.ModelPrivacyPolicy) {
			t.Fatal("网关模式 sysPrompt 应含 ModelPrivacyPolicy")
		}
	})

	// 无网关变量:行为不变(直连模型名、planner 在、无隐私政策)。
	t.Run("no_gateway", func(t *testing.T) {
		// 显式清空,避免被外部环境干扰。
		t.Setenv("ONECREAT_GATEWAY_URL", "")
		t.Setenv("ONECREAT_TIER", "")

		label, sysPrompt := newSession(t)
		if !strings.Contains(label, "planner") {
			t.Fatalf("非网关模式 planner 应仍启用,label=%q", label)
		}
		if strings.Contains(sysPrompt, config.ModelPrivacyPolicy) {
			t.Fatal("非网关模式 sysPrompt 不应含 ModelPrivacyPolicy(行为不变)")
		}
	})
}
