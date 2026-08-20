package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"reasonix/internal/boot"
	"reasonix/internal/workspace"
)

// TestHoldWorkspaceIsNilSafe: 裸 &App{}(单元测试惯用构造)没有 factory,持有请求
// 必须退化成空操作,而不是 panic。
func TestHoldWorkspaceIsNilSafe(t *testing.T) {
	var a *App
	a.holdWorkspace(workspace.Context{}).Release()
	(&App{}).holdWorkspace(workspace.Context{}).Release()
}

// TestHoldWorkspaceKeepsProjectOpen 是桌面端重建路径的行为断言:App 持住项目引用的
// 那段时间里,即使没有任何 controller 引用它,工作区也不能关闭 —— 否则项目的语言
// 服务器和 CodeGraph 守护进程会在两个 controller 之间被停掉再立刻重启。
func TestHoldWorkspaceKeepsProjectOpen(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{factory: boot.NewFactory(context.Background())}
	defer a.factory.Close()

	hold := a.holdWorkspace(ws)
	roots := a.factory.Process().Workspaces()
	if len(roots) != 1 || roots[0] != ws.Root() {
		t.Fatalf("持有期间项目应保持打开,实际 %v", roots)
	}
	hold.Release()
	if roots := a.factory.Process().Workspaces(); len(roots) != 0 {
		t.Fatalf("释放后项目应关闭,实际 %v", roots)
	}
}

func parseDesktopFile(t *testing.T, name string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// TestEveryBuildSharesTheFactory:桌面端每一处 boot.Build 都必须传共享 factory。
// 漏掉一处,那个标签就独占一份私有的工作区服务 —— 它自己起一套语言服务器和
// CodeGraph 守护进程,关标签时又把它们停掉,共享就此破功(而且完全无声)。
func TestEveryBuildSharesTheFactory(t *testing.T) {
	for _, name := range []string{"app.go", "settings_app.go"} {
		file := parseDesktopFile(t, name)
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
			if !ok || pkg.Name != "boot" || sel.Sel.Name != "Build" {
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

// TestRebuildPathsHoldTheWorkspace:三条「先关旧 controller、再建新的」的重建路径
// 必须跨重建持住项目引用。少了它,引用计数在两个 controller 之间会 1→0→1,项目的
// 共享服务被停掉再重启 —— 换个模型或改个设置就白等一次语言服务器冷启动。
func TestRebuildPathsHoldTheWorkspace(t *testing.T) {
	want := map[string]string{
		"SetModel":       "app.go",
		"rebuildTabByID": "app.go",
		"rebuild":        "settings_app.go",
	}
	for fn, name := range want {
		file := parseDesktopFile(t, name)
		var found, seen bool
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if !ok || decl.Name.Name != fn || decl.Recv == nil {
				return true
			}
			seen = true
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "holdWorkspace" {
					found = true
				}
				return true
			})
			return false
		})
		if !seen {
			t.Fatalf("%s 里找不到方法 %s —— 测试需要跟着改名更新", name, fn)
		}
		if !found {
			t.Errorf("%s.%s 没有跨重建持住项目引用(holdWorkspace)", name, fn)
		}
	}
}
