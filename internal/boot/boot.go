// Package boot assembles a ready-to-drive control.Controller from configuration:
// it loads config, resolves the model(s), builds the tool registry (built-ins +
// plugins), wires the permission gate, and constructs the executor — optionally
// wrapping it in a two-model Coordinator. It is the one place that turns "what the
// user configured" into "a Controller a frontend can drive", so every frontend —
// the terminal TUI, the HTTP/SSE server, the desktop webview — shares the exact
// same assembly instead of each re-deriving it. Frontends pass only a sink and a
// couple of run knobs; everything else comes from config.
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"reasonix/internal/account"
	"reasonix/internal/agent"
	"reasonix/internal/codegraph"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/hook"
	"reasonix/internal/jobs"
	"reasonix/internal/lsp"
	"reasonix/internal/memory"
	"reasonix/internal/netclient"
	"reasonix/internal/outputstyle"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
	"reasonix/internal/workspace"
)

// ErrUnknownModel is returned by Build when the configured model can't be
// resolved to a provider — e.g. a default_model left over from a renamed or
// removed provider. Callers can detect it (errors.Is) to re-run setup.
var ErrUnknownModel = errors.New("unknown model")

// observedHooks wraps the hook Runner so a frontend-supplied observer runs right
// before each tool executes, without changing hook semantics. PreToolUse fires
// the observer first, then delegates to the real hook runner (which alone can
// veto the call); every other method is pure delegation. Observation-only: the
// observer cannot block a tool. The desktop supplies an observer that releases
// its resident serial monitor before an MCP flash/monitor tool grabs the port.
type observedHooks struct {
	observe func(ctx context.Context, name string, args json.RawMessage)
	inner   agent.ToolHooks
}

func (h observedHooks) PreToolUse(ctx context.Context, name string, args json.RawMessage) (bool, string) {
	h.observe(ctx, name, args)
	return h.inner.PreToolUse(ctx, name, args)
}

func (h observedHooks) PostToolUse(ctx context.Context, name string, args json.RawMessage, result string) {
	h.inner.PostToolUse(ctx, name, args, result)
}

func (h observedHooks) PostLLMCall(ctx context.Context, reasoning string, turn int) string {
	return h.inner.PostLLMCall(ctx, reasoning, turn)
}

func (h observedHooks) HasPostLLMCall() bool { return h.inner.HasPostLLMCall() }

func (h observedHooks) SubagentStop(ctx context.Context, last string) {
	h.inner.SubagentStop(ctx, last)
}

func (h observedHooks) PreCompact(ctx context.Context, trigger string) string {
	return h.inner.PreCompact(ctx, trigger)
}

// Options carries the per-run knobs a frontend chooses; everything else is read
// from configuration. Model "" falls back to the configured default_model;
// MaxSteps 0 uses the config/default. RequireKey forces the executor's API key to
// be present (run/serve pass true so a missing key fails fast; chat/desktop pass
// false so the UI is reachable before a key is set). Sink receives the agent's
// typed event stream.
type Options struct {
	Model      string
	MaxSteps   int
	RequireKey bool
	Sink       event.Sink
	// Stderr is the writer for diagnostic warnings and plugin subprocess
	// stderr output. When nil, defaults to os.Stderr. Set to io.Discard
	// during model switch inside a bubbletea session to prevent any output
	// from corrupting the TUI's terminal raw mode.
	Stderr io.Writer
	// PreToolUse, when non-nil, is an observation-only callback that fires right
	// before each tool executes (after permission passes, on the run-loop
	// goroutine). The desktop uses it to release its resident serial monitor
	// before an MCP flash/monitor tool runs, so the tool doesn't hit a busy port.
	// It must NOT block or veto the call — vetoing is the hook system's job.
	PreToolUse func(ctx context.Context, name string, args json.RawMessage)
	// ExtraPlugins are MCP servers supplied by the caller for this session on top
	// of the configured ones — the ACP client's `session/new` servers. They start
	// eagerly with the session: a host that named a server expects its tools on
	// the first turn.
	ExtraPlugins []plugin.Spec
	// HostProvidesCodeIntel suppresses the two workspace services that spawn
	// long-lived subprocesses of their own — CodeGraph's daemon and the LSP
	// manager — because the host already provides them. An editor driving
	// OneCreat over ACP has its own language servers and index; starting a second
	// set inside the agent costs memory and CPU for capabilities the host already
	// has. Everything else (memory, skills, hooks, jobs, prompt policy) is
	// assembled identically.
	HostProvidesCodeIntel bool
	// Factory owns the process- and workspace-scoped services (the LSP manager and
	// the CodeGraph daemon) so several sessions on one project share them. A nil
	// Factory gives this session a private one, closed with it — which is exactly
	// the old behaviour, and the right one for a frontend that runs a single
	// workspace per process (the CLI, a headless run, an ACP session).
	//
	// A frontend that can hold several sessions on one project at once (the
	// desktop) must pass a shared Factory, otherwise closing one tab stops the
	// language servers and symbol daemon its sibling is using.
	Factory *Factory
	// Gateway is the platform account this runtime signs its model requests with.
	// nil means "import whatever the process was launched with" — the
	// compatibility path for a CLI/ACP process, which has no account runtime of
	// its own. The desktop passes its own live Gateway so a token refresh reaches
	// every already-built session without rebuilding anything.
	Gateway *account.Gateway
	// Workspace is the project directory this runtime works in. Everything
	// workspace-scoped — project config, .mcp.json, memory, skills, the file
	// tools' relative-path root, bash's working directory, CodeGraph and plugin
	// subprocess directories — resolves against it.
	//
	// The zero Context means "the process working directory", which is what
	// Build assumed unconditionally before workspaces became explicit; the CLI
	// leaves it zero. A frontend that can hold several projects at once (the
	// desktop, one per tab) must set it, otherwise two runtimes silently share
	// one root.
	Workspace workspace.Context
}

