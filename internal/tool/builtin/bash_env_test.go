package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"reasonix/internal/sandbox"
)

// C1 的真子进程断言:两个工作区的 `.env` 同名变量必须各归各。
//
// `.env` 不再 os.Setenv 之后,bash 工具只能靠显式的 env 切片拿到这些值。这个测试真的
// 起一个 shell 把变量打出来 —— 只断言 Go 层的字段拼接,证明不了子进程看见了什么。
func TestBashSeesItsOwnWorkspaceEnvNotAnothers(t *testing.T) {
	sh := sandbox.ResolveShell()
	cmd := "echo $C1_WS_KEY"
	if sh.Kind == sandbox.ShellPowerShell {
		cmd = "echo $env:C1_WS_KEY"
	}
	args, _ := json.Marshal(map[string]any{"command": cmd})

	run := func(env []string) string {
		t.Helper()
		out, err := bash{shell: sh, env: env}.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("bash 执行失败:%v(输出 %q)", err, out)
		}
		return strings.TrimSpace(out)
	}

	// 进程环境里没有这个 key,两个工作区各自从 .env 带一份进来。
	if _, ok := os.LookupEnv("C1_WS_KEY"); ok {
		t.Skip("宿主环境已经设了 C1_WS_KEY")
	}
	base := os.Environ()
	a := append(append([]string(nil), base...), "C1_WS_KEY=from-A")
	b := append(append([]string(nil), base...), "C1_WS_KEY=from-B")

	if got := run(a); got != "from-A" {
		t.Fatalf("工作区 A 的 bash 拿到 %q", got)
	}
	if got := run(b); got != "from-B" {
		t.Fatalf("工作区 B 的 bash 拿到 %q —— 两个工作区共用了同一份环境", got)
	}
}

// 不传 env 时退回进程环境:CLI 这类进程级前端的行为一个字节都不变。
func TestBashWithoutAnExplicitEnvInheritsTheProcessEnvironment(t *testing.T) {
	t.Setenv("C1_INHERITED", "yes")
	sh := sandbox.ResolveShell()
	cmd := "echo $C1_INHERITED"
	if sh.Kind == sandbox.ShellPowerShell {
		cmd = "echo $env:C1_INHERITED"
	}
	args, _ := json.Marshal(map[string]any{"command": cmd})
	out, err := bash{shell: sh}.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("bash 执行失败:%v", err)
	}
	if strings.TrimSpace(out) != "yes" {
		t.Fatalf("没传 env 时应继承进程环境,拿到 %q", out)
	}
}
