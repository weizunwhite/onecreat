package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// 这一组是 Plan 01 的特征测试:两个工作区必须能同时存在,各自的工具只认自己的 root,
// 且谁都不依赖进程 cwd —— 每个用例都会把进程 cwd 挪到第三个目录再断言,所以任何
// "其实还是靠 os.Chdir" 的实现都会红。

// wsTools indexes ConfineWorkspace's result by tool name for one workspace root.
func wsTools(t *testing.T, root string) map[string]tool.Tool {
	t.Helper()
	out := map[string]tool.Tool{}
	for _, tl := range ConfineWorkspace(root, []string{root}, sandbox.Spec{Mode: "off"}, SearchSpec{}) {
		out[tl.Name()] = tl
	}
	return out
}

// tryTool runs a tool and hands back its error instead of failing the test —
// several assertions below are about a call that must be refused. (The package's
// runTool helper fatals on error, which is the wrong shape here.)
func tryTool(t *testing.T, tl tool.Tool, args map[string]any) (string, error) {
	t.Helper()
	return tl.Execute(context.Background(), argsJSON(t, args))
}

// elsewhere moves the process working directory to a third, unrelated directory
// for the duration of the test. Every assertion below then holds *despite* the
// process cwd, which is the whole point of an explicit workspace root.
func elsewhere(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

func TestWorkspaceToolsResolveAgainstTheirOwnRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "a.txt"), []byte("from A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "b.txt"), []byte("from B"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere(t)

	toolsA, toolsB := wsTools(t, a), wsTools(t, b)

	// Same relative path, two workspaces, two different files.
	gotA, err := tryTool(t, toolsA["read_file"], map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatalf("A read a.txt: %v", err)
	}
	if !strings.Contains(gotA, "from A") {
		t.Errorf("A read_file(a.txt) = %q, want A's copy", gotA)
	}
	gotB, err := tryTool(t, toolsB["read_file"], map[string]any{"path": "b.txt"})
	if err != nil {
		t.Fatalf("B read b.txt: %v", err)
	}
	if !strings.Contains(gotB, "from B") {
		t.Errorf("B read_file(b.txt) = %q, want B's copy", gotB)
	}

	// B must not be able to reach A's file by its relative name.
	if out, err := tryTool(t, toolsB["read_file"], map[string]any{"path": "a.txt"}); err == nil {
		t.Errorf("B read_file(a.txt) succeeded (%q) — B reached into A", out)
	}
	if out, err := tryTool(t, toolsA["read_file"], map[string]any{"path": "b.txt"}); err == nil {
		t.Errorf("A read_file(b.txt) succeeded (%q) — A reached into B", out)
	}
}

func TestWorkspaceWritesLandInTheirOwnRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	elsewhere(t)

	toolsA, toolsB := wsTools(t, a), wsTools(t, b)

	if _, err := tryTool(t, toolsA["write_file"], map[string]any{"path": "out.txt", "content": "A wrote this"}); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if _, err := tryTool(t, toolsB["write_file"], map[string]any{"path": "out.txt", "content": "B wrote this"}); err != nil {
		t.Fatalf("B write: %v", err)
	}

	assertFile(t, filepath.Join(a, "out.txt"), "A wrote this")
	assertFile(t, filepath.Join(b, "out.txt"), "B wrote this")

	// Nothing landed in the process working directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "out.txt")); err == nil {
		t.Fatal("a relative write landed in the process working directory")
	}
}

// TestWorkspaceWritesAreConfinedToTheirOwnRoot proves the write confiner still
// bites per workspace: an explicit absolute path into the *other* workspace is
// refused, so an escaping write cannot cross workspaces either.
func TestWorkspaceWritesAreConfinedToTheirOwnRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	elsewhere(t)

	toolsA := wsTools(t, a)
	target := filepath.Join(b, "stolen.txt")
	if _, err := tryTool(t, toolsA["write_file"], map[string]any{"path": target, "content": "x"}); err == nil {
		t.Fatal("A wrote into B's root — write confinement is not per-workspace")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("A's write created a file inside B")
	}
}

func TestWorkspaceBashRunsInItsOwnRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 POSIX pwd 断言工作目录,Windows 上 shell 解析不同")
	}
	a, b := t.TempDir(), t.TempDir()
	elsewhere(t)

	toolsA, toolsB := wsTools(t, a), wsTools(t, b)

	outA, err := tryTool(t, toolsA["bash"], map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("A bash: %v", err)
	}
	outB, err := tryTool(t, toolsB["bash"], map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("B bash: %v", err)
	}
	if !samePath(t, outA, a) {
		t.Errorf("A bash pwd = %q, want %s", strings.TrimSpace(outA), a)
	}
	if !samePath(t, outB, b) {
		t.Errorf("B bash pwd = %q, want %s", strings.TrimSpace(outB), b)
	}
}

// TestWorkspaceToolsIgnoreLaterChdir is the核心验收 in miniature: once built, a
// workspace's tools keep their root even if something else moves the process
// working directory afterwards — which is exactly what "switching the active UI
// workspace must not change a background session's root" needs.
func TestWorkspaceToolsIgnoreLaterChdir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	toolsA := wsTools(t, a)

	// Simulate the frontend switching the active project to B the old way.
	t.Chdir(b)

	if _, err := tryTool(t, toolsA["write_file"], map[string]any{"path": "still-a.txt", "content": "A"}); err != nil {
		t.Fatalf("A write after chdir: %v", err)
	}
	assertFile(t, filepath.Join(a, "still-a.txt"), "A")
	if _, err := os.Stat(filepath.Join(b, "still-a.txt")); err == nil {
		t.Fatal("A's write followed the process cwd into B")
	}
}

// TestZeroWorkspaceKeepsProcessCwdBehaviour pins the compatibility promise: with
// no workspace root, ConfineWorkspace's tools behave exactly like the
// process-cwd built-ins the CLI has always used.
func TestZeroWorkspaceKeepsProcessCwdBehaviour(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	tools := map[string]tool.Tool{}
	for _, tl := range ConfineWorkspace("", []string{dir}, sandbox.Spec{Mode: "off"}, SearchSpec{}) {
		tools[tl.Name()] = tl
	}
	if _, err := tryTool(t, tools["write_file"], map[string]any{"path": "cwd.txt", "content": "here"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertFile(t, filepath.Join(dir, "cwd.txt"), "here")
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}

// samePath compares a tool's textual output against a directory, tolerating the
// macOS /var → /private/var symlink and any surrounding output framing.
func samePath(t *testing.T, out, dir string) bool {
	t.Helper()
	real := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		real = r
	}
	return strings.Contains(out, dir) || strings.Contains(out, real)
}
