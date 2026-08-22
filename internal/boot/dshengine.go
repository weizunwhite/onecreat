package boot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"reasonix/internal/account"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/engine/dsh"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/tool"
	"reasonix/internal/toolpolicy"
)

// engineName 解析本次装配用哪个底层引擎:显式 Options.Engine > 环境变量
// ONECREAT_ENGINE > 配置 engine。空 = native(默认不变)。
//
// **未知的名字原样返回**,由 selectEngine 报错。回退到 native 会让 `engine = "dsj"`
// 这种拼写错误静默生效成内置内核 —— 用户以为自己在用另一个引擎,实际不是,
// 这比启动失败糟得多(栈的 selectEngine 立的规矩,这里保持)。
func engineName(cfg *config.Config, override string) string {
	pick := strings.TrimSpace(override)
	if pick == "" {
		pick = strings.TrimSpace(os.Getenv("ONECREAT_ENGINE"))
	}
	if pick == "" && cfg != nil {
		pick = strings.TrimSpace(cfg.Engine)
	}
	if pick == "" {
		return "native"
	}
	return strings.ToLower(pick)
}

// dshBrandSecrets 是 dsh 上游会在错误体/日志里吐出来的品牌串。网关模式下它们
// 必须被擦成档位占位符 —— 老师只该看见档位,连"底层是哪家"都不该知道
// (docs/dsh调研/02 漏点③④,已有运行时实证)。直连模式不擦(用户用的就是自己的 key)。
func dshBrandSecrets(gatewayURL string) []string {
	out := []string{
		"deepseek-official", "llm-deepseek", "DeepSeek", "deepseek",
		"api.deepseek.com", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL",
	}
	if u := strings.TrimSpace(gatewayURL); u != "" {
		out = append(out, u)
	}
	return out
}

// dshEngineOptions 组装 dsh sidecar 引擎的参数。
type dshEngineDeps struct {
	Cfg          *config.Config
	Sink         event.Sink
	SystemPrompt string
	CWD          string
	Registry     *tool.Registry
	Session      *agent.Session
	Ledger       *evidence.Ledger
	// Pipeline 是工具策略流水线(plan mode / 权限 / hook / 检查点),
	// dsh 的预执行钩子直接走它,不另起一套门禁。
	Pipeline *toolpolicy.Pipeline
	// Gateway 是平台账号。网关 URL / token / 档位**只能**问它要 —— 直接读
	// ONECREAT_GATEWAY_URL / _TOKEN / ONECREAT_TIER 会撞穿 Plan 09 立下的边界
	// (internal/account/boundary_test.go 全树扫描非测试源码)。
	Gateway *account.Gateway
}

// buildDSHEngine 造一个 dsh sidecar 引擎。它同时是 agent.Runner(跑一轮)与
// control.EngineBackend(取消/计划模式/会话绑定/关闭)。
func buildDSHEngine(deps dshEngineDeps) (*dsh.Engine, error) {
	cfg := deps.Cfg
	gw := deps.Gateway
	gateway := gw != nil && onecreatGatewayActive(gw)
	baseURL := strings.TrimSpace(cfg.DSH.GatewayBaseURL)
	// 凭证传"取值函数"而不是快照:平台 token 约 50 分钟被后台刷新一次(desktop 刷新后
	// 只更新父进程 env、有意不重建标签),而 dsh 子进程的环境是 spawn 时的死快照。
	// 引擎每轮用它重取当前值,变了就经 onecreat/credentials.set 补发(见 dsh.syncCredentials)。
	var apiKeyFunc func() string
	var secrets []string
	if gateway {
		if baseURL == "" {
			baseURL = gw.URL()
		}
		apiKeyFunc = func() string {
			tok, err := gw.Token(context.Background())
			if err != nil {
				return ""
			}
			return strings.TrimSpace(tok)
		}
		secrets = dshBrandSecrets(baseURL)
	} else {
		// 直连:用配置里主 provider 的 base URL 与 key(与 native 同一凭证来源,
		// 都由 internal/config 的 loadDotEnv 从 .env / ~/.env 装进环境)。
		if entry, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			baseURL = entry.BaseURL
			// entry 是 ResolveModel 返回的副本,闭包捕获它是安全的;APIKey() 每次读 env。
			apiKeyFunc = func() string { return strings.TrimSpace(entry.APIKey()) }
		}
	}
	// 调试开关:把 sidecar 的 stderr 直接透到本进程 stderr(默认只留尾缓冲,
	// 免得诊断噪声混进 TUI / Web 的输出通道)。
	var sidecarStderr io.Writer
	if strings.TrimSpace(os.Getenv("ONECREAT_DSH_DEBUG")) != "" {
		sidecarStderr = os.Stderr
	}
	return dsh.New(dsh.Options{
		Cfg:            cfg.DSH,
		Stderr:         sidecarStderr,
		CWD:            deps.CWD,
		Sink:           deps.Sink,
		SystemPrompt:   deps.SystemPrompt,
		Gateway:        gateway,
		BaseURL:        baseURL,
		APIKeyFunc:     apiKeyFunc,
		SecretsToScrub: secrets,
		TierFunc:       dshTierFunc(gw, gateway),
		HardwareMCP:    resolveHardwareMCPBin(),
		SessionRoot:    filepath.Join(config.SessionDir(), "dsh"),
		Session:        deps.Session,
		Ledger:         dshRecorder(deps.Ledger),
		Tools:          dshToolInvoker(deps.Registry, deps.Ledger),
		Decide:         dshDecider(deps.Pipeline, deps.Registry),
	})
}