// Build loads config, resolves the model(s), and returns a Controller wrapping a
// single Agent, or a two-model Coordinator when agent.planner_model is set. The
// returned controller owns plugin subprocesses; call Close (via Controller.Close)
// to release them.
func Build(ctx context.Context, opts Options) (*control.Controller, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	ws := opts.Workspace
	// The platform account for this runtime. A frontend that owns one (the
	// desktop) passes it in, so a token refresh reaches every session it built;
	// anything else imports whatever the process was launched with, once.
	gw := opts.Gateway
	if gw == nil {
		gw = account.FromEnv()
	}
	cfg, err := config.LoadIn(ws)
	if err != nil {
		return nil, err
	}
	modelName := opts.Model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(modelName)
	if !ok {
		return nil, fmt.Errorf("%w %q (configured: %s); note: defining [[providers]] replaces the built-in presets, so add a [[providers]] entry for it or use a configured name, or run `reasonix setup` to reconfigure", ErrUnknownModel, modelName, providerNames(cfg))
	}
	// onecreat 网关模式:登录后把模型请求改走平台 AI 网关(用登录 token 鉴权、平台统一
	// 拿上游 key 计费),而非客户端直连厂商。账号状态来自显式的 *account.Gateway,不再
	// 从进程环境变量里读(Plan 09 / A12)。
	applyOnecreatGateway(entry, gw)
	if opts.RequireKey {
		// 校验改写后的 entry(网关模式下已换成 ONECREAT_GATEWAY_TOKEN),不能再 cfg.Validate
		// 重新解析——ResolveModel 返回副本,重解会拿回未改写的原始 entry,在网关模式下点名底层
		// 厂商 key(DEEPSEEK_API_KEY)启动即失败并泄露厂商名。
		if err := entry.Validate(modelName); err != nil {
			return nil, err
		}
	}

	// Serialize the frontend's sink once: background jobs (below) emit from their
	// own goroutines, which can overlap a running turn's emission, so every emitter
	// shares this synchronized sink. The job manager is session-scoped — its jobs
	// outlive a turn and are cancelled by Controller.Close.
	sink := event.Sync(opts.Sink)

	// A resolvable model whose API key env is unset would otherwise build fine
	// (RequireKey is false so the UI stays reachable) and then fail silently on the
	// first request, showing as an empty/dead model. Surface the cause up front.
	if !opts.RequireKey && entry.APIKeyEnv != "" && entry.APIKey() == "" {
		// 中文 + 给出两条可走的动线(桌面端设置面板 / 命令行环境变量),
		// 新用户第一分钟最常撞的就是这里,提示必须能照着做。
		sink.Emit(event.Event{Kind: event.Notice, Text: fmt.Sprintf("模型 %q 还没有配置 API Key(环境变量 %s 为空),发送消息会失败。解决:桌面端点顶栏「设置」→ 找到该模型服务商 → 填入 API Key 即可立刻生效;命令行则先 export %s=你的key 再重启。", modelName, entry.APIKeyEnv, entry.APIKeyEnv)})
	}
	jm := jobs.NewManager(sink)

	proxySpec := cfg.NetworkProxySpec()
	if err := netclient.Validate(proxySpec); err != nil {
		return nil, err
	}
	balanceClient, err := netclient.NewHTTPClient(proxySpec, 12*time.Second, netclient.TransportOptions{})
	if err != nil {
		return nil, err
	}

	execProv, err := newProviderFor(entry, proxySpec, gw)
	if err != nil {
		return nil, err
	}

	sysPrompt, err := cfg.ResolveSystemPrompt()
	if err != nil {
		return nil, err
	}
	// Output style: fold the selected persona/tone block into the base prompt
	// before language/memory/skills append, so a "replace" style (keep-coding
	// false) still keeps those. Applied once, into the cache-stable prefix.
	if st, ok := outputstyle.Resolve(cfg.Agent.OutputStyle, outputstyle.Dirs()); ok {
		sysPrompt = outputstyle.Apply(sysPrompt, st)
	}
	sysPrompt += "\n\n" + config.LanguagePolicy

	// Persistent memory (REASONIX.md / AGENTS.md hierarchy + auto-memory index)
	// folds into the system prompt exactly here, once: it becomes part of the
	// durable, cache-stable prefix every turn reuses, so memory costs nothing per
	// turn. Mid-session changes never touch this prefix — they ride the
	// controller's transient turn-injection and fold in on the next session.
	mem := memory.Load(memory.Options{CWD: ws.Resolve("."), UserDir: config.MemoryUserDir()})
	sysPrompt = memory.Compose(sysPrompt, mem)

	// Skills: discover playbooks (built-in + project/custom/global) and fold their
	// one-liner index into the same cache-stable prefix — names + descriptions
	// only; bodies load on demand via run_skill or "/<name>". Bodies never enter
	// the prefix, so the index costs a fixed, small amount per turn.
	// The workspace root is this runtime's project root. Only when no workspace
	// was supplied does it fall back to the process working directory.
	procCwd, _ := os.Getwd()
	root := ws.RootOr(procCwd)

	// Take a hold on this project's shared services. With a caller-supplied
	// Factory the hold is refcounted, so several sessions on one project share
	// the LSP servers and the CodeGraph daemon and the last one out turns them
	// off. Without one, this session gets a private factory closed alongside it —
	// the pre-Plan-05 behaviour, and the right one for a single-workspace process.
	factory := opts.Factory
	var ownFactory *Factory
	if factory == nil {
		ownFactory = NewFactory(ctx)
		factory = ownFactory
	}
	wsHandle := factory.OpenWorkspace(ws, WorkspaceSpec{Config: cfg, Root: root, HostProvidesCodeIntel: opts.HostProvidesCodeIntel})
	wsSvc := wsHandle.Services()
	// This runtime's session scope, under that workspace. Everything owned by the
	// conversation rather than the project — the MCP plugin host below — is
	// registered on it, so ending the session releases exactly those and leaves
	// the project's shared services running for its siblings.
	sess := wsHandle.Scope().NewSession("")

	// Until control.New takes over the cleanup chain, any error return has to
	// release what has already been acquired itself — otherwise a failed build
	// pins the workspace open and leaks the eager MCP subprocesses.
	//
	// 在 control.New 成功接管 cleanup 之前的任何错误返回,都必须释放已经拿到的资源
	// (session 作用域上的 MCP 子进程、workspace hold)。否则它们泄漏 —— desktop 用
	// 永不取消的 ctx 调 Build,失败的 SetModel / 标签重建每次都漏一份(M1)。
	// 控制器接管后置 success=true,由 Controller.Close() 负责调 cleanup。
	success := false
	defer func() {
		if success {
			return
		}
		sess.Close()
		wsHandle.Release()
		if ownFactory != nil {
			ownFactory.Close()
		}
	}()

	skillStore := skill.New(skill.Options{ProjectRoot: root, CustomPaths: cfg.SkillCustomPaths(), Stderr: opts.Stderr})
	skills := skillStore.List()
	sysPrompt = skill.ApplyIndex(sysPrompt, skills)
	sysPrompt += "\n\n" + config.ModelPrivacyPolicy

	reg := tool.NewRegistry()
	bashSpec := sandbox.Spec{Mode: cfg.BashMode(), WriteRoots: cfg.WriteRoots(), Network: cfg.Sandbox.Network}
	if bashSpec.Mode == "enforce" && !sandbox.Available() {
		fmt.Fprintln(stderr, "warning: bash sandbox requested but unavailable on this platform; running bash unconfined")
	}
	if sandbox.ResolveShell().Kind == sandbox.ShellPowerShell {
		fmt.Fprintln(stderr, "warning: bash not found on PATH; the shell tool will run commands under Windows PowerShell. Install Git for Windows or WSL to use bash.")
	}
	searchSpec := builtin.ResolveSearch(cfg.Tools.Search.Engine, cfg.Tools.Search.RgPath, stderr)
	addBuiltins(reg, ws, cfg.Tools.Enabled, cfg.WriteRoots(), bashSpec, searchSpec, stderr)
	// Always construct a host, even with no plugins configured, so the controller's
	// host pointer is stable for the session and `/mcp add` can hot-add into it.
	//
	// The host is session-scoped: `/mcp add` hot-adds into *this* session's host
	// and a session may carry client-supplied overlays, so it must not be shared
	// with a sibling on the same project. Registering the shutdown on the session
	// scope here — at the point of ownership, through a closure because the eager
	// tier swaps the host below — means no error path can slip out without it.
	pluginHost := plugin.NewHost()
	sess.Defer("mcp-host", func() { pluginHost.Close() })

	// Partition configured plugins by tier so eager/lazy/background can each
	// take the path that fits them. User entries default to lazy — they don't
	// slow the next launch unless the user explicitly opts in to eager.
	eagerEntries, lazyEntries, bgEntries := partitionByTier(cfg.AutoStartPlugins())

	// Auto-demote: any eager plugin that has been chronically slow (recent
	// samples repeatedly hit the blocking startup budget) drops to lazy
	// for this session. The user keeps eager intent, just doesn't pay for it
	// on a server that's been misbehaving. A notice surfaces the demotion.
	var demoteMessages []string
	budget := plugin.DefaultStartupBudget()
	kept := eagerEntries[:0]
	for _, e := range eagerEntries {
		rec := plugin.Recommend(e.Name, budget, 0)
		if rec.Demote {
			demoteMessages = append(demoteMessages, rec.Reason)
			lazyEntries = append(lazyEntries, e)
			continue
		}
		kept = append(kept, e)
	}
	eagerEntries = kept

	eagerSpecs := PluginSpecs(eagerEntries)
	lazySpecs := PluginSpecs(lazyEntries)
	bgSpecs := PluginSpecs(bgEntries)
	// Caller-supplied servers join the eager tier: the host named them for this
	// session, so their tools must exist on the first turn.
	eagerSpecs = append(eagerSpecs, opts.ExtraPlugins...)

	// CodeGraph is a built-in MCP server fetched on first use. When it resolves,
	// inject it as one more stdio plugin pinned to the project root (it is
	// cwd-aware); EnsureInit only creates .codegraph/ (fast, size-independent),
	// serve's daemon then indexes in the background, so startup never blocks even
	// on a large repo. When it is not yet installed, fetch it in the background
	// (one-time, ~45MB) if auto_install is on — startup still never blocks, the
	// tools come online next session — otherwise point the user at the explicit
	// install command. A failed init or fetch is a notice, not fatal.
	//
	// Codegraph stays eager regardless of user tier — symbol-graph tools land
	// in the system prompt, so the agent must see them on first turn.
	if cfg.Codegraph.Enabled && !opts.HostProvidesCodeIntel {
		bin, ok := codegraph.Resolve(cfg.Codegraph.Path)
		switch {
		case ok:
			if err := codegraph.EnsureInit(ctx, bin, root); err != nil {
				sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
					Text: "codegraph: init failed (" + err.Error() + ") — symbol-graph tools disabled this session"})
			}
			eagerSpecs = append(eagerSpecs, plugin.Spec{Name: "codegraph", Command: bin, Args: []string{"serve", "--mcp"}, Dir: root})
		case cfg.Codegraph.AutoInstall:
			notify := func(msg string) { sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: msg}) }
			notify("codegraph: fetching code-intelligence runtime in the background (one-time) — symbol-graph tools available next session")
			codegraphClient, err := netclient.NewHTTPClient(proxySpec, 0, netclient.TransportOptions{})
			if err != nil {
				notify("codegraph: install skipped (" + err.Error() + ")")
			} else {
				go func() {
					if _, err := codegraph.InstallWithClient(context.WithoutCancel(ctx), codegraphClient, nil); err != nil {
						notify("codegraph: install failed (" + err.Error() + ") — using grep/glob; retries next session")
					} else {
						notify("codegraph: installed — symbol-graph tools available next session")
					}
				}()
			}
		default:
			sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
				Text: "codegraph: not installed — run `reasonix codegraph install` to enable symbol-graph tools"})
		}
	}

	// Apply caller-supplied stderr override to every spec across tiers.
	if opts.Stderr != nil {
		for i := range eagerSpecs {
			eagerSpecs[i].Stderr = opts.Stderr
		}
		for i := range lazySpecs {
			lazySpecs[i].Stderr = opts.Stderr
		}
		for i := range bgSpecs {
			bgSpecs[i].Stderr = opts.Stderr
		}
	}

	// Eager: block until handshake. Failures show up in /mcp.
	if len(eagerSpecs) > 0 {
		host, ptools := plugin.StartAvailable(ctx, eagerSpecs)
		pluginHost = host
		for _, t := range ptools {
			reg.Add(t)
		}
		// PhaseB (prompts + resources) runs on the boot ctx — which is the
		// controller's session-scoped PluginCtx — so the auxiliary surfaces
		// keep streaming in after Start returns without holding up the agent.
		go host.StartPhaseB(ctx, sink)
		if text, ok := MCPStartupNotice(host.Failures()); ok {
			sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: text})
		}
	}

	// Lazy / background: register placeholder tools now; the real spawn waits
	// for either the first model call (lazy) or a goroutine kicked off here
	// (background). Both share the same pluginHost so /mcp status, hot-add,
	// and Close see one cohesive set of servers regardless of tier.
	registerDeferred := func(specs []plugin.Spec, kick bool) {
		for _, s := range specs {
			cs, _ := plugin.LoadCachedSchema(s.Name, plugin.SpecFingerprint(s))
			for _, t := range plugin.LazyToolset(s, cs, pluginHost, reg, ctx, kick) {
				reg.Add(t)
			}
		}
	}
	registerDeferred(lazySpecs, false)
	registerDeferred(bgSpecs, true)

	for _, msg := range demoteMessages {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: msg})
	}

	// Closing the session releases what the conversation owns (its MCP host); the
	// workspace hold is released after that — the project's own services (LSP
	// servers, the CodeGraph daemon) go away only when the last session on it
	// does, not when this one ends.
	cleanup := func() {
		sess.Close()
		wsHandle.Release()
		if ownFactory != nil {
			ownFactory.Close()
		}
	}

	// LSP tools query the workspace's shared manager (servers resolve on PATH and
	// spawn lazily on first use). The manager itself belongs to the workspace, so
	// it is not torn down here: two tabs on one project share the running servers.
	if wsSvc.lsp != nil && !opts.HostProvidesCodeIntel {
		for _, t := range lsp.Tools(wsSvc.lsp) {
			reg.Add(t)
		}
	}

	maxSteps := cfg.Agent.MaxSteps
	if opts.MaxSteps > 0 {
		maxSteps = opts.MaxSteps
	}

	// Permission policy gates every tool call. The headless gate (no Approver)
	// resolves "ask" to allow — preserving `reasonix run` autonomy — while deny
	// rules hard-block in every mode. Interactive frontends (chat, desktop) swap
	// in an interactive gate later via Controller.EnableInteractiveApproval.
	// Sub-agents always run headless: they have no UI to answer a prompt, so they
	// inherit this same gate.
	policy := permission.New(cfg.Permissions.Mode, cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny)
	headlessGate := permission.NewGate(policy, nil)

	// Hooks: load the global settings.json plus the project's (only when trusted —
	// project hooks run arbitrary shell commands, so cloning a repo must not
	// silently execute them). Non-blocking hook output is surfaced to the user as
	// a Notice through the shared sink. The runner fires PreToolUse/PostToolUse in
	// the agent loop and UserPromptSubmit/Stop at the controller's turn boundary.
	hooksTrusted := hook.IsTrusted(root, "")
	hookRunner := hook.NewRunner(
		hook.Load(hook.LoadOptions{ProjectRoot: root, Trusted: hooksTrusted}),
		root, nil,
		func(msg string) { sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: msg}) },
	)
	if hook.ProjectDefinesHooks(root) && !hooksTrusted {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
			Text: "this project defines hooks but they are not trusted — run /hooks trust to enable them"})
	}

	// The executor's ToolHooks: the plain hook Runner, or — when the frontend
	// supplied a PreToolUse observer (desktop: release the serial monitor before a
	// flash/monitor MCP tool) — the Runner wrapped so the observer fires first. The
	// control side keeps the concrete *hook.Runner (it only fires turn-boundary
	// hooks and lists them, never PreToolUse), so only the agent path is wrapped.
	var execHooks agent.ToolHooks = hookRunner
	if opts.PreToolUse != nil {
		execHooks = observedHooks{observe: opts.PreToolUse, inner: hookRunner}
	}

	// The `task` tool spawns sub-agents that reuse the parent's provider and
	// tool registry. Wired here after the built-ins / plugins are loaded so
	// sub-agents inherit the full tool set (minus `task` itself, to keep
	// nesting out of the picture). It registers into the same reg the
	// executor uses, so the model surfaces it like any other tool.
	reg.Add(agent.NewTaskTool(execProv, entry.Price, reg, maxSteps,
		entry.ContextWindow, cfg.Agent.Temperature, config.ArchiveDir(), "", headlessGate))

	// The `remember` tool lets the model persist durable facts to the project's
	// auto-memory store; `forget` prunes ones that turn out wrong. The saved index
	// loads into the prefix on the next session.
	reg.Add(memory.NewRememberTool(mem.Store))
	reg.Add(memory.NewForgetTool(mem.Store))

	// The `ask` tool puts structured multiple-choice questions to the user. It
	// reaches them through the Asker on the call context, which interactive
	// frontends wire to the controller (EnableInteractiveApproval); a headless run
	// has none, so ask resolves to "decide for yourself".
	reg.Add(agent.NewAskTool())

	// Skill tools: run_skill / install_skill plus the dedicated subagent wrappers
	// (explore / research / review / security_review). A subagent skill reuses the
	// sub-agent machinery via this runner — an isolated loop with the skill body
	// as system prompt, a tool set scoped to the skill's allowed-tools (minus the
	// task/skill meta-tools, to bar recursion), and an optional per-skill model.
	// Its tool activity nests under the invoking call, like `task`.
	skillRunner := func(sctx context.Context, sk skill.Skill, task string) (string, error) {
		prov, price, ctxWin := execProv, entry.Price, entry.ContextWindow
		// 网关模式下不允许子代理走独立模型覆盖(subagent_model / skill frontmatter):它经
		// cfg.ResolveModel 解析的是未被 applyOnecreatGateway 改写的原始 entry(直连厂商),会
		// 绕过平台档位计量,或在无 key 时以泄露底层厂商名的 401 失败。与 planner/classifier
		// 同理,网关模式统一回退到网关路由的主 executor provider。
		if modelRef := subagentModelRef(cfg, sk); modelRef != "" && !onecreatGatewayActive(gw) {
			if me, ok := cfg.ResolveModel(modelRef); ok {
				if p, err := NewProviderWithProxy(me, proxySpec); err == nil {
					prov, price, ctxWin = p, me.Price, me.ContextWindow
				}
			}
		}
		subReg := agent.FilterRegistry(reg, sk.AllowedTools, agent.SubagentMetaTools()...)
		steps := maxSteps
		if steps > 0 {
			if steps /= 2; steps < 5 {
				steps = 5
			}
		}
		return agent.RunSubAgent(sctx, prov, subReg, sk.Body, task, agent.Options{
			MaxSteps:      steps,
			Temperature:   cfg.Agent.Temperature,
			Pricing:       price,
			Gate:          headlessGate,
			ContextWindow: ctxWin,
			ArchiveDir:    config.ArchiveDir(),
		}, agent.NestedSink(sctx, event.Discard))
	}
	reg.Add(skill.NewRunSkillTool(skillStore, skillRunner))
	reg.Add(skill.NewInstallSkillTool(skillStore, nil))
	for _, t := range skill.BuiltinSubagentTools(skillStore, skillRunner) {
		reg.Add(t)
	}

	execSess := agent.NewSession(sysPrompt)
	executor := agent.New(execProv, reg, execSess, agent.Options{
		MaxSteps:      maxSteps,
		Temperature:   cfg.Agent.Temperature,
		Pricing:       entry.Price,
		Gate:          headlessGate,
		Hooks:         execHooks,
		Jobs:          jm,
		ContextWindow: entry.ContextWindow,
		ArchiveDir:    config.ArchiveDir(),
	}, sink)

	// Custom slash commands (.reasonix/commands + user dir). Best-effort: a malformed
	// file is skipped, and a load error never blocks the session.
	cmds, _ := command.Load(config.CommandDirs()...)

	// Expose the loaded slash commands (skills + custom commands) to the model via
	// the slash_command tool, so it can invoke a project playbook by name the way a
	// user types "/name". Skills are added first, then commands, so a command wins
	// a name clash — matching the prompt's command-over-skill precedence.
	var slashEntries []command.SlashEntry
	for _, sk := range skills {
		sk := sk
		slashEntries = append(slashEntries, command.SlashEntry{
			Name:        sk.Name,
			Description: sk.Description,
			Render:      func(args []string) string { return skill.Render(sk, strings.Join(args, " ")) },
		})
	}
	for _, cmd := range cmds {
		cmd := cmd
		slashEntries = append(slashEntries, command.SlashEntry{
			Name:        cmd.Name,
			Description: cmd.Description,
			ArgHint:     cmd.ArgHint,
			Render:      func(args []string) string { return cmd.Render(args) },
		})
	}
	reg.Add(command.NewSlashCommandTool(slashEntries))

	var runner agent.Runner = executor
	label := entry.Model
	var classifier *control.ProviderAutoPlanClassifier

	// onecreat 网关模式下禁用客户端双模型/分类器:平台统一控制模型(档位),客户端再配一个
	// planner / classifier 没有意义 —— 它们不走网关(applyOnecreatGateway 只改写主 provider),
	// 会(1)在阶段标记/顶栏 label 里泄露真实模型名,(2)直连厂商绕过网关计量(或无 key 失败)。
	gatewayActive := onecreatGatewayActive(gw)

	// Two-model collaboration: a distinct planner_model wraps the executor in a
	// Coordinator with its own session, kept separate for cache stability.
	if pm := cfg.Agent.PlannerModel; pm != "" && !gatewayActive {
		pe, ok := cfg.ResolveModel(pm)
		if !ok {
			return nil, fmt.Errorf("planner_model %q is not a configured provider", pm)
		}
		if pe.Model != entry.Model {
			plannerProv, err := NewProviderWithProxy(pe, proxySpec)
			if err != nil {
				return nil, fmt.Errorf("planner %q: %w", pm, err)
			}
			plannerSess := agent.NewSession(agent.DefaultPlannerPrompt)
			runner = agent.NewCoordinator(plannerProv, plannerSess, pe.Price, executor, cfg.Agent.Temperature, sink)
			label = entry.Model + " + planner " + pe.Model
		}
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.Agent.AutoPlan), "off") && cfg.Agent.AutoPlanClassifier != "" && !gatewayActive {
		cm := cfg.Agent.AutoPlanClassifier
		ce, ok := cfg.ResolveModel(cm)
		if !ok {
			return nil, fmt.Errorf("auto_plan_classifier %q is not a configured provider", cm)
		}
		classifierProv, err := NewProviderWithProxy(ce, proxySpec)
		if err != nil {
			return nil, fmt.Errorf("auto_plan_classifier %q: %w", cm, err)
		}
		classifier = control.NewProviderAutoPlanClassifier(classifierProv)
	}

	// 回合引擎:内置 Go 内核,或 dsh sidecar(见 engine.go)。sidecar 的关闭挂在
	// 会话作用域上,与 MCP host 同级 —— 它也是随会话生灭的子进程。
	turnEngine, err := selectEngine(ctx, engineSpec{
		Cfg:     cfg,
		Root:    root,
		Sink:    sink,
		Gateway: gw,
		Runner:  runner,
		Secrets: []string{entry.Model, entry.BaseURL},
		Scope:   sess,
	})
	if err != nil {
		return nil, err
	}

	ctrlOpts := control.Options{
		Engine:        turnEngine,
		Executor:      executor,
		Sink:          sink,
		Policy:        policy,
		Label:         label,
		SystemPrompt:  sysPrompt,
		SessionDir:    config.SessionDir(),
		Host:          pluginHost,
		Commands:      cmds,
		Skills:        skills,
		Hooks:         hookRunner,
		Memory:        mem,
		Cleanup:       cleanup,
		BalanceURL:    entry.BalanceURL,
		BalanceKey:    entry.APIKey(),
		BalanceClient: balanceClient,
		Jobs:          jm,
		Registry:      reg,
		PluginCtx:     ctx,
		WorkspaceRoot: root,
		Gateway:       gw,
		AutoPlan:      cfg.Agent.AutoPlan,
	}
	if classifier != nil {
		ctrlOpts.Classifier = classifier
	}
	success = true // 控制器接管 cleanup;defer 不再兜底释放
	return control.New(ctrlOpts), nil
}

