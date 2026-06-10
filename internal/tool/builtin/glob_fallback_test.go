package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C4 回归:Workspace 绑定实例(workDir 非空)下,用裸文件名 glob 一个深层目录里的文件,
// 必须命中——「只知文件名时全树搜索」的回退过去因 resolveIn 把 pattern 变绝对路径而失效。
func TestGlobBareFilenameWorkspaceRecursiveFallback(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "src", "module")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "main.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := globTool{workDir: dir}
	out := runTool(t, g, map[string]any{"pattern": "main.py"})
	if strings.Contains(out, "no matches") || !strings.Contains(out, "main.py") {
		t.Fatalf("workspace-bound glob of bare filename should find the deep file, got: %q", out)
	}
}

// 同时确认 CLI 模式(workDir 为空、cwd 相对)的裸文件名回退仍然工作。
func TestGlobBareFilenameCwdRecursiveFallback(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "needle.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	out := runTool(t, globTool{}, map[string]any{"pattern": "needle.txt"})
	if strings.Contains(out, "no matches") || !strings.Contains(out, "needle.txt") {
		t.Fatalf("cwd glob of bare filename should find the deep file, got: %q", out)
	}
}
