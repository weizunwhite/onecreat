package boot

import (
	"context"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/workspace"
)

// lspProject is projectConfig with the language-server layer on. LSP is the
// cheapest observable workspace-scoped service: the manager resolves its servers
// on PATH and spawns them lazily on first query, so a test machine with no
// language server installed still gets a real manager to reason about.
func lspProject(t *testing.T, dir string) workspace.Context {
	t.Helper()
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
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func openRoots(t *testing.T, f *Factory) []string {
	t.Helper()
	return f.Process().Workspaces()
}

// TestBuildSharesWorkspaceServicesAcrossSessions is Plan 05's headline
// acceptance, stated the way the roadmap states it: two tabs open on one
// project, and closing the first must not stop the services the second is still
// using.
//
// Before Plan 05 every resource hung off the session's cleanup chain, so
// ctrlA.Close() stopped the project's language servers and its CodeGraph daemon
// out from under ctrlB.
func TestBuildSharesWorkspaceServicesAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	t.Chdir(t.TempDir()) // the process stands in neither project

	f := NewFactory(context.Background())
	defer f.Close()

	ctrlA, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	ctrlB, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(B): %v", err)
	}

	// 资源按作用域只创建一次:两个会话、一个工作区。
	if got := openRoots(t, f); len(got) != 1 || got[0] != ws.Root() {
		t.Fatalf("两个同项目会话应共用一个工作区作用域,实际 %v", got)
	}

	ctrlA.Close()
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("关掉一个会话不该关掉另一个还在用的工作区,实际 %v", got)
	}

	ctrlB.Close()
	if got := openRoots(t, f); len(got) != 0 {
		t.Fatalf("最后一个会话关闭后工作区应释放,实际仍开着 %v", got)
	}
}

// TestControllerCloseIsIdempotentForTheWorkspace: an over-release would report a
// second "last holder" and tear down services that a *live* session is using.
// Controller.Close is reachable more than once (shutdown after an explicit
// close), so the hold must be released exactly once.
func TestControllerCloseIsIdempotentForTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	t.Chdir(t.TempDir())

	f := NewFactory(context.Background())
	defer f.Close()

	ctrlA, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	ctrlB, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(B): %v", err)
	}
	defer ctrlB.Close()

	ctrlA.Close()
	ctrlA.Close()
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("重复 Close 把工作区从还活着的会话手里抢走了,实际 %v", got)
	}
}

// TestOpenWorkspaceStartsServicesOnce pins the "只创建一次" half at the factory
// level: a second opener gets the *same* manager, not a second set of servers.
func TestOpenWorkspaceStartsServicesOnce(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	cfg, err := config.LoadIn(ws)
	if err != nil {
		t.Fatal(err)
	}

	f := NewFactory(context.Background())
	defer f.Close()

	h1 := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir})
	h2 := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir})
	if h1.Services().lsp == nil {
		t.Fatal("LSP 开启时工作区应持有一个 manager")
	}
	if h1.Services() != h2.Services() {
		t.Fatal("同一个项目的两次 open 应拿到同一份共享服务")
	}
	h1.Release()
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("还有一个持有者时工作区不该关闭,实际 %v", got)
	}
	h2.Release()
	if got := openRoots(t, f); len(got) != 0 {
		t.Fatalf("最后一个持有者释放后工作区应关闭,实际 %v", got)
	}

	// 释放后再 open 是一次全新的启动,不能拿到已关闭作用域上的旧服务。
	h3 := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir})
	defer h3.Release()
	if h3.Services() == h1.Services() {
		t.Fatal("工作区关闭后重开应重新启动服务,不该复用已关闭作用域上的那份")
	}
}

// TestHoldKeepsWorkspaceOpenAcrossSessionSwap is the desktop rebuild path in
// miniature: close the session, build the replacement. Without a hold the
// refcount touches zero in between and the project's services are stopped and
// immediately restarted.
func TestHoldKeepsWorkspaceOpenAcrossSessionSwap(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	cfg, err := config.LoadIn(ws)
	if err != nil {
		t.Fatal(err)
	}

	f := NewFactory(context.Background())
	defer f.Close()

	session := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir})
	svc := session.Services()

	hold := f.Hold(ws)
	if hold.Services() != nil {
		t.Fatal("Hold 是生命周期引用,不该发放服务")
	}

	session.Release() // 旧会话走了
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("hold 期间工作区必须保持打开,实际 %v", got)
	}

	replacement := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir}) // 新会话接上
	if replacement.Services() != svc {
		t.Fatal("跨重建的工作区服务被停掉重启了 —— hold 没有生效")
	}
	hold.Release()
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("新会话还在用,释放 hold 不该关掉工作区,实际 %v", got)
	}
	replacement.Release()
	if got := openRoots(t, f); len(got) != 0 {
		t.Fatalf("全部释放后工作区应关闭,实际 %v", got)
	}
}

// TestFactoryCloseReleasesEverything: app shutdown must leave nothing running,
// including a workspace whose sessions were never closed.
func TestFactoryCloseReleasesEverything(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	cfg, err := config.LoadIn(ws)
	if err != nil {
		t.Fatal(err)
	}

	f := NewFactory(context.Background())
	h := f.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: dir})
	f.Close()
	if got := openRoots(t, f); len(got) != 0 {
		t.Fatalf("Factory.Close 后不该还有开着的工作区,实际 %v", got)
	}
	// 迟到的 Release 必须是无害的,而不是 panic 或二次关闭。
	h.Release()
	h.Release()
}

// TestBuildWithoutFactoryStillCleansUp: the CLI, headless runs and ACP sessions
// pass no Factory. They get a private one, closed with the controller — the
// pre-Plan-05 behaviour, with no shared state left behind.
func TestBuildWithoutFactoryStillCleansUp(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	t.Chdir(t.TempDir())

	ctrl, err := Build(context.Background(), Options{Workspace: ws})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctrl.Close()
	ctrl.Close() // 二次 Close 不能 panic
}

// TestSessionsAreScopedUnderTheWorkspace pins the ownership direction the whole
// plan is about: a session lives *under* a workspace. Ending one drops a session
// from the project and nothing else; the project itself goes away only when the
// thing that opened it lets go.
func TestSessionsAreScopedUnderTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws := lspProject(t, dir)
	t.Chdir(t.TempDir())

	f := NewFactory(context.Background())
	defer f.Close()

	// A frontend-style hold, so the project's lifetime is visibly independent of
	// any single session's.
	hold := f.Hold(ws)
	scope := hold.Scope()

	ctrlA, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	ctrlB, err := Build(context.Background(), Options{Workspace: ws, Factory: f})
	if err != nil {
		t.Fatalf("Build(B): %v", err)
	}
	if n := scope.Sessions(); n != 2 {
		t.Fatalf("两个 controller 应是这个工作区下的两个 session,实际 %d", n)
	}

	ctrlA.Close()
	if n := scope.Sessions(); n != 1 {
		t.Fatalf("关掉一个会话后应剩 1 个,实际 %d", n)
	}
	ctrlB.Close()
	if n := scope.Sessions(); n != 0 {
		t.Fatalf("会话全部关闭后应剩 0 个,实际 %d", n)
	}
	// 会话全走了,但持有者还在:工作区不关。
	if got := openRoots(t, f); len(got) != 1 {
		t.Fatalf("持有者还在时工作区不该关闭,实际 %v", got)
	}
	hold.Release()
	if got := openRoots(t, f); len(got) != 0 {
		t.Fatalf("持有者释放后工作区应关闭,实际 %v", got)
	}
}
