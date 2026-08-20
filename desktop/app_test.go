package main

import (
	"context"
	"os"
	"reasonix/internal/account"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
)

// blockingWorkspaceRunner 阻塞在 Run 里,让 controller 的 Running() 稳定为 true,用于 D3。
type blockingWorkspaceRunner struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingWorkspaceRunner) Run(ctx context.Context, input string) error {
	b.started <- struct{}{}
	<-b.release
	return nil
}

// D3 回归:活动标签有回合在跑时,SwitchWorkspace 必须后端拒绝——前端 disabled={running} 只
// 覆盖 UI 入口,os.Chdir 后在途 bash(cmd.Dir 空 → 用进程 cwd)会落到新项目目录执行。去掉
// 运行态守卫,本测试应挂。
func TestSwitchWorkspaceRejectsWhileRunning(t *testing.T) {
	runner := &blockingWorkspaceRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	ctrl := control.New(control.Options{Runner: runner})
	defer ctrl.Close()

	a := newBareApp(context.Background(), nil)
	a.tabs.Register(&tabRuntime{id: "t", ctrl: ctrl})

	ctrl.Submit("hello") // 起一个会阻塞的回合 → Running() 变 true
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("回合未在 2s 内进入 runner")
	}
	defer close(runner.release) // 让阻塞的回合收尾

	if _, err := a.SwitchWorkspace(t.TempDir()); err == nil || !strings.Contains(err.Error(), "有任务正在运行") {
		t.Fatalf("运行中切换项目应被拒绝,got err=%v", err)
	}
}

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
	old := control.New(control.Options{Label: "old-controller"})
	app.tabUpdate("", func(rt *tabRuntime) {
		rt.model = "deepseek-flash/deepseek-v4-flash"
		rt.ctrl = old
	})
	defer func() {
		if ctrl := app.activeCtrl(); ctrl != nil {
			ctrl.Close()
		}
	}()

	if err := app.SetEffort("max"); err != nil {
		t.Fatalf("SetEffort(max): %v", err)
	}
	if app.activeCtrl() == nil {
		t.Fatal("SetEffort should leave a rebuilt controller")
	}
	if app.activeCtrl() == old {
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
	running := control.New(control.Options{Runner: runner})
	app.tabUpdate("", func(rt *tabRuntime) { rt.ctrl = running })
	running.Submit("work")
	<-runner.started

	err := app.SetEffort("max")
	if err == nil || !strings.Contains(err.Error(), "finish or cancel") {
		t.Fatalf("SetEffort while running error = %v, want finish/cancel guard", err)
	}

	close(runner.release)
	waitNotRunning(t, running)
}

func TestGatewayModeHidesModelManagementSurfaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(accountModeEnv, "platform")
	app := NewApp()
	app.gateway.SetSession("https://t.example.com/api/onecreat/v1", "tok", "tier-1")
	app.tabUpdate("", func(rt *tabRuntime) {
		rt.ctrl = control.New(control.Options{Label: "deepseek-flash/tier-1"})
		rt.model = "deepseek-flash/deepseek-v4-flash"
		rt.label = "deepseek-flash/tier-1"
	})

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

// clearGatewayEnv 保证进程环境里没有残留的网关变量。它只影响 account.FromEnv 的
// 【导入】(NewApp 启动时读一次),不再是运行期状态源 —— 那是 app.gateway 这个对象。
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(account.EnvURL, "")
	t.Setenv(account.EnvToken, "")
	t.Setenv(account.EnvTier, "")
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