func subagentModelRef(cfg *config.Config, sk skill.Skill) string {
	if cfg != nil {
		for _, key := range subagentModelKeys(sk.Name) {
			if m := strings.TrimSpace(cfg.Agent.SubagentModels[key]); m != "" {
				return m
			}
		}
	}
	if m := strings.TrimSpace(sk.Model); m != "" {
		return m
	}
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Agent.SubagentModel)
}

func subagentModelKeys(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	keys := []string{name}
	for _, alias := range []string{
		strings.ReplaceAll(name, "-", "_"),
		strings.ReplaceAll(name, "_", "-"),
	} {
		if alias == "" {
			continue
		}
		seen := false
		for _, key := range keys {
			if key == alias {
				seen = true
				break
			}
		}
		if !seen {
			keys = append(keys, alias)
		}
	}
	return keys
}

// onecreatGatewayActive 报告当前是否处于 onecreat 网关模式(桌面端登录后会设这个 env)。
func onecreatGatewayActive(gw *account.Gateway) bool { return gw.Active() }

// applyOnecreatGateway 在「onecreat 网关模式」下改写已解析的 provider entry:BaseURL 指向
// 平台 AI 网关、API key 取登录 token、关掉直连余额查询。仅当 ONECREAT_GATEWAY_URL 存在且该
// provider 是 openai 兼容类型时生效 —— 只有桌面端登录后才会设这两个 env,命令行/其他前端不
// 设则完全无副作用(零耦合)。对外 provider 名也统一成 onecreat,避免错误提示泄露底层模型族。
func applyOnecreatGateway(e *config.ProviderEntry, gw *account.Gateway) {
	if !gw.Active() || e == nil || e.Kind != "openai" {
		return
	}
	e.Name = "onecreat"
	e.BaseURL = gw.URL()
	e.APIKeyEnv = account.EnvToken // 名字只用于报错话术;token 从 Gateway 拿,不读 env
	e.BalanceURL = ""              // 网关模式下不直连厂商查余额
	// 档位模式:把发给上游的 model 改成选中的档位 "tier-N",平台网关再映射到真实模型
	//(对用户隐藏)。未设档位则保持原 model(过渡期:旧版客户端仍发旧模型名,网关兼容)。
	if tier := gw.Tier(); tier != "" {
		e.Model = tier
	}
}