// dshDecider 把 dsh 的工具预执行钩子接到**栈的那条 toolpolicy 流水线**上。
//
// 这里刻意不自己判 plan mode、不自己查 permission.Policy、不自己做 checkpoint 快照 ——
// 那会是第二份门禁真源,而两份门禁迟早对不上(native 改了规则,dsh 悄悄没跟)。
// `Pipeline.Before` 是 native 路径上每一次工具调用都要过的同一个函数,顺序固定:
// plan mode 只读门 → 权限门(交互式审批就装在这个 Gate 里,ask 会在这里阻塞等用户)
// → PreToolUse hook → 写前检查点快照。任一步拒绝,这里就翻译成 dsh 的 deny,
// sidecar 收到后**不执行**该工具。
//
// 因此 dsh 侧不再需要单独的 Approver / PreEdit / PlanMode 回调:审批与快照都发生在
// Before 里面。dsh 只保留自己的证据记账(见 dshRecorder)——那是从真实的
// tool/result 事件流观测到的结果,不是策略。
//
// 只读性优先查 Go 工具注册表(同名工具口径一致),查不到就按 dsh 侧常见工具名兜底。
func dshDecider(pipeline *toolpolicy.Pipeline, reg *tool.Registry) dsh.Decider {
	return func(name string, args json.RawMessage) (string, string) {
		call := toolpolicy.Call{Name: name, Args: args, ReadOnly: dshToolReadOnly(name)}
		if reg != nil {
			if t, ok := reg.Get(name); ok {
				call.ReadOnly = t.ReadOnly()
				if pv, ok := t.(tool.Previewer); ok {
					call.Preview = pv.Preview
				}
			}
		}
		if _, block := pipeline.Before(context.Background(), call); block != nil {
			return dsh.DecisionDeny, block.Output
		}
		return dsh.DecisionAllow, ""
	}
}

// dshToolReadOnly 判断一个 dsh 内建工具是不是只读(dsh 的工具不在 Go 注册表里)。
func dshToolReadOnly(name string) bool {
	switch name {
	case "read", "grep", "glob", "ls", "todo_write", "complete_step", "exit_plan_mode":
		return true
	}
	return false
}

// resolveHardwareMCPBin 找 OneCreat 硬件 MCP 二进制(挂给 dsh 用)。找不到返回空串
// (dsh 侧就不挂,普通软件项目照常跑)。与 desktop 的 resolveHardwareMCP 同一约定。
func resolveHardwareMCPBin() string {
	if override := strings.TrimSpace(os.Getenv("REASONIX_HARDWARE_MCP")); override != "" {
		if isExecutable(override) {
			return override
		}
		return ""
	}
	bins := []string{"onecreat-hardware-mcp", "reasonix-hardware-mcp"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, b := range bins {
			for _, cand := range []string{
				filepath.Join(dir, b),
				filepath.Join(dir, b+".exe"),
				filepath.Join(dir, "..", "Resources", b),
			} {
				if isExecutable(cand) {
					return filepath.Clean(cand)
				}
			}
		}
	}
	for _, b := range bins {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for _, b := range bins {
			for _, cand := range []string{
				filepath.Join(cwd, "bin", b),
				filepath.Join(cwd, "..", "bin", b),
			} {
				if isExecutable(cand) {
					return filepath.Clean(cand)
				}
			}
		}
	}
	return ""
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// dshRecorder 把证据账本包成引擎层能收的注入闭包。引擎层不得 import
// internal/evidence(A14 守卫),所以 Receipt 的构造留在这一侧。
func dshRecorder(led *evidence.Ledger) dsh.Recorder {
	if led == nil {
		return dsh.Recorder{}
	}
	return dsh.Recorder{
		Reset: led.Reset,
		ToolCall: func(name string, args json.RawMessage, success, readOnly bool) {
			led.Record(evidence.ReceiptFromToolCall(name, args, success, readOnly))
		},
		Todos: func(raw json.RawMessage) {
			var items []evidence.TodoItem
			if err := json.Unmarshal(raw, &items); err != nil {
				return
			}
			led.Record(evidence.Receipt{ToolName: "todo_write", Success: true, Todos: items})
		},
	}
}

// dshToolInvoker 是工具桥的 Go 侧执行闭包:查注册表 → 把证据账本塞进 ctx → 执行 →
// 给这次调用记账。同样出于 A14 守卫,tool.Registry 与 evidence 都不越过引擎边界。
func dshToolInvoker(reg *tool.Registry, led *evidence.Ledger) dsh.ToolInvoker {
	return func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if reg == nil {
			return "", fmt.Errorf("OneCreat 侧没有名为 %s 的工具", name)
		}
		t, ok := reg.Get(name)
		if !ok {
			return "", fmt.Errorf("OneCreat 侧没有名为 %s 的工具", name)
		}
		if led != nil {
			ctx = evidence.WithLedger(ctx, led)
		}
		out, err := t.Execute(ctx, args)
		// 工具桥自己的调用也要记账(complete_step 的签收本身是一条事实)。
		if led != nil {
			led.Record(evidence.ReceiptFromToolCall(name, args, err == nil, true))
		}
		return out, err
	}
}

// dshTierFunc 交出"当前档位"的取值函数。档位是账号状态,只能问 *account.Gateway 要
// —— 引擎层直接读 ONECREAT_TIER 会绕过 Plan 09 的 account 边界。传函数而不是快照,
// 于是切档之后不必重建引擎也能读到新值。
func dshTierFunc(gw *account.Gateway, gateway bool) func() string {
	if !gateway || gw == nil {
		return nil
	}
	return gw.Tier
}
