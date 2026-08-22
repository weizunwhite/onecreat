package boot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/engine/dsh"
	"reasonix/internal/permission"
	"reasonix/internal/tool"
)

// dshDecider 是 dsh 引擎下**每次工具调用的唯一决策点**的 Go 半边:计划模式硬门 +
// 只读判定 + permission.Policy 三态透传。它是纯函数,所以整套语义可以用一张表钉死;
// 另一半(deny 真的挡住了文件)由 internal/engine/dsh 的真 sidecar e2e 钉住。

// fakeTool 是只用来占注册表位置的假工具:它的 ReadOnly 与内置名单**故意相反**,
// 于是"注册表优先"这条能被真的证伪。
type fakeTool struct {
	name     string
	readOnly bool
}

func (f fakeTool) Name() string            { return f.name }
func (f fakeTool) Description() string     { return "测试用" }
func (f fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) ReadOnly() bool          { return f.readOnly }
func (f fakeTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

// allowAll 是"什么规则都没有、写工具兜底放行"的策略,用来把计划模式/只读判定
// 从权限规则里隔离出来单独观察。
func allowAll() permission.Policy { return permission.New("allow", nil, nil, nil) }

func planOn() func() bool  { return func() bool { return true } }
func planOff() func() bool { return func() bool { return false } }

// TestDSHDeciderPlanModeDeniesWriters:计划模式是硬门 —— 非只读工具一律拒,
// 不管权限策略怎么说。dsh 自己的 plan-mode 只是软引导(它 README 明说 sandbox/
// approval 才是硬约束),少了这条,计划模式在 dsh 下会退化成"提示模型别改",
// 实际仍能改文件 —— 安全错觉比没有更糟。
func TestDSHDeciderPlanModeDeniesWriters(t *testing.T) {
	decide := dshDecider(allowAll(), nil, planOn())
	for _, name := range []string{"write", "edit", "bash"} {
		decision, reason := decide(name, json.RawMessage(`{"file_path":"/tmp/x"}`))
		if decision != dsh.DecisionDeny {
			t.Errorf("%s: 计划模式下应该 deny,得到 %q", name, decision)
		}
		if !strings.Contains(reason, "计划模式") {
			t.Errorf("%s: deny 原因里应说明是计划模式,得到 %q", name, reason)
		}
	}
}

// TestDSHDeciderPlanModeLetsReadersThrough:只读工具不被计划模式拦 —— 计划模式
// 的意思是"只调研",调研本身要能读文件、能写 todo、能申请退出计划模式。
func TestDSHDeciderPlanModeLetsReadersThrough(t *testing.T) {
	decide := dshDecider(allowAll(), nil, planOn())
	for _, name := range []string{"read", "grep", "glob", "ls", "todo_write", "complete_step", "exit_plan_mode"} {
		decision, _ := decide(name, json.RawMessage(`{}`))
		if decision != dsh.DecisionAllow {
			t.Errorf("%s: 只读工具不该被计划模式拦,得到 %q", name, decision)
		}
	}
}

// TestDSHDeciderNilPlanModeIsOff:PlanMode 回调还没回填时(Controller 造好之前)
// 不能把整个引擎当成计划模式,否则第一轮什么都干不了。
func TestDSHDeciderNilPlanModeIsOff(t *testing.T) {
	decide := dshDecider(allowAll(), nil, nil)
	if decision, _ := decide("write", json.RawMessage(`{}`)); decision != dsh.DecisionAllow {
		t.Fatalf("planMode=nil 应按「不在计划模式」处理,得到 %q", decision)
	}
}

// TestDSHDeciderRegistryWinsOverBuiltinNames:同名工具以 Go 注册表的 ReadOnly 为准,
// 名单只是"注册表里没有"时的兜底 —— 否则一个插件把 read 换成会写盘的实现,
// 计划模式就漏了。两个方向都验:名单说只读→注册表说不是,以及反过来。
func TestDSHDeciderRegistryWinsOverBuiltinNames(t *testing.T) {
	reg := tool.NewRegistry()
	// 名单里 read 是只读,注册表里这个 read 不是。
	reg.Add(fakeTool{name: "read", readOnly: false})
	// 名单里 write 不是只读,注册表里这个 write 是。
	reg.Add(fakeTool{name: "write", readOnly: true})

	decide := dshDecider(allowAll(), reg, planOn())
	if decision, _ := decide("read", json.RawMessage(`{}`)); decision != dsh.DecisionDeny {
		t.Errorf("注册表说 read 不是只读,计划模式就该拒它,得到 %q", decision)
	}
	if decision, _ := decide("write", json.RawMessage(`{}`)); decision != dsh.DecisionAllow {
		t.Errorf("注册表说 write 是只读,计划模式不该拦它,得到 %q", decision)
	}
	// 注册表里没有的名字仍走内置名单兜底。
	if decision, _ := decide("bash", json.RawMessage(`{}`)); decision != dsh.DecisionDeny {
		t.Errorf("注册表里没有 bash,应按名单当写工具拒,得到 %q", decision)
	}
}

// TestDSHDeciderPolicyThreeWay:不在计划模式时,permission.Policy 的三态原样透传到
// dsh —— deny 名单、ask 规则在 dsh 引擎下和 native 是同一份口径。
func TestDSHDeciderPolicyThreeWay(t *testing.T) {
	cases := []struct {
		name     string
		policy   permission.Policy
		tool     string
		args     string
		want     string
		reasonOK func(string) bool
	}{
		{
			name:     "deny 名单命中",
			policy:   permission.New("allow", nil, nil, []string{"bash"}),
			tool:     "bash",
			args:     `{"command":"rm -rf /"}`,
			want:     dsh.DecisionDeny,
			reasonOK: func(s string) bool { return strings.TrimSpace(s) != "" },
		},
		{
			name:   "ask 规则命中",
			policy: permission.New("allow", nil, []string{"write"}, nil),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAsk,
		},
		{
			name:   "都不命中且 Mode=allow",
			policy: permission.New("allow", nil, nil, nil),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAllow,
		},
		{
			name:   "都不命中且 Mode=ask(默认档)",
			policy: permission.New("ask", nil, nil, nil),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAsk,
		},
		{
			name:     "带 glob 的 deny 规则按对象匹配",
			policy:   permission.New("allow", nil, nil, []string{"write(/etc/*)"}),
			tool:     "write",
			args:     `{"file_path":"/etc/passwd"}`,
			want:     dsh.DecisionDeny,
			reasonOK: func(s string) bool { return strings.TrimSpace(s) != "" },
		},
		{
			name:   "glob 不匹配的同名调用不受影响",
			policy: permission.New("allow", nil, nil, []string{"write(/etc/*)"}),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAllow,
		},
		{
			name:   "只读工具即便 Mode=ask 也放行",
			policy: permission.New("ask", nil, nil, nil),
			tool:   "read",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAllow,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decide := dshDecider(c.policy, nil, planOff())
			decision, reason := decide(c.tool, json.RawMessage(c.args))
			if decision != c.want {
				t.Fatalf("决定错:得到 %q,期望 %q(理由 %q)", decision, c.want, reason)
			}
			if c.reasonOK != nil && !c.reasonOK(reason) {
				t.Fatalf("deny 必须带一句给模型看的原因,得到 %q", reason)
			}
		})
	}
}

// TestDSHToolReadOnlyNames 钉住内置名单本身:它是"Go 注册表里查不到"时的兜底,
// 而 dsh 自带的 bash/write/edit 恰恰都不在 Go 注册表里 —— 名单错了,计划模式就漏。
func TestDSHToolReadOnlyNames(t *testing.T) {
	readers := []string{"read", "grep", "glob", "ls", "todo_write", "complete_step", "exit_plan_mode"}
	writers := []string{"write", "edit", "bash", "multi_edit", "mcp__hardware__flash", ""}
	for _, n := range readers {
		if !dshToolReadOnly(n) {
			t.Errorf("%q 应判为只读", n)
		}
	}
	for _, n := range writers {
		if dshToolReadOnly(n) {
			t.Errorf("%q 不该判为只读(不认识的名字必须按写工具兜底)", n)
		}
	}
}