// ApplyOnecreatGateway / OnecreatGatewayActive 是上面两个内部函数的导出封装,供 ACP 等
// 平行装配路径(不走 Build)复用同一网关入口逻辑,避免各自实现漂移导致计费旁路 / 泄露厂商。
func ApplyOnecreatGateway(e *config.ProviderEntry, gw *account.Gateway) { applyOnecreatGateway(e, gw) }

// OnecreatGatewayActive 报告是否处于 onecreat 网关模式(见 onecreatGatewayActive)。
func OnecreatGatewayActive(gw *account.Gateway) bool { return onecreatGatewayActive(gw) }

// NewProvider builds a provider.Provider from a configured entry. Exported so
// custom assemblers (e.g. the ACP per-session factory) can reuse it without
// going through the full Build.
func NewProvider(e *config.ProviderEntry) (provider.Provider, error) {
	return NewProviderWithProxy(e, netclient.ProxySpec{Mode: netclient.ModeAuto})
}

// NewProviderWithProxy builds a provider.Provider with the configured ordinary
// network proxy settings.
func NewProviderWithProxy(e *config.ProviderEntry, proxy netclient.ProxySpec) (provider.Provider, error) {
	return newProviderFor(e, proxy, nil)
}

// newProviderFor builds the provider for one entry. gw is the platform account,
// non-nil and active only on the gateway path: it is both the credential source
// (so a token refresh reaches this client on its next request) and the flag that
// tells the client its upstream is the platform, whose model must never leak
// through an error message.
func newProviderFor(e *config.ProviderEntry, proxy netclient.ProxySpec, gw *account.Gateway) (provider.Provider, error) {
	var creds account.CredentialSource = account.EnvCredential{Var: e.APIKeyEnv}
	gateway := false
	if gw.Active() {
		creds, gateway = gw, true
	}
	return provider.New(e.Kind, provider.Config{
		Name:        e.Name,
		BaseURL:     e.BaseURL,
		Model:       e.Model,
		Credentials: creds,
		Gateway:     gateway,
		// Pass the key's env var so auth failures can name where to fix it, plus
		// provider-kind-specific knobs (the anthropic provider reads thinking/effort;
		// the openai one ignores them).
		Extra: map[string]any{
			"api_key_env": e.APIKeyEnv,
			"thinking":    e.Thinking,
			"effort":      e.Effort,
			"proxy_spec":  proxy,
		},
	})
}

