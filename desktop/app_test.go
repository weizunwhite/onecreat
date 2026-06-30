package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
)

func TestCommandsIncludesEffortNotThinking(t *testing.T) {
	clearGatewayEnv(t)
	app := NewApp()
	cmds := app.Commands()
	if !hasCommand(cmds, "effort") {
		t.Fatalf("Commands() should include effort: %+v", cmds)
	}
	if hasCommand(cmds, "thinking") {
		t.Fatalf("Commands() should not include thinking: %+v", cmds)
	}
}

func TestEffortDefaultsBeforeStartup(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got := NewApp().Effort()
	if !got.Supported || got.Current != "auto" || got.Default != "high" || !hasLevel(got.Levels, "auto") {
		t.Fatalf("pre-startup Effort() = %+v, want auto with DeepSeek default high", got)
	}
}

func TestSetEffortPersistsAndAutoClears(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	app := NewApp()
	if err := app.SetEffort("max"); err != nil {
		t.Fatalf("SetEffort(max): %v", err)
	}
	if got := app.Effort().Current; got != "max" {
		t.Fatalf("Effort current = %q, want max", got)
	}
	if err := app.SetEffort("auto"); err != nil {
		t.Fatalf("SetEffort(auto): %v", err)
	}
	if got := app.Effort().Current; got != "auto" {
		t.Fatalf("Effort current = %q, want auto", got)
	}
	body, err := os.ReadFile(config.UserConfigPath())
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if strings.Contains(string(body), `effort      = "max"`) {
		t.Fatalf("auto should clear explicit max effort:\n%s", body)
	}
}

func TestSetEffortRebuildsController(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	app := NewApp()
	app.ctx = context.Background()
	app.model = "deepseek-flash/deepseek-v4-flash"
	old := control.New(control.Options{Label: "old-controller"})
	app.ctrl = old
	defer func() {
		if app.ctrl != nil {
			app.ctrl.Close()
		}
	}()

	if err := app.SetEffort("max"); err != nil {
		t.Fatalf("SetEffort(max): %v", err)
	}
	if app.ctrl == nil {
		t.Fatal("SetEffort should leave a rebuilt controller")
	}
	if app.ctrl == old {
		t.Fatal("SetEffort should rebuild the active controller so the provider sees the new effort")
	}
	if got := app.Effort().Current; got != "max" {
		t.Fatalf("Effort current = %q, want max", got)
	}
}

func TestSetEffortRejectsRunningTurn(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	app := NewApp()
	app.ctrl = control.New(control.Options{Runner: runner})
	app.ctrl.Submit("work")
	<-runner.started

	err := app.SetEffort("max")
	if err == nil || !strings.Contains(err.Error(), "finish or cancel") {
		t.Fatalf("SetEffort while running error = %v, want finish/cancel guard", err)
	}

	close(runner.release)
	waitNotRunning(t, app.ctrl)
}

func TestGatewayModeHidesModelManagementSurfaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(accountModeEnv, "platform")
	t.Setenv(gatewayEnvURL, "https://t.example.com/api/onecreat/v1")
	app := NewApp()
	app.ctrl = control.New(control.Options{Label: "deepseek-flash/tier-1"})
	app.model = "deepseek-flash/deepseek-v4-flash"
	app.label = "deepseek-flash/tier-1"

	if got := app.SlashArgs("/model "); len(got.Items) != 0 {
		t.Fatalf("网关模式 /model 补全不应暴露真实模型: %+v", got.Items)
	}
	if got := app.SlashArgs("/effort "); len(got.Items) != 0 {
		t.Fatalf("网关模式 /effort 补全不应暴露模型能力: %+v", got.Items)
	}
	if hasCommand(app.Commands(), "model") || hasCommand(app.Commands(), "effort") {
		t.Fatalf("网关模式命令列表不应暴露模型管理入口: %+v", app.Commands())
	}
	if got := app.Models(); len(got) != 0 {
		t.Fatalf("网关模式 Models() 不应返回真实模型: %+v", got)
	}
	if got := app.Meta().Label; strings.Contains(strings.ToLower(got), "deepseek") {
		t.Fatalf("网关模式 Meta label 不应暴露真实模型: %q", got)
	}
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(gatewayEnvURL, "")
	t.Setenv(gatewayEnvToken, "")
	t.Setenv(gatewayEnvTier, "")
}

func (r *blockingRunner) Run(ctx context.Context, _ string) error {
	close(r.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

func waitNotRunning(t *testing.T, ctrl *control.Controller) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for ctrl.Running() {
		if time.Now().After(deadline) {
			t.Fatal("controller still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasLevel(levels []string, want string) bool {
	for _, level := range levels {
		if level == want {
			return true
		}
	}
	return false
}

func hasCommand(cmds []CommandInfo, name string) bool {
	for _, cmd := range cmds {
		if cmd.Name == name {
			return true
		}
	}
	return false
}
