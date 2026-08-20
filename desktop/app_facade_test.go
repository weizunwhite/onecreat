package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// appFile parses desktop/app.go. The guards below are deliberately scoped to that
// one file: A03 named it by name — Tab、Chat、Workspace、账号、硬件、知识库、设置、
// 更新 had all collected on `App`, in a single 3766-line file.
func appFile(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "app.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}
	return fset, f
}

// TestAppHoldsNoDomainState is Plan 06's acceptance, stated as a structural fact:
// App is a transport facade, so its fields may only be the transport and the
// composition — the host context, the shell, the tab registry, the shared
// resource factory, and the domain services it delegates to.
//
// A lock is the tell. Every mutex that used to live here guarded a piece of
// domain state (the hardware mutual-exclusion slots, the MCP drawer's disabled
// map, the serial connection, the per-tab autosave single-flight table, the
// selected project folder). If one comes back, so has the God Object.
func TestAppHoldsNoDomainState(t *testing.T) {
	_, file := appFile(t)
	allowed := map[string]bool{
		"ctx": true, "shell": true, "tabs": true, "factory": true,
		"hw": true, "mcp": true, "files": true, "memory": true,
		"sessions": true, "rt": true, "serial": true,
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "App" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		found = true
		for _, f := range st.Fields.List {
			typ := types(f.Type)
			if strings.HasPrefix(typ, "sync.") {
				t.Errorf("App 又长出了一把锁(%v %s)—— 锁意味着它自己持有领域状态,"+
					"该状态应该属于某个服务", names(f), typ)
			}
			for _, name := range f.Names {
				if !allowed[name.Name] {
					t.Errorf("App 多了字段 %q(%s):App 只能持有传输与装配层的东西,"+
						"领域状态请放进对应的服务;确实是新服务的话把它加进这份白名单",
						name.Name, typ)
				}
			}
		}
		return false
	})
	if !found {
		t.Fatal("app.go 里找不到 type App struct —— 守卫失效了")
	}
}

func names(f *ast.Field) []string {
	var out []string
	for _, n := range f.Names {
		out = append(out, n.Name)
	}
	return out
}

func types(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + types(v.X)
	case *ast.SelectorExpr:
		return types(v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}

// TestAppFileStaysAFacade is the coarse companion alarm. app.go was 3766 lines
// before Plan 06; a facade over the services is a fraction of that. If it grows
// back past the cap, business logic is landing here again instead of in a service.
func TestAppFileStaysAFacade(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(src), "\n") + 1
	const limit = 900
	if lines > limit {
		t.Errorf("desktop/app.go 是 %d 行(上限 %d)—— 检查是不是又有业务逻辑落回 App 了", lines, limit)
	}
}

// TestAppMethodsAreThin: every method on App in app.go is delegation or DTO
// mapping, so none of them should be long. The two startup paths are exempt by
// name — assembling the app *is* App's own job.
func TestAppMethodsAreThin(t *testing.T) {
	fset, file := appFile(t)
	exempt := map[string]bool{"buildController": true, "startup": true}
	const limit = 26
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil || exempt[fn.Name.Name] {
			continue
		}
		if types(fn.Recv.List[0].Type) != "*App" {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Line
		end := fset.Position(fn.Body.End()).Line
		if n := end - start; n > limit {
			t.Errorf("App.%s 有 %d 行(上限 %d)—— transport facade 上的方法应该是转发或 DTO 组装,"+
				"真正的逻辑请搬进服务", fn.Name.Name, n, limit)
		}
	}
}