// addBuiltins adds enabled built-in tools to reg. An empty list means all of
// them. writeRoots confines the file-writing built-ins to the workspace, and ws
// binds every workspace-relative built-in to this runtime's project root: after
// the (unconfined, process-cwd) defaults are added, each enabled one is replaced
// by an instance bound to the workspace (preserving registry order).
//
// A zero ws leaves the replacements' work dir empty, i.e. process-cwd relative —
// identical to the registered defaults — so a process-scoped frontend is
// unaffected.
func addBuiltins(reg *tool.Registry, ws workspace.Context, enabled, writeRoots []string, bashSpec sandbox.Spec, searchSpec builtin.SearchSpec, stderr io.Writer) {
	if len(enabled) == 0 {
		for _, t := range tool.Builtins() {
			reg.Add(t)
		}
	} else {
		for _, name := range enabled {
			if t, ok := tool.LookupBuiltin(name); ok {
				reg.Add(t)
			} else {
				fmt.Fprintf(stderr, "warning: unknown built-in tool %q\n", name)
			}
		}
	}
	// Replace the unconfined defaults with workspace-bound instances (registry
	// order is preserved on replace): relative paths resolve against the
	// workspace root, file-writers are confined to writeRoots, bash runs in the
	// workspace under the OS sandbox. Only replace tools actually enabled/present.
	confined := builtin.ConfineWorkspace(ws.Root(), writeRoots, bashSpec, searchSpec)
	for _, t := range confined {
		if _, ok := reg.Get(t.Name()); ok {
			reg.Add(t)
		}
	}
}

