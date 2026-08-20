package engine_test

// Plan 12 / A14 的防漂移守卫。
//
// 这一层的价值全在「小」上。一旦有人往 TurnEngine 上加 Approve / History /
// Resume / Fork / SetPlanMode,它就变成了 Controller 的改名版,抽象带来的一切好处
// 当场归零 —— 计划文档把这条单独列成「禁止的接口」,就是因为它是这类重构最常见的
// 死法。约定挡不住,所以这里用 AST 钉死。

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/engine"
	dshadapter "reasonix/internal/engine/dsh"
	"reasonix/internal/engine/native"
)

// forbiddenMethods 是 A14 点名的反模式方法:它们全是**应用策略**,一个都不该
// 出现在引擎边界上。
var forbiddenMethods = []string{
	"Approve", "PendingApprovals", "History", "Resume", "Fork", "Rewind",
	"Plan", "SetPlanMode", "NewSession", "SessionPath", "Compact", "Branch",
	"Submit", "Send",
}

// ifaceMethods 解析 internal/engine 里某个接口声明的方法名。
func ifaceMethods(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("解析 internal/engine: %v", err)
	}
	for _, p := range pkg {
		for _, f := range p.Files {
			var out []string
			var found bool
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					return true
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				found = true
				for _, m := range it.Methods.List {
					for _, id := range m.Names {
						out = append(out, id.Name)
					}
				}
				return false
			})
			if found {
				return out
			}
		}
	}
	t.Fatalf("internal/engine 里找不到接口 %s", name)
	return nil
}

func TestTurnEngineStaysMinimal(t *testing.T) {
	got := ifaceMethods(t, "TurnEngine")
	if len(got) != 1 || got[0] != "Start" {
		t.Fatalf("TurnEngine 必须只有 Start 一个方法,拿到 %v —— 见 A14「禁止的接口」", got)
	}
}

func TestTurnHandleStaysMinimal(t *testing.T) {
	got := ifaceMethods(t, "TurnHandle")
	want := map[string]bool{"Cancel": true, "Wait": true}
	if len(got) != len(want) {
		t.Fatalf("TurnHandle 应只有 Cancel + Wait,拿到 %v", got)
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("TurnHandle 多了方法 %q:句柄只负责「停下」和「等它结束」", m)
		}
	}
}

func TestEngineInterfacesRejectApplicationPolicy(t *testing.T) {
	for _, iface := range []string{"TurnEngine", "TurnHandle", "Capable"} {
		for _, m := range ifaceMethods(t, iface) {
			for _, bad := range forbiddenMethods {
				if m == bad {
					t.Errorf("%s.%s 是应用策略,不是引擎能力 —— 把它塞进引擎边界,"+
						"等于把整个 Controller API 改名叫 engineBackend(A14)", iface, m)
				}
			}
		}
	}
}

// applicationPolicyPkgs 是 OneCreat 的产品策略层。引擎边界与 dsh 适配器都在它们
// **下面**,谁 import 了谁就是在长第二套 core。
var applicationPolicyPkgs = []string{
	"reasonix/internal/control",
	"reasonix/internal/toolpolicy",
	"reasonix/internal/permission",
	"reasonix/internal/checkpoint",
	"reasonix/internal/evidence",
	"reasonix/internal/memory",
	"reasonix/internal/billing",
	"reasonix/internal/hook",
	"reasonix/internal/skill",
	"reasonix/internal/tool",
	"reasonix/internal/plugin",
}

// assertNoImports 断言 dir 下的非测试 .go 文件不 import 任何 banned 包。
func assertNoImports(t *testing.T, dir string, banned []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if path == b {
					t.Errorf("%s import 了 %s —— 引擎层不得依赖应用策略(A14)", name, b)
				}
			}
		}
	}
}

func TestEngineBoundaryDependsOnNoPolicy(t *testing.T) {
	// 边界包连 internal/agent 都不该 import:那是**其中一个**引擎的实现包,
	// 边界依赖它就等于边界由内置引擎定义 —— 正是这次重构要消除的形态。
	assertNoImports(t, ".", append(applicationPolicyPkgs, "reasonix/internal/agent"))
}

func TestDSHAdapterIsNotASecondCore(t *testing.T) {
	assertNoImports(t, "dsh", applicationPolicyPkgs)
}

// —— 能力矩阵:说实话,而不是假装什么都支持 ——

func TestNativeDeclaresFullCapabilities(t *testing.T) {
	e := native.New(nil)
	for _, c := range []engine.Capability{
		engine.CapStreaming, engine.CapApproval, engine.CapResume, engine.CapFork,
	} {
		if !engine.Supports(e, c) {
			t.Errorf("内置内核应支持 %s", c)
		}
	}
}

func TestDSHDoesNotClaimWhatItCannotDo(t *testing.T) {
	a, err := dshadapter.NewAdapter(dshadapter.AdapterOptions{
		Options: dshadapter.Options{Cfg: dshConfigWithBin()},
	})
	if err != nil {
		t.Fatalf("构造 dsh 适配器: %v", err)
	}
	if !engine.Supports(a, engine.CapStreaming) {
		t.Error("dsh 有流式增量,应声明 streaming")
	}
	// 这三条是本 Plan 的核心断言:dsh 的工具跑在它自己的进程里、会话日志也在它
	// 那边,OneCreat 的权限门够不着、本地日志也不是它的真源。声明支持才是危险的。
	for _, c := range []engine.Capability{engine.CapApproval, engine.CapResume, engine.CapFork} {
		if engine.Supports(a, c) {
			t.Errorf("dsh 不该声明 %s —— 它兑现不了,假装支持只会让上层静默走样", c)
		}
	}
}

// stubEngine 是一个**不**实现 Capable 的引擎,用来锁住「未声明即不支持」。
type stubEngine struct{}

func (stubEngine) Start(context.Context, engine.TurnRequest) (engine.TurnHandle, error) {
	return nil, nil
}

// dshConfigWithBin 给出一份能通过 NewAdapter 校验的最小 dsh 配置。
// bin_path 指向一个不存在的路径也没关系:这些用例只构造适配器,不拉起子进程。
func dshConfigWithBin() config.DSHConfig {
	return config.DSHConfig{BinPath: filepath.Join(os.TempDir(), "dsh-not-launched")}
}

func TestUndeclaredCapabilitiesFailClosed(t *testing.T) {
	var e engine.TurnEngine = stubEngine{}
	for _, c := range []engine.Capability{
		engine.CapStreaming, engine.CapApproval, engine.CapResume, engine.CapFork,
	} {
		if engine.Supports(e, c) {
			t.Errorf("没声明 Capable 的引擎不该被认为支持 %s —— 未声明即不支持", c)
		}
	}
}
