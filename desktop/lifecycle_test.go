package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"reasonix/internal/boot"
	"reasonix/internal/workspace"
)

// TestHoldWorkspaceIsNilSafe: 零值 service(单元测试惯用构造)没有 factory,
// 持有请求必须退化成空操作,而不是 panic。
func TestHoldWorkspaceIsNilSafe(t *testing.T) {
	var r *tabRuntimeService
	r.hold(workspace.Context{}).Release()
	(&tabRuntimeService{}).hold(workspace.Context{}).Release()
}

// TestHoldWorkspaceKeepsProjectOpen 是桌面端重建路径的行为断言:持住项目引用的那段
// 时间里,即使没有任何 controller 引用它,工作区也不能关闭 —— 否则项目的语言服务器和
// CodeGraph 守护进程会在两个 controller 之间被停掉再立刻重启。
func TestHoldWorkspaceKeepsProjectOpen(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := boot.NewFactory(context.Background())
	defer f.Close()
	r := &tabRuntimeService{factory: f}

	hold := r.hold(ws)
	roots := f.Process().Workspaces()
	if len(roots) != 1 || roots[0] != ws.Root() {
		t.Fatalf("持有期间项目应保持打开,实际 %v", roots)
	}
	hold.Release()
	if roots := f.Process().Workspaces(); len(roots) != 0 {
		t.Fatalf("释放后项目应关闭,实际 %v", roots)
	}
}

// packageFiles are the package's non-test Go sources. The lifecycle guards below
// scan *all* of them rather than a hand-listed few: a new call site in a new file
// is exactly the case a fixed file list would miss.
func packageFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		file.Name.Name = name // 借 Name 记住文件名,报错时好定位
		out = append(out, file)
	}
	return out
}

func callsSelector(n ast.Node, pkg, sel string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		s, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != sel {
			return true
		}
		if pkg == "" {
			found = true
			return false
		}
		if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg {
			found = true
			return false
		}
		return true
	})
	return found
}

// buildSites maps每个调用 boot.Build 的函数名 → 它所在的文件。
func buildSites(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, file := range packageFiles(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if callsSelector(fn.Body, "boot", "Build") {
				out[fn.Name.Name] = file.Name.Name
			}
		}
	}
	return out
}

// TestEveryBuildSharesTheFactory:桌面端每一处 boot.Build 都必须传共享 factory。
// 漏掉一处,那个标签就独占一份私有的工作区服务 —— 它自己起一套语言服务器和
// CodeGraph 守护进程,关标签时又把它们停掉,共享就此破功(而且完全无声)。
func TestEveryBuildSharesTheFactory(t *testing.T) {
	for _, file := range packageFiles(t) {
		name := file.Name.Name
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Build" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "boot" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Factory" {
						return true
					}
				}
			}
			t.Errorf("%s: 有一处 boot.Build 没传 Factory —— 该标签会独占一份工作区服务", name)
			return true
		})
	}
}

// TestRebuildPathsHoldTheWorkspace:每一条「先关旧 controller、再建新的」的重建路径
// 都必须跨重建持住项目引用。少了它,引用计数在两个 controller 之间会 1→0→1,项目的
// 共享服务被停掉再重启 —— 换个模型或改个设置就白等一次语言服务器冷启动。
//
// 两处不需要 hold,理由写在这里而不是散在代码里:
//   - BuildTab 是标签的首次装配,之前没有 controller 可关;
//   - SwitchWorkspace 换的是【另一个】根,而且它先 Build 后 Close 旧的。
func TestRebuildPathsHoldTheWorkspace(t *testing.T) {
	exempt := map[string]bool{"BuildTab": true, "SwitchWorkspace": true}
	sites := buildSites(t)
	if len(sites) == 0 {
		t.Fatal("找不到任何 boot.Build 调用点 —— 守卫失效了")
	}
	var names []string
	for name := range sites {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if exempt[name] {
			continue
		}
		file := sites[name]
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		var held bool
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			held = callsSelector(fn.Body, "", "hold")
		}
		if !held {
			t.Errorf("%s.%s 重建 controller 时没有跨重建持住项目引用(hold)", file, name)
		}
	}
	t.Logf("boot.Build 调用点:%v", names)
}