// partitionByTier splits configured plugin entries into the three startup
// buckets — eager (block boot until ready), lazy (placeholder until first
// model use), background (placeholder + start spawn now). Entries with an
// unrecognised or empty tier land in lazy (the default).
func partitionByTier(entries []config.PluginEntry) (eager, lazy, bg []config.PluginEntry) {
	for _, e := range entries {
		switch e.ResolvedTier() {
		case "eager":
			eager = append(eager, e)
		case "background":
			bg = append(bg, e)
		default:
			lazy = append(lazy, e)
		}
	}
	return eager, lazy, bg
}

// PluginSpecs maps configured plugin entries to plugin.Spec, expanding ${VAR}
// references. Exported so custom assemblers can connect the config's plugins
// alongside their own (e.g. ACP's per-session MCP servers).
func PluginSpecs(entries []config.PluginEntry) []plugin.Spec {
	specs := make([]plugin.Spec, len(entries))
	for i, e := range entries {
		e = e.ExpandedPlugin() // resolve ${VAR} / ${VAR:-default} from the environment
		specs[i] = plugin.Spec{
			Name:    e.Name,
			Type:    e.Type,
			Command: e.Command,
			Args:    e.Args,
			Env:     e.Env,
			URL:     e.URL,
			Headers: e.Headers,
		}
	}
	return specs
}

