package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"reasonix/internal/acp"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
)

// acpCommand runs Reasonix as an Agent Client Protocol agent: a stdio JSON-RPC
// server that editors and other host clients drive (initialize, session/new,
// session/prompt, session/cancel). It keeps v2 wire-compatible with the many
// tools that integrated with v1 over ACP.
//
// stdin/stdout are the JSON-RPC channel — nothing else may write to stdout, so
// all diagnostics go to stderr. Each session is assembled by acpFactory, rooted
// at the cwd the client opens.
func acpCommand(args []string, version string) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	modelName := *model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	// Fail fast on a missing/invalid key, with stderr (never stdout) so the wire
	// stays clean, rather than failing per-session deep inside session/new. Resolve
	// and apply the onecreat gateway first, then validate the (possibly rewritten)
	// entry — otherwise gateway mode fails here demanding the underlying vendor key
	// and leaks its name, the very leak the per-session path guards (F1).
	entry, ok := cfg.ResolveModel(modelName)
	if !ok {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, fmt.Errorf("unknown model %q", modelName))
		return 1
	}
	boot.ApplyOnecreatGateway(entry)
	if err := entry.Validate(modelName); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	if cfg.BashMode() == "enforce" && !sandbox.Available() {
		fmt.Fprintln(os.Stderr, "warning: bash sandbox requested but unavailable on this platform; running bash unconfined")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	factory := &acpFactory{cfg: cfg, model: modelName}
	info := acp.AgentInfo{Name: "reasonix", Version: version}
	if err := acp.Serve(ctx, os.Stdin, os.Stdout, factory, info); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	return 0
}

// acpFactory builds one control.Controller per ACP session. It mirrors setup()'s
// assembly, with two differences that make sessions independent: the built-in
// tools are bound to the session's cwd via builtin.Workspace (so concurrent
// sessions have separate path roots), and the client's per-session MCP servers
// are connected alongside the config's own plugins.
type acpFactory struct {
	cfg   *config.Config
	model string
}

// NewSession assembles the per-session controller. Resources (MCP subprocesses)
// are released via the controller's Cleanup, run on ctrl.Close().
func (f *acpFactory) NewSession(ctx context.Context, p acp.SessionParams) (*control.Controller, error) {
	cfg := f.cfg
	entry, ok := cfg.ResolveModel(f.model)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", f.model)
	}
	// onecreat 网关模式:与 boot.Build 同一处理——把 provider 改走平台网关(登录 token 鉴权、
	// 平台统一计费)。否则 ACP 这条平行装配会绕过档位计量直连厂商、甚至以命名 DEEPSEEK_API_KEY
	// 的错误泄露厂商。非网关模式下 ApplyOnecreatGateway 为 no-op,行为不变(F1)。
	boot.ApplyOnecreatGateway(entry)
	gatewayActive := boot.OnecreatGatewayActive()
	proxySpec := cfg.NetworkProxySpec()
	execProv, err := boot.NewProviderWithProxy(entry, proxySpec)
	if err != nil {
		return nil, err
	}
	sysPrompt, err := cfg.ResolveSystemPrompt()
	if err != nil {
		return nil, err
	}
	// 网关模式下追加模型隐私政策(与 chat 一致):问"你是什么模型"不得答真名。非网关不变。
	if gatewayActive {
		sysPrompt += "\n\n" + config.ModelPrivacyPolicy
	}

	// Built-ins rooted at the session cwd. Writes confine to that cwd by default
	// (Workspace makes Dir the sole write root when WriteRoots is empty), which is
	// the right scope for a client that opened the session on a project; an empty
	// cwd falls back to process-cwd tools, identical to the headless run.
	reg := tool.NewRegistry()
	var writeRoots []string
	if p.Cwd != "" {
		writeRoots = []string{p.Cwd}
	}
	bashSpec := sandbox.Spec{Mode: cfg.BashMode(), WriteRoots: writeRoots, Network: cfg.Sandbox.Network}
	ws := builtin.Workspace{Dir: p.Cwd, WriteRoots: writeRoots, Bash: bashSpec, Search: builtin.ResolveSearch(cfg.Tools.Search.Engine, cfg.Tools.Search.RgPath, nil)}
	for _, t := range ws.Tools(cfg.Tools.Enabled...) {
		reg.Add(t)
	}

	// MCP: the config's own plugins plus the servers the client passed in
	// session/new, all connected for the session's lifetime.
	cleanup := func() {}
	var host *plugin.Host
	specs := append(boot.PluginSpecs(cfg.AutoStartPlugins()), p.MCPServers...)
	if len(specs) > 0 {
		h, ptools := plugin.StartAvailable(ctx, specs)
		host = h
		cleanup = h.Close
		for _, t := range ptools {
			reg.Add(t)
		}
		// Mirror boot.Build: phase B (prompts + resources) is deferred to a
		// background goroutine on the session ctx so the ACP path also sees
		// non-empty Host.Prompts()/Resources() once the auxiliary surfaces
		// stream in. Without this, MCPPrompt and @-ref consumers would stay
		// empty for the session.
		go h.StartPhaseB(ctx, p.Sink)
		if text, ok := boot.MCPStartupNotice(h.Failures()); ok {
			p.Sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: text})
		}
	}

	maxSteps := cfg.Agent.MaxSteps
	policy := permission.New(cfg.Permissions.Mode, cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny)
	headlessGate := permission.NewGate(policy, nil)
	reg.Add(agent.NewTaskTool(execProv, entry.Price, reg, maxSteps,
		entry.ContextWindow, cfg.Agent.Temperature, config.ArchiveDir(), "", headlessGate))

	executor := agent.New(execProv, reg, agent.NewSession(sysPrompt), agent.Options{
		MaxSteps:      maxSteps,
		Temperature:   cfg.Agent.Temperature,
		Pricing:       entry.Price,
		Gate:          headlessGate,
		ContextWindow: entry.ContextWindow,
		ArchiveDir:    config.ArchiveDir(),
	}, p.Sink)

	cmds, _ := command.Load(config.CommandDirs()...)

	var runner agent.Runner = executor
	label := entry.Model
	// 网关模式下关掉 planner:planner 是独立 provider,不走网关(ApplyOnecreatGateway 只改主
	// provider),会直连厂商 → 绕过计费 + 泄露厂商。与 boot.Build 的 !gatewayActive 门禁一致(F1)。
	if pm := cfg.Agent.PlannerModel; pm != "" && !gatewayActive {
		pe, ok := cfg.ResolveModel(pm)
		if !ok {
			cleanup()
			return nil, fmt.Errorf("planner_model %q is not a configured provider", pm)
		}
		if pe.Model != entry.Model {
			plannerProv, err := boot.NewProviderWithProxy(pe, proxySpec)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("planner %q: %w", pm, err)
			}
			plannerSess := agent.NewSession(agent.DefaultPlannerPrompt)
			runner = agent.NewCoordinator(plannerProv, plannerSess, pe.Price, executor, cfg.Agent.Temperature, p.Sink)
			label = entry.Model + " + planner " + pe.Model
		}
	}

	return control.New(control.Options{
		Runner:       runner,
		Executor:     executor,
		Sink:         p.Sink,
		Policy:       policy,
		Label:        label,
		SystemPrompt: sysPrompt,
		SessionDir:   config.SessionDir(),
		Host:         host,
		Commands:     cmds,
		Cleanup:      cleanup,
	}), nil
}
