package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/acp"
	"reasonix/internal/config"
	"reasonix/internal/event"
)

// acpProject writes a self-contained project the composition root can assemble
// without a network or an API key, and returns its directory.
//
// It is written to disk rather than injected as a *config.Config because the ACP
// factory no longer assembles anything itself: it asks boot.Build, which loads
// the *session workspace's* config. That is the point of Plan 04 — the config a
// session runs under is the project's, resolved by the one composition root, not
// a struct the transport happened to be holding.
func acpProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := `
default_model = "flash"

[codegraph]
enabled = false

[lsp]
enabled = false

[agent]
system_prompt = "BASE"
planner_model = "pro"

[[providers]]
name = "flash"
kind = "openai"
base_url = "https://example.invalid"
model = "deepseek-v4-flash"
api_key_env = "REASONIX_TEST_KEY_UNSET"

[[providers]]
name = "pro"
kind = "openai"
base_url = "https://example.invalid"
model = "deepseek-v4-pro"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`
	if err := os.WriteFile(filepath.Join(dir, "onecreat.toml"), []byte(strings.TrimPrefix(body, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// F1 回归:ACP 会话在网关模式下必须与 chat 会话行为一致——provider 改走网关(model 变
// tier-N)、sysPrompt 含 ModelPrivacyPolicy、planner 关闭。
//
// Plan 04 之前这三条是 acp.go 里手抄的三处保护;现在它们只存在于 boot.Build 一处,
// ACP 因为走同一个装配入口而自动获得。本测试从「检查手抄的副本」变成「检查 ACP 确实
// 继承了唯一真源的行为」。
func TestACPFactoryGatewayParity(t *testing.T) {
	dir := acpProject(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	newSession := func(t *testing.T) (label, sysPrompt string) {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		f := &acpFactory{cfg: cfg, model: "flash"}
		ctrl, err := f.NewSession(context.Background(), acp.SessionParams{Cwd: dir, Sink: event.Discard})
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

	// 无网关变量:直连真实模型名,planner 仍启用。
	t.Run("no_gateway", func(t *testing.T) {
		t.Setenv("ONECREAT_GATEWAY_URL", "")
		t.Setenv("ONECREAT_GATEWAY_TOKEN", "")
		t.Setenv("ONECREAT_TIER", "")

		label, sysPrompt := newSession(t)
		if !strings.Contains(label, "planner") {
			t.Fatalf("非网关模式 planner 应仍启用,label=%q", label)
		}
		if !strings.Contains(label, "deepseek-v4-flash") {
			t.Fatalf("非网关模式应直连真实模型名,label=%q", label)
		}
		// Plan 04 的一处【有意】行为收敛:boot.Build 无条件追加 ModelPrivacyPolicy,
		// 所以 chat / desktop / serve 本地模式下一直都有它;只有 ACP 这条平行装配
		// 从前把它限制在网关模式。收敛之后 ACP 与其它 transport 一致 —— 方向是
		// 「更保守」(多一条不许自曝底层模型的规则),不会少任何保护。
		if !strings.Contains(sysPrompt, config.ModelPrivacyPolicy) {
			t.Fatal("收敛后 ACP 应与 chat 一致:ModelPrivacyPolicy 恒在")
		}
	})
}

// TestACPSessionUsesItsOwnWorkspace proves the per-session isolation the ACP
// client relies on: two sessions opened on different cwds get their own project
// config, not a shared one.
func TestACPSessionUsesItsOwnWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ONECREAT_GATEWAY_URL", "")
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "")

	a := acpProject(t)
	b := acpProject(t)
	if err := os.WriteFile(filepath.Join(a, "REASONIX.md"), []byte("Project rule: this is A."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "REASONIX.md"), []byte("Project rule: this is B."), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := &acpFactory{cfg: cfg, model: "flash"}

	open := func(dir string) string {
		ctrl, err := f.NewSession(context.Background(), acp.SessionParams{Cwd: dir, Sink: event.Discard})
		if err != nil {
			t.Fatalf("NewSession(%s): %v", dir, err)
		}
		defer ctrl.Close()
		return ctrl.SystemPrompt()
	}

	promptA, promptB := open(a), open(b)
	if !strings.Contains(promptA, "this is A") || strings.Contains(promptA, "this is B") {
		t.Errorf("session A picked up the wrong project memory:\n%s", promptA)
	}
	if !strings.Contains(promptB, "this is B") || strings.Contains(promptB, "this is A") {
		t.Errorf("session B picked up the wrong project memory:\n%s", promptB)
	}
}

// TestACPRejectsUnusableCwd: a cwd the client made up is a client error worth
// reporting, not a silent fallback to whatever directory the agent stands in.
func TestACPRejectsUnusableCwd(t *testing.T) {
	cfg := config.Default()
	f := &acpFactory{cfg: cfg, model: cfg.DefaultModel}
	missing := filepath.Join(t.TempDir(), "no-such-project")
	if _, err := f.NewSession(context.Background(), acp.SessionParams{Cwd: missing, Sink: event.Discard}); err == nil {
		t.Fatal("NewSession with a non-existent cwd should fail")
	}
}
