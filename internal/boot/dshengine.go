package boot

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/engine/dsh"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/permission"
	"reasonix/internal/tool"
)

// engineName 解析本次装配用哪个底层引擎:显式 Options.Engine > 环境变量
// ONECREAT_ENGINE > 配置 engine。空/未知一律回退 native(默认不变)。
func engineName(cfg *config.Config, override string) string {
	pick := strings.TrimSpace(override)
	if pick == "" {
		pick = strings.TrimSpace(os.Getenv("ONECREAT_ENGINE"))
	}
	if pick == "" && cfg != nil {
		pick = strings.TrimSpace(cfg.Engine)
	}
	if strings.EqualFold(pick, "dsh") {
		return "dsh"
	}
	return "native"
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
	Policy       permission.Policy
}

// buildDSHEngine 造一个 dsh sidecar 引擎。它同时是 agent.Runner(跑一轮)与
// control.EngineBackend(取消/计划模式/会话绑定/关闭)。
func buildDSHEngine(deps dshEngineDeps) (*dsh.Engine, error) {
	cfg := deps.Cfg
	gateway := onecreatGatewayActive()
	baseURL := strings.TrimSpace(cfg.DSH.GatewayBaseURL)
	apiKeyEnv := strings.TrimSpace(cfg.DSH.GatewayTokenEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = "ONECREAT_GATEWAY_TOKEN"
	}
	var apiKey string
	var secrets []string
	if gateway {
		if baseURL == "" {
			baseURL = strings.TrimSpace(os.Getenv("ONECREAT_GATEWAY_URL"))
		}
		apiKey = strings.TrimSpace(os.Getenv(apiKeyEnv))
		secrets = dshBrandSecrets(baseURL)
	} else {
		// 直连:用配置里主 provider 的 base URL 与 key(与 native 同一凭证来源,
		// 都由 internal/config 的 loadDotEnv 从 .env / ~/.env 装进环境)。
		if entry, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			baseURL = entry.BaseURL
			apiKey = entry.APIKey()
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
		APIKey:         apiKey,
		SecretsToScrub: secrets,
		HardwareMCP:    resolveHardwareMCPBin(),
		SessionRoot:    filepath.Join(config.SessionDir(), "dsh"),
		Session:        deps.Session,
		Ledger:         deps.Ledger,
		Tools: func(name string) (tool.Tool, bool) {
			if deps.Registry == nil {
				return nil, false
			}
			return deps.Registry.Get(name)
		},
		Decide: dshDecider(deps.Policy, deps.Registry),
	})
}

// dshDecider 把 Go 侧的 permission.Policy 接到 dsh 的工具预执行钩子上。
// 只读性优先查 Go 工具注册表(同名工具口径一致),查不到就按 dsh 侧常见工具名兜底。
func dshDecider(policy permission.Policy, reg *tool.Registry) dsh.Decider {
	return func(name string, args json.RawMessage) (string, string) {
		readOnly := dshToolReadOnly(name)
		if reg != nil {
			if t, ok := reg.Get(name); ok {
				readOnly = t.ReadOnly()
			}
		}
		switch policy.Decide(name, readOnly, args) {
		case permission.Deny:
			return dsh.DecisionDeny, "被权限策略拒绝(deny 名单)—— 不要重试,换个做法或停下来说明。"
		case permission.Ask:
			return dsh.DecisionAsk, ""
		default:
			return dsh.DecisionAllow, ""
		}
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
