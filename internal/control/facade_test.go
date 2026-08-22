package control

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// A04 named control.Controller an application-layer God Object: Turn, Session,
// Command, Approval, Checkpoint, MCP, Jobs and Memory all aggregated on one type,
// in one 1923-line file, behind one mutex. Plan 07 split the state into services
// and kept the Controller as a compat facade so no caller had to change.
//
// The guards below pin that shape. They are deliberately narrow — a line cap and
// a field whitelist — because the failure mode is gradual: one more field, one
// more inlined domain operation, and the God Object is back.

func controllerFile(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "controller.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse controller.go: %v", err)
	}
	return fset, f
}

func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + typeName(v.X)
	case *ast.SelectorExpr:
		return typeName(v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}

// TestControllerHoldsNoDomainLock is the sharpest statement of the split: every
// piece of mutable domain state moved into a service that carries its own lock,
// so the Controller itself needs none. Before Plan 07 a single c.mu guarded the
// run state, the approval maps, the checkpoint boundaries and the memory snapshot
// — four unrelated domains, none of which ever needed to be atomic together.
//
// A mutex reappearing here means state has moved back onto the facade.
func TestControllerHoldsNoDomainLock(t *testing.T) {
	_, file := controllerFile(t)
	allowed := map[string]bool{
		// transport / identity / immutable-after-construction
		"engine": true, "executor": true, "sink": true, "label": true,
		"systemPrompt": true, "commands": true, "skills": true, "hooks": true,
		"cleanup": true, "autoPlan": true, "classifier": true, "startedOnce": true,
		"balanceURL": true, "balanceKey": true, "balanceClient": true,
		"jobs": true, "reg": true, "wsRoot": true, "gateway": true,
		// the services
		"approvals": true, "session": true, "memory": true, "mcp": true,
		"ckpt": true, "turn": true,
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Controller" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		found = true
		for _, f := range st.Fields.List {
			typ := typeName(f.Type)
			if strings.HasPrefix(typ, "sync.") {
				t.Errorf("Controller 又长出了一把锁(%s)—— 锁意味着它自己持有领域状态,"+
					"该状态应该属于某个服务", typ)
			}
			for _, name := range f.Names {
				if !allowed[name.Name] {
					t.Errorf("Controller 多了字段 %q(%s):它是各服务的兼容 facade,"+
						"领域状态请放进对应的服务;确实是新服务的话把它加进这份白名单",
						name.Name, typ)
				}
			}
		}
		return false
	})
	if !found {
		t.Fatal("controller.go 里找不到 type Controller struct —— 守卫失效了")
	}
}

// TestControllerFileStaysAFacade is the coarse companion alarm: controller.go was
// 1923 lines before Plan 07. If it grows back past the cap, a domain operation is being
// implemented here again instead of in a service.
func TestControllerFileStaysAFacade(t *testing.T) {
	src, err := os.ReadFile("controller.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(src), "\n") + 1
	const limit = 950
	if lines > limit {
		t.Errorf("internal/control/controller.go 是 %d 行(上限 %d)—— 检查是不是又有领域逻辑落回 Controller 了", lines, limit)
	}
}
