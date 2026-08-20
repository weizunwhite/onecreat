// Package workspace makes "which project directory is this runtime working in"
// an explicit, passed-around value instead of a property of the OS process.
//
// Historically OneCreat modelled a workspace as the process working directory:
// opening a project called os.Chdir, and every workspace-relative path
// (onecreat.toml, .mcp.json, AGENTS.md, skills, the file tools, bash) resolved
// against it implicitly. That makes a workspace process-global, so two runtimes
// cannot hold different workspaces at the same time — the desktop had to refuse
// switching projects while more than one tab was open, because a background
// tab's in-flight bash and relative file writes would silently land in the
// newly-chdir'd directory.
//
// A Context is that root made explicit. It is immutable, always absolute, and
// carries no other state: everything a workspace owns hangs off the root, and
// resolving a relative path against it is the only operation it needs.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// Context is one workspace's identity: an absolute root directory.
//
// The zero Context is "no explicit workspace" — Root reports "" and Resolve
// returns paths unchanged, which reproduces the historical process-cwd
// behaviour byte for byte. That keeps callers that have not been threaded
// through yet (and the CLI, which legitimately *is* process-cwd scoped)
// working without a special case.
//
// The root is unexported on purpose. A workspace whose root can be reassigned,
// or set to a relative path, is the bug this type exists to remove: construct
// one with New and pass it down.
type Context struct {
	root string
}

// New resolves root to an absolute path and verifies it is an existing
// directory. An empty root yields the zero Context (process-cwd semantics), not
// an error, so "no workspace specified" stays expressible.
func New(root string) (Context, error) {
	if root == "" {
		return Context{}, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Context{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Context{}, err
	}
	if !info.IsDir() {
		return Context{}, fmt.Errorf("workspace root %s is not a directory", abs)
	}
	return Context{root: abs}, nil
}

// Current returns a Context for the process working directory. It is the
// startup-compatibility path: a frontend that has no explicit workspace yet
// (the CLI, or the desktop before it has read the remembered folder) starts
// here, and from then on passes the Context down instead of relying on cwd.
func Current() (Context, error) {
	wd, err := os.Getwd()
	if err != nil {
		return Context{}, err
	}
	return Context{root: wd}, nil
}

// Root is the absolute workspace directory, or "" for the zero Context.
func (c Context) Root() string { return c.root }

// IsZero reports whether this is the "no explicit workspace" Context.
func (c Context) IsZero() bool { return c.root == "" }

// Resolve maps a workspace-relative path onto the root. An absolute p is
// returned unchanged (an explicit absolute path is honoured verbatim — write
// confinement, not this, enforces the workspace boundary), and so is any p when
// the Context is zero. An empty or "." p resolves to the root itself.
func (c Context) Resolve(p string) string {
	if c.root == "" {
		return p
	}
	if p == "" || p == "." {
		return c.root
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.root, p)
}

// RootOr returns the workspace root, falling back to fallback when this is the
// zero Context. Call sites that need a concrete directory (a subprocess's Dir,
// an index root) use it to spell the fallback explicitly instead of silently
// inheriting the process cwd.
func (c Context) RootOr(fallback string) string {
	if c.root == "" {
		return fallback
	}
	return c.root
}

// String makes a Context readable in logs and test failures.
func (c Context) String() string {
	if c.root == "" {
		return "workspace(process-cwd)"
	}
	return "workspace(" + c.root + ")"
}
