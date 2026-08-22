package boot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"reasonix/internal/engine/dsh"
	"reasonix/internal/permission"
	"reasonix/internal/tool"
	"reasonix/internal/toolpolicy"
)

// dshDecider 是 dsh 引擎下**每次工具调用的唯一决策点**的 Go 半边。它不再自己判
// 计划模式、不再自己查 permission.Policy —— 它把调用交给 `toolpolicy.Pipeline.Before`,
// 也就是 native 路径上每次工具调用走的同一个函数。这些用例因此同时钉住两件事:
// 接线对(dsh 的名字/只读判定正确地喂进 Call),以及语义等价(plan mode 硬门、
// deny 名单、ask→审批,在 dsh 下与 native 是同一份口径)。
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

// pipelineWith 造一条只装了权限门的流水线 —— 与装配根给 dsh 的是同一个类型,
// 只是没接 hook / checkpoint / 证据(那些各有自己的用例)。
func pipelineWith(p permission.Policy, a permission.Approver) *toolpolicy.Pipeline {
	return &toolpolicy.Pipeline{Gate: permission.NewGate(p, a)}
}

// allowAll 是"什么规则都没有、写工具兜底放行"的策略,用来把计划模式/只读判定
// 从权限规则里隔离出来单独观察。
func allowAll() permission.Policy { return permission.New("allow", nil, nil, nil) }

// approverFunc 让一个闭包当审批人用。
type approverFunc func(ctx context.Context, tool, subject string, args json.RawMessage) (bool, bool, error)

func (f approverFunc) Approve(ctx context.Context, tool, subject string, args json.RawMessage) (bool, bool, error) {
	return f(ctx, tool, subject, args)
}

// TestDSHDeciderPlanModeDeniesWriters:计划模式是硬门 —— 非只读工具一律拒,
// 不管权限策略怎么说。dsh 自己的 plan-mode 只是软引导(它 README 明说 sandbox/
// approval 才是硬约束),少了这条,计划模式在 dsh 下会退化成"提示模型别改",
// 实际仍能改文件 —— 安全错觉比没有更糟。
func TestDSHDeciderPlanModeDeniesWriters(t *testing.T) {
	p := pipelineWith(allowAll(), nil)
	p.SetPlanMode(true)
	decide := dshDecider(p, nil)
	for _, name := range []string{"write", "edit", "bash"} {
		decision, reason := decide(name, json.RawMessage(`{"file_path":"/tmp/x"}`))
		if decision != dsh.DecisionDeny {
			t.Errorf("%s: 计划模式下应该 deny,得到 %q", name, decision)
		}
		if !strings.Contains(reason, "plan mode") {
			t.Errorf("%s: deny 原因里应说明是计划模式,得到 %q", name, reason)
		}
	}
}

// TestDSHDeciderPlanModeLetsReadersThrough:只读工具不被计划模式拦 —— 计划模式
// 的意思是"只调研",调研本身要能读文件、能写 todo、能申请退出计划模式。
func TestDSHDeciderPlanModeLetsReadersThrough(t *testing.T) {
	p := pipelineWith(allowAll(), nil)
	p.SetPlanMode(true)
	decide := dshDecider(p, nil)
	for _, name := range []string{"read", "grep", "glob", "ls", "todo_write", "complete_step", "exit_plan_mode"} {
		decision, _ := decide(name, json.RawMessage(`{}`))
		if decision != dsh.DecisionAllow {
			t.Errorf("%s: 只读工具不该被计划模式拦,得到 %q", name, decision)
		}
	}
}

// TestDSHDeciderDefaultsToPlanModeOff:新造的流水线不在计划模式 —— 否则第一轮
// 什么都干不了。(旧版这条测的是 "planMode 回调还没回填";回调没有了,但
// "默认不是计划模式"这个必须成立的性质仍然要钉住。)
func TestDSHDeciderDefaultsToPlanModeOff(t *testing.T) {
	decide := dshDecider(pipelineWith(allowAll(), nil), nil)
	if decision, _ := decide("write", json.RawMessage(`{}`)); decision != dsh.DecisionAllow {
		t.Fatalf("新流水线应按「不在计划模式」处理,得到 %q", decision)
	}
}

