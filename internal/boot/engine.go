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
	"reasonix/internal/runtime"
)

// engineSpec 是选引擎需要的全部输入。
type engineSpec struct {
	Cfg     *config.Config
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
}

// selectEngine 按 cfg.Engine 装配回合引擎。
//
// 配置错误一律在**装配期**炸出来,不留到用户敲下第一句话:未知的引擎名是错误,
// dsh 缺 bin_path 是错误,sidecar 握手失败也是错误 —— 静默回退到 native 会让人
// 以为自己在用 dsh,那比启动失败糟得多。
func selectEngine(ctx context.Context, spec engineSpec) (engine.TurnEngine, error) {
	switch name := strings.ToLower(strings.TrimSpace(spec.Cfg.Engine)); name {
	case "", "native":
		return native.New(spec.Runner), nil

	case "dsh":
		var token string
		if spec.Gateway != nil {
			// 网关 token 只能问 account.Gateway 要 —— 直接读环境变量会撞穿
			// Plan 09 立下的边界(account/boundary_test.go)。拿不到不是致命错误:
			// 本地直连的 dsh 配置本来就没有网关。
			token, _ = spec.Gateway.Token(ctx)
		}
		ad, err := dsh.NewAdapter(dsh.AdapterOptions{Options: dsh.Options{
			Cfg:            spec.Cfg.DSH,
			CWD:            spec.Root,
			Sink:           spec.Sink,
			GatewayToken:   token,
			SecretsToScrub: spec.Secrets,
		}})
		if err != nil {
			return nil, fmt.Errorf("engine = \"dsh\": %w", err)
		}
		if err := ad.Boot(ctx); err != nil {
			return nil, fmt.Errorf("engine = \"dsh\": %w", err)
		}
		if spec.Scope != nil {
			spec.Scope.Defer("dsh-engine", func() {
				_ = ad.Shutdown(context.Background())
			})
		}
		return ad, nil

	default:
		return nil, fmt.Errorf("engine = %q 不是可用的引擎(可选:\"native\"、\"dsh\")", spec.Cfg.Engine)
	}
}
