package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"reasonix/internal/acp"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/sandbox"
	"reasonix/internal/workspace"
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

// acpFactory builds one control.Controller per ACP session.
//
// It owns no assembly of its own: every session goes through boot.Build, the
// single composition root the desktop and the CLI already use. Before Plan 04
// this file hand-assembled a parallel stack — provider, tool registry, MCP host,
// permission policy, task tool, executor, planner coordinator, controller — with
// comments like "mirror boot.Build" and "与 boot.Build 的门禁一致" marking every
// rule that had to be copied. Those rules (gateway rewriting, the model-privacy
// prompt, disabling the planner on the gateway path) are billing- and
// IP-sensitive; keeping two copies in step by hand is how they drift.
type acpFactory struct {
	cfg   *config.Config
	model string
}

// NewSession assembles the per-session controller through boot.Build. Two
// per-session inputs come from the ACP client:
//
//   - p.Cwd becomes the session's workspace, so concurrent sessions on different
//     projects get independent path roots, config, memory and skills;
//   - p.MCPServers are the servers the client declared in `session/new`, started
//     alongside the configured ones.
//
// HostProvidesCodeIntel is set because the host is an editor: it already runs
// language servers and its own index, so the agent does not start a second
// CodeGraph daemon and LSP manager inside the session.
//
// Resources (MCP subprocesses and the rest) are released via the controller's
// cleanup, run on ctrl.Close().
func (f *acpFactory) NewSession(ctx context.Context, p acp.SessionParams) (*control.Controller, error) {
	// An empty Cwd yields the zero workspace — process-cwd tools, identical to a
	// headless run. A cwd the client made up is a client error worth reporting.
	ws, err := workspace.New(p.Cwd)
	if err != nil {
		return nil, fmt.Errorf("session cwd %q: %w", p.Cwd, err)
	}
	return boot.Build(ctx, boot.Options{
		Model:                 f.model,
		Sink:                  p.Sink,
		Workspace:             ws,
		ExtraPlugins:          p.MCPServers,
		HostProvidesCodeIntel: true,
		// RequireKey stays false: the key was validated once at startup (above),
		// and a per-session failure would surface deep inside session/new.
		// Stderr stays nil (os.Stderr): stdout is the JSON-RPC channel and must
		// carry nothing else, which boot already respects.
	})
}