// TestDSHDeciderNilPipelineAllows:Pipeline 为 nil 时不该 panic。
// (*Pipeline).Before 自己是 nil 安全的,这里把这条性质在 dsh 这一侧也钉住。
func TestDSHDeciderNilPipelineAllows(t *testing.T) {
	decide := dshDecider(nil, nil)
	if decision, _ := decide("write", json.RawMessage(`{}`)); decision != dsh.DecisionAllow {
		t.Fatalf("nil 流水线不该 panic,也不该凭空 deny,得到 %q", decision)
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

	p := pipelineWith(allowAll(), nil)
	p.SetPlanMode(true)
	decide := dshDecider(p, reg)
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

// TestDSHDeciderPolicyThreeWay:不在计划模式时,permission 的三态经流水线原样
// 生效 —— deny 名单、ask 规则在 dsh 引擎下和 native 是同一份口径。
//
// 与旧版的差别:ask 不再作为 DecisionAsk 交回引擎、再由引擎去问审批人。审批发生在
// Gate 里面(和 native 一模一样),所以 decider 只剩 allow / deny 两态。没有审批人
// 时 Gate 的既有语义是"保持自主 → 放行",headless 下因此是 allow。
func TestDSHDeciderPolicyThreeWay(t *testing.T) {
	cases := []struct {
		name     string
		policy   permission.Policy
		approver permission.Approver
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
			name:   "ask 规则命中 + 无审批人(headless)→ 保持自主放行",
			policy: permission.New("allow", nil, []string{"write"}, nil),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAllow,
		},
		{
			name:   "ask 规则命中 + 审批人批准 → 放行",
			policy: permission.New("allow", nil, []string{"write"}, nil),
			approver: approverFunc(func(context.Context, string, string, json.RawMessage) (bool, bool, error) {
				return true, false, nil
			}),
			tool: "write",
			args: `{"file_path":"/tmp/x"}`,
			want: dsh.DecisionAllow,
		},
		{
			name:   "ask 规则命中 + 审批人拒绝 → deny(工具不执行)",
			policy: permission.New("allow", nil, []string{"write"}, nil),
			approver: approverFunc(func(context.Context, string, string, json.RawMessage) (bool, bool, error) {
				return false, false, nil
			}),
			tool:     "write",
			args:     `{"file_path":"/tmp/x"}`,
			want:     dsh.DecisionDeny,
			reasonOK: func(s string) bool { return strings.Contains(s, "declined") },
		},
		{
			name:   "审批出错 → fail-closed",
			policy: permission.New("allow", nil, []string{"write"}, nil),
			approver: approverFunc(func(context.Context, string, string, json.RawMessage) (bool, bool, error) {
				return false, false, errors.New("通道断了")
			}),
			tool:     "write",
			args:     `{"file_path":"/tmp/x"}`,
			want:     dsh.DecisionDeny,
			reasonOK: func(s string) bool { return strings.TrimSpace(s) != "" },
		},
		{
			name:   "都不命中且 Mode=allow",
			policy: permission.New("allow", nil, nil, nil),
			tool:   "write",
			args:   `{"file_path":"/tmp/x"}`,
			want:   dsh.DecisionAllow,
		},
		{
			name:   "都不命中且 Mode=ask(默认档)+ 审批人拒绝",
			policy: permission.New("ask", nil, nil, nil),
			approver: approverFunc(func(context.Context, string, string, json.RawMessage) (bool, bool, error) {
				return false, false, nil
			}),
			tool: "write",
			args: `{"file_path":"/tmp/x"}`,
			want: dsh.DecisionDeny,
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
			decide := dshDecider(pipelineWith(c.policy, c.approver), nil)
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
