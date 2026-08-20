package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// productPolicyPackages are the packages that implement OneCreat's *product*
// behaviour around a model loop, rather than the loop itself. A05 was exactly
// this: agent.Agent had grown fields for all of them and executed their policy
// inline, which welded the engine to the product — a second engine (dsh) could
// not reuse any of it without reusing agent.Agent.
//
// Plan 08 moved that policy into internal/toolpolicy. The engine now reaches it
// only through the pipeline, so it has no reason to import these directly.
var productPolicyPackages = []string{
	"reasonix/internal/evidence",
	"reasonix/internal/memory",
	"reasonix/internal/diff",
	"reasonix/internal/permission",
	"reasonix/internal/checkpoint",
	"reasonix/internal/hook",
	"reasonix/internal/billing",
}

func packageImports(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			out[path] = append(out[path], name)
		}
	}
	return out
}

// TestEngineDoesNotImportProductPolicy is Plan 08's acceptance stated as a test:
// the model loop must not reach for the product's policy packages. If one shows
// up here again, policy is being implemented in the engine instead of in
// internal/toolpolicy — and whatever was added is unavailable to any other engine.
func TestEngineDoesNotImportProductPolicy(t *testing.T) {
	imports := packageImports(t)
	for _, pkg := range productPolicyPackages {
		if files := imports[pkg]; len(files) > 0 {
			t.Errorf("internal/agent imports %s (in %v) — that is product policy; "+
				"put it in internal/toolpolicy so a second engine can share it", pkg, files)
		}
	}
	if _, ok := imports["reasonix/internal/toolpolicy"]; !ok {
		t.Error("internal/agent no longer imports toolpolicy — the policy seam is gone")
	}
}

// TestExecuteOneIsThin pins the shape of the hot path. Before Plan 08 executeOne
// was ~110 lines: five policy stages hand-inlined around one t.Execute. It is now
// the loop's own plumbing plus two calls into the pipeline.
func TestExecuteOneIsThin(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatalf("parse agent.go: %v", err)
	}
	const limit = 55
	var seen bool
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "executeOne" || fn.Body == nil {
			continue
		}
		seen = true
		n := fset.Position(fn.Body.End()).Line - fset.Position(fn.Body.Pos()).Line
		if n > limit {
			t.Errorf("executeOne 有 %d 行(上限 %d)—— 检查是不是又有产品策略被内联进工具调用路径了", n, limit)
		}
	}
	if !seen {
		t.Fatal("agent.go 里找不到 executeOne —— 守卫失效了")
	}
}
