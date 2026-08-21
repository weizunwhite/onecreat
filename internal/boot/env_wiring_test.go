package boot

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
	"reasonix/internal/workspace"
)

func lookupIn(environ []string, key string) (string, bool) {
	out, found := "", false
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			out, found = v, true
		}
	}
	return out, found
}

// dotEnvWorkspace 造一个带 `.env` 的最小工作区。
func dotEnvWorkspace(t *testing.T, dotenv string) workspace.Context {
	t.Helper()
	dir := t.TempDir()
	projectConfig(t, dir, "PROMPT", "rule")
	writeFile(t, dir, ".env", dotenv)
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// workspaceEnv 加载一个带 `.env` 的工作区,返回它的叠加层。
func workspaceEnv(t *testing.T, dotenv string) config.Env {
	t.Helper()
	cfg, err := config.LoadIn(dotEnvWorkspace(t, dotenv))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Env()
}

// C1 的装配端断言:Build 真的把本工作区的 `.env` 交给了会起子进程的东西。
//
// `.env` 不再 os.Setenv 之后,漏接一处就是**静默失效** —— 用户的 token 突然对某个
// 钩子/插件/语言服务器不生效,而且没有任何报错。所以这条必须走 Build,不能只测
// config 层拼得对不对。Hook Runner 是这条链上唯一从 Controller 可观察的一环。
func TestBuildHandsEachWorkspaceItsOwnChildEnv(t *testing.T) {
	os.Unsetenv("C1_BOOT_KEY")
	t.Chdir(t.TempDir()) // 进程 cwd 既不是 A 也不是 B

	wsA := dotEnvWorkspace(t, "C1_BOOT_KEY=boot-A\n")
	wsB := dotEnvWorkspace(t, "C1_BOOT_KEY=boot-B\n")

	ctrlA, err := Build(context.Background(), Options{Workspace: wsA})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	defer ctrlA.Close()
	ctrlB, err := Build(context.Background(), Options{Workspace: wsB})
	if err != nil {
		t.Fatalf("Build(B): %v", err)
	}
	defer ctrlB.Close()

	got, ok := lookupIn(ctrlA.HookRunner().Env(), "C1_BOOT_KEY")
	if !ok || got != "boot-A" {
		t.Fatalf("工作区 A 的钩子子进程环境:%q(存在=%v)", got, ok)
	}
	got, ok = lookupIn(ctrlB.HookRunner().Env(), "C1_BOOT_KEY")
	if !ok || got != "boot-B" {
		t.Fatalf("工作区 B 的钩子子进程环境:%q(存在=%v)—— A 的值漏过来了", got, ok)
	}
	if v, ok := os.LookupEnv("C1_BOOT_KEY"); ok {
		t.Fatalf("Build 把 .env 写进了进程环境(%q)", v)
	}
}

// bash 工具是用户最直接感知这件事的地方(`.env` 里的 GITHUB_TOKEN 对 `gh` 还灵不灵)。
// 这里真起一个 shell,不只断言字段拼接。
func TestAddBuiltinsGivesBashTheWorkspaceEnv(t *testing.T) {
	sh := sandbox.ResolveShell()
	cmd := "echo $C1_BUILTIN_KEY"
	if sh.Kind == sandbox.ShellPowerShell {
		cmd = "echo $env:C1_BUILTIN_KEY"
	}
	args, _ := json.Marshal(map[string]any{"command": cmd})

	run := func(childEnv []string) string {
		t.Helper()
		reg := tool.NewRegistry()
		addBuiltins(reg, workspace.Context{}, []string{"bash"}, nil,
			sandbox.Spec{Mode: "off"}, builtin.SearchSpec{}, childEnv, os.Stderr)
		bt, ok := reg.Get("bash")
		if !ok {
			t.Fatal("bash 没进注册表")
		}
		out, err := bt.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("bash 执行失败:%v(输出 %q)", err, out)
		}
		return strings.TrimSpace(out)
	}

	base := os.Environ()
	if got := run(append(append([]string(nil), base...), "C1_BUILTIN_KEY=ws-A")); got != "ws-A" {
		t.Fatalf("工作区 A 的 bash 拿到 %q", got)
	}
	if got := run(append(append([]string(nil), base...), "C1_BUILTIN_KEY=ws-B")); got != "ws-B" {
		t.Fatalf("工作区 B 的 bash 拿到 %q —— 两个工作区共用了一份环境", got)
	}
}

