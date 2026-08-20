package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewMakesRootAbsolute(t *testing.T) {
	dir := t.TempDir()
	// A relative root must not survive construction: the whole point is that the
	// root no longer depends on where the process happens to stand.
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("no relative path from cwd to tempdir: %v", err)
	}
	ws, err := New(rel)
	if err != nil {
		t.Fatalf("New(%q): %v", rel, err)
	}
	if !filepath.IsAbs(ws.Root()) {
		t.Fatalf("Root() = %q, want absolute", ws.Root())
	}
	if got, want := evalSymlinks(t, ws.Root()), evalSymlinks(t, dir); got != want {
		t.Fatalf("Root() = %q, want %q", got, want)
	}
}

func TestNewRejectsNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(file); err == nil {
		t.Fatal("New(file) should fail — a workspace root must be a directory")
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("New(missing) should fail")
	}
}

func TestZeroContextKeepsProcessCwdSemantics(t *testing.T) {
	var zero Context
	if !zero.IsZero() || zero.Root() != "" {
		t.Fatalf("zero Context = %+v", zero)
	}
	// The zero Context must be a no-op resolver, so callers not yet threaded
	// through behave exactly as they did before.
	for _, p := range []string{"", ".", "rel/path.txt", filepath.Join(string(filepath.Separator), "abs", "path")} {
		if got := zero.Resolve(p); got != p {
			t.Errorf("zero.Resolve(%q) = %q, want unchanged", p, got)
		}
	}
	if got := zero.RootOr("fallback"); got != "fallback" {
		t.Errorf("zero.RootOr = %q, want fallback", got)
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	ws, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(string(filepath.Separator), "elsewhere", "x.txt")
	cases := []struct{ in, want string }{
		{"", ws.Root()},
		{".", ws.Root()},
		{"a.txt", filepath.Join(ws.Root(), "a.txt")},
		{filepath.Join("sub", "a.txt"), filepath.Join(ws.Root(), "sub", "a.txt")},
		{abs, abs},
	}
	for _, tc := range cases {
		if got := ws.Resolve(tc.in); got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := ws.RootOr("fallback"); got != ws.Root() {
		t.Errorf("RootOr = %q, want %q", got, ws.Root())
	}
}

// TestTwoContextsAreIndependent is the shape Plan 01 exists to enable: two
// workspaces alive at once, neither able to disturb the other, with no process
// state involved.
func TestTwoContextsAreIndependent(t *testing.T) {
	a, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.Root() == b.Root() {
		t.Fatal("distinct temp dirs produced the same root")
	}
	if got := a.Resolve("a.txt"); got != filepath.Join(a.Root(), "a.txt") {
		t.Errorf("a.Resolve leaked: %q", got)
	}
	if got := b.Resolve("b.txt"); got != filepath.Join(b.Root(), "b.txt") {
		t.Errorf("b.Resolve leaked: %q", got)
	}
}

func TestCurrentIsAbsolute(t *testing.T) {
	ws, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if ws.IsZero() || !filepath.IsAbs(ws.Root()) {
		t.Fatalf("Current() = %v", ws)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func evalSymlinks(t *testing.T, p string) string {
	t.Helper()
	// macOS hands out /var/... temp dirs that are symlinks to /private/var/...;
	// compare resolved paths so the assertion is about identity, not spelling.
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}
