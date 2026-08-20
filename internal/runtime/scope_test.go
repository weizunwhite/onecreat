package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"reasonix/internal/workspace"
)

// recorder collects release order so the tests can assert on it.
type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) mark(name string) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.order = append(r.order, name)
	}
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func mustWS(t *testing.T, dir string) workspace.Context {
	t.Helper()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCloseOrderIsChildrenThenReverseRegistration pins the ordering contract:
// children unwind before the parent's own resources, and within one scope the
// newest resource is released first (so it may depend on older ones).
func TestCloseOrderIsChildrenThenReverseRegistration(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	p.Defer("process-1", r.mark("process-1"))
	p.Defer("process-2", r.mark("process-2"))

	w := p.OpenWorkspace(mustWS(t, t.TempDir()))
	w.Defer("workspace-1", r.mark("workspace-1"))
	w.Defer("workspace-2", r.mark("workspace-2"))

	s := w.NewSession("s")
	s.Defer("session-1", r.mark("session-1"))

	turn := s.BeginTurn()
	turn.Defer("turn-1", r.mark("turn-1"))

	p.Close()

	want := []string{
		"turn-1",
		"session-1",
		"workspace-2", "workspace-1",
		"process-2", "process-1",
	}
	if got := r.got(); !equal(got, want) {
		t.Fatalf("release order = %v, want %v", got, want)
	}
}

// TestSessionCloseLeavesWorkspaceResourcesAlone is Plan 03's headline invariant
// and the concrete bug it exists to prevent: today every runtime chains the
// CodeGraph daemon and the LSP manager into the *session's* cleanup, so closing
// one desktop tab stops the daemon a second tab on the same project is using.
func TestSessionCloseLeavesWorkspaceResourcesAlone(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	defer p.Close()

	w := p.OpenWorkspace(mustWS(t, t.TempDir()))
	w.Defer("codegraph", r.mark("codegraph"))
	w.Defer("lsp", r.mark("lsp"))

	first := w.NewSession("tab1")
	first.Defer("tab1-mcp-overlay", r.mark("tab1-mcp-overlay"))
	second := w.NewSession("tab2")

	first.Close()

	if got := r.got(); !equal(got, []string{"tab1-mcp-overlay"}) {
		t.Fatalf("closing one session released %v — workspace resources must survive", got)
	}
	if w.Closed() {
		t.Fatal("workspace closed when one of its sessions did")
	}
	if second.Closed() {
		t.Fatal("closing one session closed a sibling session")
	}
	if err := second.Context().Err(); err != nil {
		t.Fatalf("sibling session's context was cancelled: %v", err)
	}
	if got := w.Sessions(); got != 1 {
		t.Fatalf("workspace session count = %d, want 1", got)
	}
}

// TestTurnCancelDoesNotCloseSession is the other half: a cancelled turn releases
// only the turn.
func TestTurnCancelDoesNotCloseSession(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	defer p.Close()
	w := p.OpenWorkspace(workspace.Context{})
	s := w.NewSession("s")
	s.Defer("session-resource", r.mark("session-resource"))

	turn := s.BeginTurn()
	turn.Defer("turn-resource", r.mark("turn-resource"))
	turnCtx := turn.Context()

	turn.Cancel()

	if got := r.got(); !equal(got, []string{"turn-resource"}) {
		t.Fatalf("turn cancel released %v, want only the turn's resource", got)
	}
	if turnCtx.Err() == nil {
		t.Fatal("cancelled turn's context should be done")
	}
	if s.Closed() || s.Context().Err() != nil {
		t.Fatal("cancelling a turn must not close or cancel its session")
	}
	if s.CurrentTurn() != nil {
		t.Fatal("session still reports a current turn after cancel")
	}
	// The session must be able to run another turn.
	next := s.BeginTurn()
	if next.Context().Err() != nil {
		t.Fatal("a new turn after a cancelled one should be live")
	}
}

// TestSessionCloseEndsItsTurn is the containment rule in the other direction: a
// turn never outlives its session.
func TestSessionCloseEndsItsTurn(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	s := p.OpenWorkspace(workspace.Context{}).NewSession("s")
	turn := s.BeginTurn()

	s.Close()

	if !turn.Closed() || turn.Context().Err() == nil {
		t.Fatal("closing a session must end its running turn")
	}
}

// TestBeginTurnEndsThePreviousOne keeps a session to one turn at a time, so a
// caller cannot strand the previous turn's context.
func TestBeginTurnEndsThePreviousOne(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	s := p.OpenWorkspace(workspace.Context{}).NewSession("s")

	first := s.BeginTurn()
	second := s.BeginTurn()

	if !first.Closed() {
		t.Fatal("starting a turn should end the previous one")
	}
	if second.Closed() {
		t.Fatal("the new turn should be live")
	}
	if s.CurrentTurn() != second {
		t.Fatal("session's current turn should be the new one")
	}
}

// TestWorkspaceIsSharedByRoot proves the sharing rule: same project → same
// scope, different projects → different scopes.
func TestWorkspaceIsSharedByRoot(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	a, b := mustWS(t, t.TempDir()), mustWS(t, t.TempDir())

	w1 := p.OpenWorkspace(a)
	w2 := p.OpenWorkspace(a)
	other := p.OpenWorkspace(b)

	if w1 != w2 {
		t.Fatal("two opens of the same root must share one workspace scope")
	}
	if w1.Holders() != 2 {
		t.Fatalf("holders = %d, want 2", w1.Holders())
	}
	if other == w1 {
		t.Fatal("different roots must not share a workspace scope")
	}
	if got := len(p.Workspaces()); got != 2 {
		t.Fatalf("open workspaces = %d, want 2", got)
	}
}

// TestWorkspaceClosesOnLastRelease pins refcounted release: shared resources go
// away exactly once, when nobody is holding the workspace any more.
func TestWorkspaceClosesOnLastRelease(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	defer p.Close()
	root := mustWS(t, t.TempDir())

	w := p.OpenWorkspace(root)
	w.Defer("codegraph", r.mark("codegraph"))
	_ = p.OpenWorkspace(root) // a second holder

	if closed := w.Release(); closed {
		t.Fatal("releasing one of two holders must not close the workspace")
	}
	if got := r.got(); len(got) != 0 {
		t.Fatalf("shared resource released too early: %v", got)
	}

	if closed := w.Release(); !closed {
		t.Fatal("releasing the last holder should close the workspace")
	}
	if got := r.got(); !equal(got, []string{"codegraph"}) {
		t.Fatalf("released %v, want the workspace resource", got)
	}
	if !w.Closed() {
		t.Fatal("workspace should be closed after the last release")
	}
	// Deregistered, so a later open starts a fresh scope rather than reusing a corpse.
	if got := p.Workspaces(); len(got) != 0 {
		t.Fatalf("closed workspace still registered: %v", got)
	}
	fresh := p.OpenWorkspace(root)
	if fresh == w || fresh.Closed() {
		t.Fatal("reopening a released root should produce a live scope")
	}
}

// TestReleaseIsIdempotent guards against an over-release taking the workspace
// down while another holder still needs it.
func TestReleaseIsIdempotent(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	w := p.OpenWorkspace(workspace.Context{})
	if !w.Release() {
		t.Fatal("first release should close a single-holder workspace")
	}
	if w.Release() {
		t.Fatal("releasing again should not report a second close")
	}
}

// TestCloseIsIdempotentAndPanicSafe: shutting down twice is a no-op, and one
// misbehaving resource does not abort the rest of the unwind.
func TestCloseIsIdempotentAndPanicSafe(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	p.Defer("first", r.mark("first"))
	p.Defer("boom", func() { panic("resource exploded") })
	p.Defer("last", r.mark("last"))

	p.Close()
	p.Close()

	if got := r.got(); !equal(got, []string{"last", "first"}) {
		t.Fatalf("release order = %v — a panicking closer must not abort the rest", got)
	}
}

// TestDeferAfterCloseReleasesImmediately: a resource that arrives after its
// lifetime ended has no owner left, so it must not simply leak.
func TestDeferAfterCloseReleasesImmediately(t *testing.T) {
	var r recorder
	p := NewProcess(context.Background())
	p.Close()
	p.Defer("late", r.mark("late"))
	if got := r.got(); !equal(got, []string{"late"}) {
		t.Fatalf("late Defer = %v, want immediate release", got)
	}
}

// TestChildOpenedDuringShutdownIsClosed: opening a scope under a parent that is
// already closing must not produce something that outlives it.
func TestChildOpenedDuringShutdownIsClosed(t *testing.T) {
	p := NewProcess(context.Background())
	p.Close()

	w := p.OpenWorkspace(workspace.Context{})
	if !w.Closed() {
		t.Fatal("workspace opened on a closed process should be closed")
	}
	s := w.NewSession("s")
	if !s.Closed() {
		t.Fatal("session opened on a closed workspace should be closed")
	}
	if turn := s.BeginTurn(); !turn.Closed() {
		t.Fatal("turn opened on a closed session should be closed")
	}
}

// TestContextsCancelDownwardsOnly: a parent's cancellation reaches children;
// closing a child never disturbs the parent.
func TestContextsCancelDownwardsOnly(t *testing.T) {
	p := NewProcess(context.Background())
	w := p.OpenWorkspace(workspace.Context{})
	s := w.NewSession("s")
	turn := s.BeginTurn()

	turn.End()
	if s.Context().Err() != nil || w.Context().Err() != nil || p.Context().Err() != nil {
		t.Fatal("ending a turn cancelled an enclosing scope")
	}
	s.Close()
	if w.Context().Err() != nil || p.Context().Err() != nil {
		t.Fatal("closing a session cancelled an enclosing scope")
	}

	p.Close()
	if w.Context().Err() == nil {
		t.Fatal("closing the process should cancel its workspaces")
	}
}

// TestScopeNamesDescribeTheLevel keeps diagnostics legible: a leaked resource
// should say which lifetime it belonged to.
func TestScopeNamesDescribeTheLevel(t *testing.T) {
	dir := t.TempDir()
	p := NewProcess(context.Background())
	defer p.Close()
	w := p.OpenWorkspace(mustWS(t, dir))
	s := w.NewSession("tab1")
	turn := s.BeginTurn()

	if p.Name() != "process" {
		t.Errorf("process name = %q", p.Name())
	}
	if !strings.Contains(w.Name(), dir) {
		t.Errorf("workspace name %q should name its root", w.Name())
	}
	if !strings.Contains(s.Name(), "tab1") {
		t.Errorf("session name %q should name the session", s.Name())
	}
	if !strings.Contains(turn.Name(), "tab1#1") {
		t.Errorf("turn name %q should name session and turn number", turn.Name())
	}
	// The zero workspace is a real, nameable root.
	if got := p.OpenWorkspace(workspace.Context{}).Name(); !strings.Contains(got, "process-cwd") {
		t.Errorf("zero-workspace scope name = %q", got)
	}
}

// TestGeneratedSessionIDsAreUnique covers the unnamed-session path.
func TestGeneratedSessionIDsAreUnique(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	w := p.OpenWorkspace(workspace.Context{})
	a, b := w.NewSession(""), w.NewSession("")
	if a.ID() == "" || a.ID() == b.ID() {
		t.Fatalf("generated session ids not unique: %q / %q", a.ID(), b.ID())
	}
}

// TestConcurrentScopeLifecyclesAreRaceFree hammers the tree from several
// goroutines; under -race this is the guard that the ownership bookkeeping is
// actually locked.
func TestConcurrentScopeLifecyclesAreRaceFree(t *testing.T) {
	p := NewProcess(context.Background())
	defer p.Close()
	roots := []workspace.Context{
		mustWS(t, t.TempDir()),
		mustWS(t, t.TempDir()),
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				w := p.OpenWorkspace(roots[(i+j)%len(roots)])
				w.Defer("res", func() {})
				s := w.NewSession("")
				s.Defer("sres", func() {})
				turn := s.BeginTurn()
				turn.Defer("tres", func() {})
				_ = w.Sessions()
				_ = p.Workspaces()
				turn.Cancel()
				s.Close()
				w.Release()
			}
		}(i)
	}
	wg.Wait()

	// Every OpenWorkspace was matched by a Release, so nothing should be left open.
	if got := p.Workspaces(); len(got) != 0 {
		t.Fatalf("workspaces still open after balanced release: %v", got)
	}
}
