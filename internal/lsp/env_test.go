package lsp

import (
	"os"
	"strings"
	"testing"
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

// C1:语言服务器的子进程环境按工作区来。`.env` 不再 os.Setenv 之后,两个工作区的
// 同名变量只能靠各自的 baseEnv 区分 —— 共用进程环境就又变成"先启动的那个赢"。
func TestChildEnvUsesTheWorkspaceBaseNotTheProcessEnvironment(t *testing.T) {
	t.Setenv("C1_LSP_SHARED", "from-process")

	a := NewManagerWithEnv(t.TempDir(), nil, []string{"C1_LSP_KEY=ws-A"}).clientEnv(ServerSpec{})
	b := NewManagerWithEnv(t.TempDir(), nil, []string{"C1_LSP_KEY=ws-B"}).clientEnv(ServerSpec{})

	if got, _ := lookupIn(a, "C1_LSP_KEY"); got != "ws-A" {
		t.Fatalf("工作区 A 的语言服务器拿到 %q", got)
	}
	if got, _ := lookupIn(b, "C1_LSP_KEY"); got != "ws-B" {
		t.Fatalf("工作区 B 的语言服务器拿到 %q —— 两个工作区共用了一份环境", got)
	}
	// 显式 base 就是全部:没被列进来的进程变量不该混进去。
	if _, ok := lookupIn(a, "C1_LSP_SHARED"); ok {
		t.Fatal("显式 baseEnv 之外的进程变量漏进了子进程环境")
	}
}

// baseEnv 为 nil 时继承进程环境:没有工作区的调用方(CLI、测试)行为不变。
func TestChildEnvFallsBackToTheProcessEnvironment(t *testing.T) {
	t.Setenv("C1_LSP_INHERIT", "yes")
	m := NewManager(t.TempDir(), nil)
	got, ok := lookupIn(m.clientEnv(ServerSpec{}), "C1_LSP_INHERIT")
	if !ok || got != "yes" {
		t.Fatalf("nil baseEnv 应继承进程环境,拿到 %q(存在=%v)", got, ok)
	}
	if len(m.clientEnv(ServerSpec{})) != len(os.Environ()) {
		t.Fatal("nil baseEnv 不该增删进程环境里的键")
	}
}

// spec 的 Env 叠在 base 之上:per-server 覆盖仍然生效。
func TestSpecEnvOverridesTheWorkspaceBase(t *testing.T) {
	m := NewManagerWithEnv(t.TempDir(), nil, []string{"C1_LSP_KEY=base"})
	got, _ := lookupIn(m.clientEnv(ServerSpec{Env: map[string]string{"C1_LSP_KEY": "spec"}}), "C1_LSP_KEY")
	if got != "spec" {
		t.Fatalf("spec.Env 应覆盖 base,拿到 %q", got)
	}
}

// NewManager 保持 nil base:没有工作区的调用方一个字节都不变。
func TestNewManagerKeepsTheProcessEnvironment(t *testing.T) {
	if NewManager(t.TempDir(), nil).baseEnv != nil {
		t.Fatal("NewManager 应保持 nil(继承进程环境)")
	}
}
