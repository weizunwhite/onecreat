package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// assemblyPackages are the building blocks of a OneCreat runtime. A transport
// that imports these is assembling its own stack rather than asking the
// composition root for one — which is exactly how the ACP path drifted away from
// boot.Build (its own gateway rewriting, its own model-privacy prompt, its own
// planner gating, each a hand-kept copy of a billing- or IP-sensitive rule).
var assemblyPackages = []string{
	"reasonix/internal/agent",
	"reasonix/internal/permission",
	"reasonix/internal/plugin",
	"reasonix/internal/tool",
	"reasonix/internal/tool/builtin",
	"reasonix/internal/provider",
	"reasonix/internal/memory",
	"reasonix/internal/skill",
	"reasonix/internal/lsp",
	"reasonix/internal/codegraph",
	"reasonix/internal/hook",
	"reasonix/internal/jobs",
}

func acpImports(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "acp.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse acp.go: %v", err)
	}
	out := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out[path] = true
	}
	return out
}

// TestACPDoesNotAssembleItsOwnRuntime is Plan 04's acceptance, stated as a test:
// the ACP transport must not hold a second copy of the assembly. It asks
// boot.Build for a controller and supplies only what is genuinely per-session
// (the client's cwd and its `session/new` MCP servers).
func TestACPDoesNotAssembleItsOwnRuntime(t *testing.T) {
	imports := acpImports(t)
	for _, pkg := range assemblyPackages {
		if imports[pkg] {
			t.Errorf("internal/cli/acp.go imports %s — the ACP transport is assembling its own runtime again; "+
				"add what it needs to boot.Options instead", pkg)
		}
	}
	if !imports["reasonix/internal/boot"] {
		t.Error("internal/cli/acp.go no longer imports boot — it must go through the composition root")
	}
}

// TestACPCallsTheCompositionRoot pins the positive half: the factory's session
// path actually calls boot.Build, rather than merely avoiding the imports.
func TestACPCallsTheCompositionRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "acp.go", nil, 0)
	if err != nil {
		t.Fatalf("parse acp.go: %v", err)
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name == "boot" && sel.Sel.Name == "Build" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Error("internal/cli/acp.go does not call boot.Build — sessions must come from the composition root")
	}
}

// TestACPFileStaysSmall is a coarse but effective drift alarm: the transport is
// flag parsing, startup validation and a call into boot. If it grows back toward
// its pre-Plan-04 size (206 lines, most of it assembly), something is being
// rebuilt here that belongs in the composition root.
func TestACPFileStaysSmall(t *testing.T) {
	src, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatalf("read acp.go: %v", err)
	}
	lines := bytes.Count(src, []byte("\n")) + 1
	const limit = 160
	if lines > limit {
		t.Errorf("internal/cli/acp.go is %d lines (limit %d) — check whether runtime assembly is creeping back in", lines, limit)
	}
}

// TestACPSetsHostProvidesCodeIntel documents, and pins, the one deliberate
// behavioural difference: an editor host already runs its own language servers
// and index, so the agent must not start a second CodeGraph daemon and LSP
// manager inside the session.
func TestACPSetsHostProvidesCodeIntel(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "acp.go", nil, 0)
	if err != nil {
		t.Fatalf("parse acp.go: %v", err)
	}
	var buf strings.Builder
	ast.Inspect(file, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "HostProvidesCodeIntel" {
				if val, ok := kv.Value.(*ast.Ident); ok {
					buf.WriteString(val.Name)
				}
			}
		}
		return true
	})
	if buf.String() != "true" {
		t.Errorf("acp.go should pass HostProvidesCodeIntel: true (got %q) — an editor host provides its own code intelligence", buf.String())
	}
}