// MCP 插件两件事都要按工作区来:`${VAR}` 用哪个工作区的值展开,子进程用哪份环境启动。
func TestPluginSpecsInResolveAndLaunchPerWorkspace(t *testing.T) {
	os.Unsetenv("C1_PLUGIN_KEY")
	t.Chdir(t.TempDir())
	envA := workspaceEnv(t, "C1_PLUGIN_KEY=plugin-A\n")
	envB := workspaceEnv(t, "C1_PLUGIN_KEY=plugin-B\n")

	entries := []config.PluginEntry{{
		Name:    "s",
		Type:    "stdio",
		Command: "server",
		Args:    []string{"--token=${C1_PLUGIN_KEY}"},
	}}

	a := PluginSpecsIn(envA, entries)[0]
	b := PluginSpecsIn(envB, entries)[0]

	if a.Args[0] != "--token=plugin-A" {
		t.Fatalf("工作区 A 的 ${VAR} 展开成 %q", a.Args[0])
	}
	if b.Args[0] != "--token=plugin-B" {
		t.Fatalf("工作区 B 的 ${VAR} 展开成 %q —— 展开时用错了工作区", b.Args[0])
	}
	if got, _ := lookupIn(a.BaseEnv, "C1_PLUGIN_KEY"); got != "plugin-A" {
		t.Fatalf("工作区 A 的插件子进程环境:%q", got)
	}
	if got, _ := lookupIn(b.BaseEnv, "C1_PLUGIN_KEY"); got != "plugin-B" {
		t.Fatalf("工作区 B 的插件子进程环境:%q", got)
	}
	// 没有叠加层的调用方(ACP 的 per-session servers)保持 nil,继承进程环境。
	if PluginSpecs(entries)[0].BaseEnv != nil {
		t.Fatal("零值 Env 不该凭空造一份 BaseEnv")
	}
}

// 语言服务器由 Factory 按工作区启动 —— 这条链没有别的运行时守卫:真跑一个 gopls
// 需要 PATH 上有它,CI 上不成立。所以直接断言 Factory 造出来的 manager 带的是本
// 工作区的环境;manager.Env() 返回的就是它 spawn 时用的基底。
func TestFactoryGivesTheLSPManagerTheWorkspaceEnv(t *testing.T) {
	os.Unsetenv("C1_LSP_WS_KEY")
	t.Chdir(t.TempDir())

	open := func(dotenv string) []string {
		t.Helper()
		ws := lspWorkspace(t, dotenv)
		cfg, err := config.LoadIn(ws)
		if err != nil {
			t.Fatal(err)
		}
		f := NewFactory(context.Background())
		t.Cleanup(f.Close)
		h := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: ws.Root()})
		t.Cleanup(h.Release)
		if h.svc.lsp == nil {
			t.Fatal("LSP manager 没被启动")
		}
		return h.svc.lsp.Env()
	}

	if got, _ := lookupIn(open("C1_LSP_WS_KEY=lsp-A\n"), "C1_LSP_WS_KEY"); got != "lsp-A" {
		t.Fatalf("工作区 A 的语言服务器环境:%q", got)
	}
	if got, _ := lookupIn(open("C1_LSP_WS_KEY=lsp-B\n"), "C1_LSP_WS_KEY"); got != "lsp-B" {
		t.Fatalf("工作区 B 的语言服务器环境:%q —— A 的值漏过来了", got)
	}
}

// lspWorkspace 是 dotEnvWorkspace 的开了 LSP 的版本。
func lspWorkspace(t *testing.T, dotenv string) workspace.Context {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "onecreat.toml", `
default_model = "test-model"

[codegraph]
enabled = false

[lsp]
enabled = true

[[providers]]
name = "test-model"
kind = "openai"
base_url = "https://example.invalid"
model = "x"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`)
	writeFile(t, dir, ".env", dotenv)
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
