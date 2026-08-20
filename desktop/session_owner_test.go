package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyOneSessionRegistryIsOpened guards the reason the desktop threads a
// *session.Registry through its helpers instead of opening one per call.
//
// The registry serialises its read-modify-write under its own lock, but an
// atomic rename only keeps the file from being corrupted — it does not prevent a
// lost update. Two tabs recording a display mapping at the same time through two
// *different* instances each hold a different lock, so the later write clobbers
// the earlier one's entry. That is exactly what the old package-level sidecarMu
// prevented, and it is easy to reintroduce by writing `session.Open(dir)` at a
// call site.
//
// One instance, owned by sessionService. Tests open their own — they run against
// their own temp directories.
func TestOnlyOneSessionRegistryIsOpened(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var fn string
		ast.Inspect(file, func(n ast.Node) bool {
			if d, ok := n.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Open" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "session" {
				sites = append(sites, name+":"+fn)
			}
			return true
		})
	}
	if len(sites) != 1 || !strings.HasSuffix(sites[0], ":newSessionService") {
		t.Errorf("session.Open call sites = %v; want exactly one, in newSessionService.\n"+
			"多开一个实例就多一把锁,多标签并发写元数据会丢更新 —— 把 sessionService.reg 传下去。", sites)
	}
}
