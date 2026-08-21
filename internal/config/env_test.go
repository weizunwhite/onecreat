package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// C1:`.env` 不再往进程环境里写。
//
// 这条断言与它取代的那条(“子进程兼容层还在”)方向相反,是**故意**的。原来的兼容导出
// 是 AR-R10 只修一半时留下的:凭据解析走了叠加层,子进程仍靠 os.Setenv 继承。但那个
// os.Setenv 就是两个工作区互相污染的根 —— 进程环境只有一份,谁先加载谁的值就留在里面,
// 之后所有子进程(bash 里的 gh、MCP 插件、语言服务器)都拿到那一份。
//
// 现在改成:谁要 `.env` 的值,谁显式地把 Env().Environ() 交给自己的子进程。
func TestDotEnvDoesNotTouchTheProcessEnvironment(t *testing.T) {
	os.Unsetenv("AR_R10_SUBPROC")
	ws := writeWorkspace(t, "AR_R10_SUBPROC=only-in-the-overlay\n")
	cfg, err := LoadIn(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := os.LookupEnv("AR_R10_SUBPROC"); ok {
		t.Fatalf(".env 仍然写进了进程环境(值 %q)—— 两个工作区又会互相污染", got)
	}
	if got := cfg.Env().Get("AR_R10_SUBPROC"); got != "only-in-the-overlay" {
		t.Fatalf("叠加层里应当有这个值,拿到 %q", got)
	}
}

// 子进程环境按工作区各自组装:同名 key 两份,互不覆盖。
func TestTwoWorkspacesGetDifferentChildEnvironments(t *testing.T) {
	os.Unsetenv("AR_R10_CHILD")
	a := writeWorkspace(t, "AR_R10_CHILD=child-A\n")
	b := writeWorkspace(t, "AR_R10_CHILD=child-B\n")
	cfgA, err := LoadIn(a)
	if err != nil {
		t.Fatal(err)
	}
	cfgB, err := LoadIn(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupIn(cfgA.Env().Environ(), "AR_R10_CHILD"); got != "child-A" {
		t.Fatalf("工作区 A 的子进程环境拿到 %q", got)
	}
	if got := lookupIn(cfgB.Env().Environ(), "AR_R10_CHILD"); got != "child-B" {
		t.Fatalf("工作区 B 的子进程环境拿到 %q —— A 的值漏过来了", got)
	}
}

// 用户显式设的环境变量在子进程环境里也是最高优先级,`.env` 不得覆盖。
func TestEnvironKeepsTheProcessValueWhenTheUserSetOne(t *testing.T) {
	t.Setenv("AR_R10_CHILD_EXPLICIT", "from-process")
	var env Env
	env.set("AR_R10_CHILD_EXPLICIT", "from-file")
	got := lookupIn(env.Environ(), "AR_R10_CHILD_EXPLICIT")
	if got != "from-process" {
		t.Fatalf("子进程应看到用户显式设的值,拿到 %q", got)
	}
}

// lookupIn 取 `KEY=VALUE` 切片里最后一次出现的值 —— exec 的语义就是后者胜出。
func lookupIn(environ []string, key string) string {
	out := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			out = v
		}
	}
	return out
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

// Windows 的环境变量名不区分大小写,`os/exec` 的去重也不区分且保留最后一个。若这里
// 按大小写敏感比较,进程里的 `Path` 和 `.env` 里的 `PATH` 会一起进切片,子进程最终用
// 到 `.env` 那份 —— 把「进程环境优先」倒过来。Unix 上则必须保持大小写敏感。
//
// 两条规则都在这里跑,与当前 GOOS 无关:否则 Windows 那条只会在 Windows CI 上被执行,
// 而它恰恰是"不执行就静默出错"的那一条。
func TestEnvironMergeFollowsTheNameCasingRule(t *testing.T) {
	overlay := map[string]string{"C1_CASE": "from-file"}
	base := []string{"c1_case=from-process"}

	insensitive := func(k string) string { return strings.ToUpper(k) }
	if _, ok := lookupExact(environMerge(base, overlay, insensitive), "C1_CASE"); ok {
		t.Fatal("大小写不敏感时,同名变量不该被追加 —— 子进程会用 .env 覆盖进程环境")
	}
	sensitive := func(k string) string { return k }
	if _, ok := lookupExact(environMerge(base, overlay, sensitive), "C1_CASE"); !ok {
		t.Fatal("大小写敏感时,C1_CASE 与 c1_case 是两个不同的变量,不该被当成同一个")
	}
}

// envKey 选的是当前平台的那条规则。
func TestEnvKeyMatchesThePlatform(t *testing.T) {
	got := envKey("Path")
	want := "Path"
	if runtime.GOOS == "windows" {
		want = "PATH"
	}
	if got != want {
		t.Fatalf("envKey(%q) = %q,want %q(GOOS=%s)", "Path", got, want, runtime.GOOS)
	}
}

func lookupExact(environ []string, key string) (string, bool) {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}
