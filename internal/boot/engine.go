package boot

// 回合引擎的选择(Plan 12 / A14)。
//
// 在此之前 `engine = "dsh"` 是个**不生效**的配置项:`config.Config.Engine` 除了被
// 渲染进 TOML 之外没有任何消费者,写了它也只会静默跑内置内核。这里把它接上 ——
// 顺带也就把「装配根决定用哪个引擎」这件事收在了唯一的装配入口里。

import (
	"context"
	"fmt"
	"strings"

	"reasonix/internal/account"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/engine"
	"reasonix/internal/engine/dsh"
	"reasonix/internal/engine/native"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/runtime"
	"reasonix/internal/tool"
	"reasonix/internal/toolpolicy"
)

// engineSpec 是选引擎需要的全部输入。
type engineSpec struct {
	Cfg *config.Config
	// Name 是已解析好的引擎名(显式 Options.Engine > ONECREAT_ENGINE > cfg.Engine,
	// 见 engineName)。装配根解析一次,这里不再自己读配置或环境。
	Name    string
	Root    string
	Sink    event.Sink
	Gateway *account.Gateway
	// Runner 是内置内核(executor,或包了 planner 的 coordinator)。
	Runner agent.Runner
	// Secrets 是要在 dsh 事件里兜底擦掉的真实串(模型名、上游 base URL)。
	// 网关模式下它们是计费与 IP 机密,绝不能经由 sidecar 的事件流泄到 UI。
	Secrets []string
	// Scope 是会话作用域;sidecar 的关闭挂在它上面(Plan 05 的所有权模型)。
	Scope *runtime.Session
	// SystemPrompt 是已组装好的系统提示(dsh 在自己进程里跑模型,要显式下发)。
	SystemPrompt string
	// Registry 是 Go 侧工具注册表,工具桥用(complete_step 由 Go 执行)。
	Registry *tool.Registry
	// Session 是 Go 侧消息镜像,引擎每轮把 user/assistant 文本投影进去。
	Session *agent.Session
	// Ledger 是证据账本(经 dshRecorder 包成注入闭包后交给引擎)。
	Ledger *evidence.Ledger
	// Pipeline 是工具策略流水线,dsh 的预执行钩子经 dshDecider 走它。
	Pipeline *toolpolicy.Pipeline
}

// dshProbe 让 fail-closed 检查复用 dsh 适配器**真实的**能力声明,而不是在这里
// 另写一份判断 —— 那就成了第二个真源。它现在**放行**了:见 selectEngine 里的说明。
type dshProbe struct{}

func (dshProbe) Start(context.Context, engine.TurnRequest) (engine.TurnHandle, error) {
	return nil, nil
}
func (dshProbe) EngineName() string                { return dsh.Name }
func (dshProbe) Supports(c engine.Capability) bool { return dsh.Capabilities().Supports(c) }

// selectEngine 按 cfg.Engine 装配回合引擎。
//
// 配置错误一律在**装配期**炸出来,不留到用户敲下第一句话:未知的引擎名是错误,
// dsh 缺 bin_path 是错误,sidecar 握手失败也是错误 —— 静默回退到 native 会让人
// 以为自己在用 dsh,那比启动失败糟得多。
func selectEngine(ctx context.Context, spec engineSpec) (engine.TurnEngine, error) {
	switch name := strings.ToLower(strings.TrimSpace(spec.Name)); name {
	case "", "native":
		return native.New(spec.Runner), nil

	case "dsh":
		// fail-closed(AR-R01)仍然在,只是判据从"工具必须在 OneCreat 进程里执行"
		// 放宽到了**门禁时点**:`CapHostedTools || CapGatedTools`。
		//
		// 原来的判据写于 rc.7 spike 时期,当时的结论是"dsh 协议没有把工具调用委托
		// 回 OneCreat 的通道",因此它的 tool/call 推过来时文件已经写完了 —— 事后
		// 看一眼不叫门禁,所以直接拒绝装配。**那个结论现在过时了**:dsh-tools 的
		// 调度器在派发之前会跑 `tools/pre-execute` waterfall,控制面插件把它 await
		// 到 Go 侧,权限策略 / plan mode 只读门 / 审批 / 写前检查点全部在**工具真的
		// 跑起来之前**完成,拒绝时工具不执行(docs/dsh调研/07 §3,真 sidecar e2e 五条)。
		//
		// 变的是判据,不是原则:一个既不 hosted 也不 gated 的引擎照旧拒绝装配,
		// 而且仍然**没有**"developer preview"之类的开关可以绕过它 —— 一个能被一行
		// 配置关掉的安全门等于没有门。
		if err := requireToolGating(dshProbe{}); err != nil {
			return nil, err
		}
		inner, err := buildDSHEngine(dshEngineDeps{
			Cfg:          spec.Cfg,
			Sink:         spec.Sink,
			SystemPrompt: spec.SystemPrompt,
			CWD:          spec.Root,
			Registry:     spec.Registry,
			Session:      spec.Session,
			Ledger:       spec.Ledger,
			Pipeline:     spec.Pipeline,
			Gateway:      spec.Gateway,
		})
		if err != nil {
			return nil, fmt.Errorf("engine = \"dsh\": %w", err)
		}
		if err := inner.Start(ctx); err != nil {
			return nil, fmt.Errorf("engine = \"dsh\": %w", err)
		}
		if spec.Scope != nil {
			spec.Scope.Defer("dsh-engine", func() {
				_ = inner.Shutdown(context.Background())
			})
		}
		return dsh.NewTurnEngine(inner), nil

	default:
		return nil, fmt.Errorf("engine = %q 不是可用的引擎(可选:\"native\"、\"dsh\")", name)
	}
}

// requireToolGating 是 AR-R01 的 fail-closed 判据:一个引擎要么在 OneCreat 进程里
// 执行工具(CapHostedTools),要么在派发前阻塞等 OneCreat 裁决(CapGatedTools)。
// 两条都没有,就说明它在背着我们跑工具 —— 权限门 / plan mode / PreToolUse hook /
// 写前检查点 / 证据链全部落空,事后看一眼不叫门禁,拒绝装配。
//
// 判据从"必须 hosted"放宽到"hosted 或 gated",是因为原判据写于 rc.7 spike 时期,
// 当时的结论是"dsh 协议没有把工具调用委托回 OneCreat 的通道"。**那个结论现在过时了**:
// dsh-tools 的调度器在派发之前跑 `tools/pre-execute` waterfall,控制面插件把它 await
// 到 Go 侧,拒绝时工具不执行(docs/dsh调研/07 §3,真 sidecar e2e 五条)。
//
// 变的是判据,不是原则:仍然**没有**"developer preview"之类的开关能绕过它 ——
// 一个能被一行配置关掉的安全门等于没有门。
func requireToolGating(e engine.TurnEngine) error {
	if engine.Supports(e, engine.CapHostedTools) || engine.Supports(e, engine.CapGatedTools) {
		return nil
	}
	return fmt.Errorf("engine = %q 暂不可用:%w\n"+
		"  这个引擎既不在 OneCreat 进程里执行工具(hosted-tools),也没有在派发前\n"+
		"  等待 OneCreat 裁决(gated-tools)—— 权限门 / plan mode / PreToolUse hook /\n"+
		"  写前检查点 / 证据链都够不着它。请改回 engine = \"native\"。",
		engine.NameOf(e), engine.Unsupported(engine.NameOf(e), "执行工具调用", engine.CapGatedTools))
}