// MCPStartupNotice formats the warning shown when configured MCP servers failed
// to connect, naming the first few; ok is false when none failed.
func MCPStartupNotice(failures []plugin.Failure) (text string, ok bool) {
	if len(failures) == 0 {
		return "", false
	}
	names := make([]string, 0, min(len(failures), 3))
	for i, f := range failures {
		if i >= 3 {
			break
		}
		names = append(names, f.Name)
	}
	more := ""
	if len(failures) > len(names) {
		more = fmt.Sprintf(" (+%d more)", len(failures)-len(names))
	}
	return fmt.Sprintf("%d MCP server(s) failed to start: %s%s — run /mcp for details",
		len(failures), strings.Join(names, ", "), more), true
}

// LSPSpecs returns the language → server map: the built-in defaults overlaid with
// any user overrides. A user entry may set only the fields it wants to change;
// empty fields keep the default for that language.
func LSPSpecs(cfg config.LSPConfig) map[string]lsp.ServerSpec {
	specs := lsp.DefaultSpecs()
	for lang, s := range cfg.Servers {
		spec := specs[lang]
		if s.Command != "" {
			spec.Command = s.Command
		}
		if s.Args != nil {
			spec.Args = s.Args
		}
		if s.Env != nil {
			spec.Env = s.Env
		}
		if s.LanguageID != "" {
			spec.LanguageID = s.LanguageID
		}
		if s.Extensions != nil {
			spec.Extensions = s.Extensions
		}
		if s.InstallHint != "" {
			spec.InstallHint = s.InstallHint
		}
		if spec.LanguageID == "" {
			spec.LanguageID = lang
		}
		specs[lang] = spec
	}
	return specs
}

func providerNames(cfg *config.Config) string {
	names := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, "/")
}
