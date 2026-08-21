package config

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/workspace"
)

// writeWorkspace 造一个带 .env 与最小 onecreat.toml 的工作区。
func writeWorkspace(t *testing.T, dotenv string) workspace.Context {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := "[[providers]]\nname = \"p\"\nkind = \"openai\"\nbase_url = \"https://example.invalid\"\nmodel = \"m\"\napi_key_env = \"AR_R10_KEY\"\n"
	if err := os.WriteFile(filepath.Join(dir, "onecreat.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func providerKey(t *testing.T, ws workspace.Context) string {
	t.Helper()
	cfg, err := LoadIn(ws)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cfg.Provider("p")
	if !ok {
		t.Fatal("找不到 provider p")
	}
	return e.APIKey()
}

// AR-R10 的核心断言:同一个进程里两个工作区,同名 key 各是各的。
//
// 原来 .env 直接 os.Setenv 且「已有值不覆盖」,于是**先加载的那个永久获胜** ——
// 工作区 B 的 provider 会一直拿着工作区 A 的 key。用错 key 意味着请求打到别的账号上,
// 或者计费记到别人头上。
func TestTwoWorkspacesDoNotShareTheirDotEnvKeys(t *testing.T) {
	os.Unsetenv("AR_R10_KEY")
	a := writeWorkspace(t, "AR_R10_KEY=key-from-A\n")
	b := writeWorkspace(t, "AR_R10_KEY=key-from-B\n")

	if got := providerKey(t, a); got != "key-from-A" {
		t.Fatalf("工作区 A 应拿到自己的 key,拿到 %q", got)
	}
	if got := providerKey(t, b); got != "key-from-B" {
		t.Fatalf("工作区 B 拿到了 %q —— 先加载的工作区把它的 key 泄漏过来了", got)
	}
	// 反向再来一次:顺序不该影响结果。
	if got := providerKey(t, a); got != "key-from-A" {
		t.Fatalf("再次加载工作区 A 时拿到 %q", got)
	}
}

// 某个工作区没有的 key,不该从别的工作区借来。
func TestAKeyOnlyInAnotherWorkspaceIsNotVisible(t *testing.T) {
	os.Unsetenv("AR_R10_KEY")
	withKey := writeWorkspace(t, "AR_R10_KEY=only-here\n")
	without := writeWorkspace(t, "SOMETHING_ELSE=1\n")

	if got := providerKey(t, withKey); got != "only-here" {
		t.Fatalf("有 key 的工作区应拿到它,拿到 %q", got)
	}
	if got := providerKey(t, without); got != "" {
		t.Fatalf("没配 key 的工作区应当是未配置,却拿到了 %q", got)
	}
}

// 用户显式设的环境变量永远赢过任何 .env —— 这条语义不能变。
func TestExplicitProcessEnvStillWins(t *testing.T) {
	t.Setenv("AR_R10_EXPLICIT", "from-process")
	var env Env
	env.set("AR_R10_EXPLICIT", "from-file")
	if got := env.Get("AR_R10_EXPLICIT"); got != "from-process" {
		t.Fatalf("进程环境应最高优先级,拿到 %q", got)
	}
}

// 零值 Env 的行为与直接读进程环境一致 —— 那些不经 Load 构造 ProviderEntry 的地方
// (测试、setup 向导)不受这次改动影响。
func TestZeroEnvBehavesLikeGetenv(t *testing.T) {
	t.Setenv("AR_R10_ZERO", "v")
	var zero Env
	if got := zero.Get("AR_R10_ZERO"); got != "v" {
		t.Fatalf("零值 Env 应等价于 os.Getenv,拿到 %q", got)
	}
	if got := zero.Get("AR_R10_DEFINITELY_UNSET"); got != "" {
		t.Fatalf("未设置的变量应为空,拿到 %q", got)
	}
}

// 子进程兼容层还在:.env 的值仍然会落到进程环境,否则 bash 工具里的 gh / docker
// 会突然拿不到 token —— 那是一个用户可见的回归,不能悄悄做。
func TestDotEnvIsStillExportedForSubprocesses(t *testing.T) {
	os.Unsetenv("AR_R10_SUBPROC")
	ws := writeWorkspace(t, "AR_R10_SUBPROC=visible-to-children\n")
	if _, err := LoadIn(ws); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AR_R10_SUBPROC"); got != "visible-to-children" {
		t.Fatalf("子进程兼容层丢了:os.Getenv 拿到 %q", got)
	}
	os.Unsetenv("AR_R10_SUBPROC")
}

// ResolveModel 返回的是结构体副本,副本必须自己带着叠加层 —— 否则一离开 Config
// 就退化成读进程环境,这个 bug 会非常难查。
func TestResolvedModelCopyKeepsTheWorkspaceOverlay(t *testing.T) {
	os.Unsetenv("AR_R10_KEY")
	a := writeWorkspace(t, "AR_R10_KEY=key-from-A\n")
	b := writeWorkspace(t, "AR_R10_KEY=key-from-B\n")
	if _, err := LoadIn(a); err != nil {
		t.Fatal(err)
	}
	cfgB, err := LoadIn(b)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cfgB.ResolveModel("p")
	if !ok {
		t.Fatal("ResolveModel 失败")
	}
	if got := e.APIKey(); got != "key-from-B" {
		t.Fatalf("ResolveModel 的副本拿到 %q —— 副本丢了工作区叠加层", got)
	}
}
