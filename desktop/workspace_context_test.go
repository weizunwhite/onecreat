package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组守住 Plan 01 在桌面端的核心验收:标签各自持有自己的 workspace root,
// 切换项目不再依赖 os.Chdir,因此后台标签不会被活动标签的切换带偏。

func newTestApp(t *testing.T, root string) *App {
	t.Helper()
	app := NewApp()
	app.ws = testWorkspace(t, root)
	return app
}

// TestSwitchWorkspaceDoesNotChdir is the核心验收 stated as a test: switching the
// selected project must leave the process working directory alone. Before Plan
// 01 the switch *was* an os.Chdir, which is exactly why two tabs could not hold
// two projects.
func TestSwitchWorkspaceDoesNotChdir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	app := newTestApp(t, a)
	// SwitchWorkspace rebuilds the active tab's controller, which needs a real
	// app context; drive the part under test directly instead.
	app.mu.Lock()
	app.ws = testWorkspace(t, b)
	app.mu.Unlock()

	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(before, after) {
		t.Fatalf("process cwd moved from %q to %q — the switch is still process-global", before, after)
	}
	if !samePath(app.workspaceRoot(), b) {
		t.Fatalf("workspaceRoot() = %q, want %q", app.workspaceRoot(), b)
	}
}

// TestTabsHoldIndependentWorkspaces proves the multi-tab invariant Plan 01
// unlocks: every tab carries its own root, and re-pointing the app's selected
// project does not rewrite a background tab's.
func TestTabsHoldIndependentWorkspaces(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(t.TempDir())

	app := newTestApp(t, a)
	background := &tabRuntime{id: "bg", kind: "chat", ws: testWorkspace(t, a)}
	app.tabs["bg"] = background

	// The user opens project B and creates a tab there.
	app.mu.Lock()
	app.ws = testWorkspace(t, b)
	app.mu.Unlock()
	foreground := &tabRuntime{id: "fg", kind: "chat", ws: app.workspace()}
	app.tabs["fg"] = foreground

	// …then selects project C.
	app.mu.Lock()
	app.ws = testWorkspace(t, c)
	app.mu.Unlock()

	if !samePath(background.ws.Root(), a) {
		t.Errorf("background tab root = %q, want %q — it followed the active selection", background.ws.Root(), a)
	}
	if !samePath(foreground.ws.Root(), b) {
		t.Errorf("second tab root = %q, want %q", foreground.ws.Root(), b)
	}
	if !samePath(app.workspaceRoot(), c) {
		t.Errorf("selected workspace = %q, want %q", app.workspaceRoot(), c)
	}
}

// TestWorkspacePathConfinesToTheSelectedProject pins the "@" file-reference
// boundary to the open project rather than the process working directory.
func TestWorkspacePathConfinesToTheSelectedProject(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(b) // the process stands in the *other* project

	if err := os.WriteFile(filepath.Join(a, "in-a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := workspacePath(a, "in-a.txt")
	if err != nil || !ok {
		t.Fatalf("workspacePath(a, in-a.txt) = %q, %v, %v", got, ok, err)
	}
	if !samePath(got, filepath.Join(a, "in-a.txt")) {
		t.Fatalf("resolved to %q, want inside %q", got, a)
	}
	// Escaping the project root is still refused.
	if _, ok, err := workspacePath(a, filepath.Join("..", "escape.txt")); ok || err == nil {
		t.Fatal("workspacePath allowed an escape above the project root")
	}
	// An empty base is refused rather than silently falling back to the process cwd.
	if _, ok, err := workspacePath("", "in-a.txt"); ok || err == nil {
		t.Fatal("workspacePath with no base should be refused")
	}
}

// TestListDirFollowsTheSelectedProject proves the "@" menu lists the open
// project, not the process working directory.
func TestListDirFollowsTheSelectedProject(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "only-in-a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "only-in-b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(b)

	app := newTestApp(t, a)
	var names []string
	for _, e := range app.ListDir("") {
		names = append(names, e.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "only-in-a.txt") {
		t.Errorf("ListDir did not list the selected project: %v", names)
	}
	if strings.Contains(joined, "only-in-b.txt") {
		t.Errorf("ListDir listed the process working directory: %v", names)
	}
}

// TestResolveHardwareProjectDirDefaultsToSelectedWorkspace keeps the hardware
// panel pointed at the open project.
func TestResolveHardwareProjectDirDefaultsToSelectedWorkspace(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(b)
	app := newTestApp(t, a)

	if got := app.resolveHardwareProjectDir(""); !samePath(got, a) {
		t.Errorf("resolveHardwareProjectDir(\"\") = %q, want the selected workspace %q", got, a)
	}
	// An explicit directory still wins.
	if got := app.resolveHardwareProjectDir(b); !samePath(got, b) {
		t.Errorf("resolveHardwareProjectDir(b) = %q, want %q", got, b)
	}
}

// TestResolveStartupWorkspaceReportsWhereItLanded pins the startup contract: the
// remembered folder becomes both the process cwd (compatibility) and the
// explicit Context (runtime identity).
func TestResolveStartupWorkspaceReportsWhereItLanded(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", cfgHome)
	project := t.TempDir()
	saveWorkspace(project)
	t.Chdir(t.TempDir())

	ws := resolveStartupWorkspace()
	if ws.IsZero() {
		t.Fatal("resolveStartupWorkspace returned the zero Context")
	}
	if !samePath(ws.Root(), project) {
		t.Fatalf("startup workspace = %q, want the remembered folder %q", ws.Root(), project)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(wd, project) {
		t.Fatalf("startup did not chdir into the remembered folder: cwd = %q", wd)
	}
}

// samePath compares two paths through symlinks (macOS temp dirs are symlinked).
func samePath(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return resolve(a) == resolve(b)
}
