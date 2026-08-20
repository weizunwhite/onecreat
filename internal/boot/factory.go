package boot

import (
	"context"
	"sync"

	"reasonix/internal/codegraph"
	"reasonix/internal/config"
	"reasonix/internal/lsp"
	"reasonix/internal/runtime"
	"reasonix/internal/workspace"
)

// Factory owns the process- and workspace-scoped half of a OneCreat runtime, so
// several sessions on the same project share what belongs to the project instead
// of each starting its own copy.
//
// Before Plan 05 every resource lived on one lifetime — the session's. Build
// chained the CodeGraph daemon and the LSP manager into the controller's
// cleanup, so closing one desktop tab stopped services a second tab on the same
// project was still using. (The CodeGraph daemon is *itself* documented as
// "按工作区共享" — shared per workspace — which is exactly why stopping it from a
// session was wrong.)
//
// A Factory is explicit, never global: a frontend that wants sharing creates one
// and passes it in Options.Factory. Callers that leave it nil (the CLI, a
// headless run, an ACP session) get a private one-shot factory, which reproduces
// the old behaviour exactly — those frontends run one workspace per process, so
// there is nothing to share.
type Factory struct {
	proc *runtime.Process

	mu sync.Mutex
	// services is the live workspace-scoped service set, keyed exactly as the
	// runtime keys its workspace scopes (workspace.Context.Root()) so the two
	// registries can never disagree about what "the same project" means. An entry
	// exists while at least one session is using it.
	services map[string]*workspaceServices
}

// NewFactory opens a process scope. Close releases every workspace still open
// under it — call it when the app exits.
func NewFactory(ctx context.Context) *Factory {
	return &Factory{
		proc:     runtime.NewProcess(ctx),
		services: map[string]*workspaceServices{},
	}
}

// Close releases the whole tree: every open workspace and its shared services.
func (f *Factory) Close() {
	f.proc.Close()
	f.mu.Lock()
	f.services = map[string]*workspaceServices{}
	f.mu.Unlock()
}

// Process exposes the runtime scope, for callers that need to hang
// process-lifetime resources off it.
func (f *Factory) Process() *runtime.Process { return f.proc }

// workspaceServices are the services shared by every session on one project.
//
// What is *not* here is as deliberate as what is:
//   - the MCP plugin host is session-scoped — `/mcp add` hot-adds into one
//     session's host, and a session may carry client-supplied overlays, so
//     sharing it would leak one tab's servers into another;
//   - background jobs are session-scoped — a job belongs to the conversation
//     that started it;
//   - the skill and memory indexes are per-build snapshots folded into the
//     cache-stable system prompt, not long-lived resources. Caching them per
//     workspace would mean a session opened after an AGENTS.md edit still saw
//     the old text, trading a real correctness property for a trivial saving.
type workspaceServices struct {
	// lsp is the workspace's language-server manager, or nil when LSP is off.
	// It is safe to share: the manager guards its client map and spawns servers
	// lazily on first query.
	lsp *lsp.Manager
}

// WorkspaceHandle is one hold on a workspace's shared services. Every handle
// must be released exactly once; the workspace's services are torn down when the
// last handle goes away.
type WorkspaceHandle struct {
	f     *Factory
	scope *runtime.Workspace
	svc   *workspaceServices
	key   string

	once sync.Once
}

// Release gives up this hold. The workspace closes — stopping its LSP servers
// and CodeGraph daemon — only when it was the last one.
func (h *WorkspaceHandle) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if h.scope.Release() {
			h.f.mu.Lock()
			delete(h.f.services, h.key)
			h.f.mu.Unlock()
		}
	})
}

// Scope is the runtime workspace scope this hold keeps open. Sessions are opened
// under it, so a session's own resources are released when the session ends and
// the project's shared ones survive it.
func (h *WorkspaceHandle) Scope() *runtime.Workspace {
	if h == nil {
		return nil
	}
	return h.scope
}

// Services is the shared service set. It is nil for a handle from Hold, which is
// a lifetime reference rather than a use.
func (h *WorkspaceHandle) Services() *workspaceServices {
	if h == nil {
		return nil
	}
	return h.svc
}

// WorkspaceSpec is what the opening session needs from its project. It is only
// consulted the first time a workspace is opened — the session that gets there
// first starts the shared services. That is not a race between different
// answers: Config is the project's own, loaded from this very root, and a
// process has one code-intelligence posture throughout (an editor host provides
// it for every session, a desktop or CLI for none).
type WorkspaceSpec struct {
	// Config is the project config the services are configured from.
	Config *config.Config
	// Root is the concrete directory the services work in — ws.Root() for a real
	// workspace, the process working directory for the zero Context (a CLI run
	// that was given no project).
	Root string
	// HostProvidesCodeIntel suppresses the code-intelligence services. An editor
	// host runs its own language servers and symbol index, so such a session must
	// neither start a CodeGraph daemon nor — the reason this flag reaches down
	// here at all — stop one it never started: the daemon is shared per workspace
	// across processes, so killing it would hit a desktop OneCreat open on the
	// same project.
	HostProvidesCodeIntel bool
}

// OpenWorkspace takes a hold on a project's shared services, starting them on
// first use. Every call must be matched by Release. The registry is keyed by
// ws.Root(), matching how the runtime keys its workspace scopes.
func (f *Factory) OpenWorkspace(ws workspace.Context, spec WorkspaceSpec) *WorkspaceHandle {
	scope := f.proc.OpenWorkspace(ws)
	key := ws.Root()

	f.mu.Lock()
	svc, ok := f.services[key]
	if !ok {
		svc = f.startWorkspaceServices(scope, spec)
		f.services[key] = svc
	}
	f.mu.Unlock()

	return &WorkspaceHandle{f: f, scope: scope, svc: svc, key: key}
}

// Hold takes a bare reference on a project without starting or using anything.
//
// It is how a frontend keeps a workspace alive across a controller swap. The
// desktop rebuilds a tab's controller when the model or the settings change: it
// closes the old one and builds the replacement. Both sessions want the same
// project, but for the moment in between neither holds it — so without a hold the
// refcount touches zero, the workspace closes, and the project's language servers
// and CodeGraph daemon are stopped and immediately restarted. Holding one
// reference across the swap states the intent directly: *I* am keeping this
// project open while I replace the session on it.
//
// The returned handle carries no services (Services reports nil). Release it once
// the swap is done.
func (f *Factory) Hold(ws workspace.Context) *WorkspaceHandle {
	return &WorkspaceHandle{f: f, scope: f.proc.OpenWorkspace(ws), key: ws.Root()}
}

// startWorkspaceServices boots the shared services once per workspace and
// registers their teardown on the workspace scope — so they are released when
// the workspace closes, never when a session does.
func (f *Factory) startWorkspaceServices(scope *runtime.Workspace, spec WorkspaceSpec) *workspaceServices {
	svc := &workspaceServices{}
	if spec.Config == nil || spec.HostProvidesCodeIntel {
		return svc
	}

	// LSP: servers resolve on PATH and spawn lazily on first query, so building
	// the manager is cheap even with none installed. It is workspace-scoped:
	// two tabs on one project share the same running servers.
	if spec.Config.LSP.Enabled {
		mgr := lsp.NewManager(spec.Root, LSPSpecs(spec.Config.LSP))
		svc.lsp = mgr
		scope.Defer("lsp", mgr.Close)
	}

	// CodeGraph's MCP frontend is started per session (it is one more stdio
	// plugin in that session's host), but the daemon it forks is detached,
	// keyed by workspace root and shared with an idle timeout. Stopping it
	// belongs to the workspace: that is the resource a closing tab used to kill
	// out from under its sibling.
	//
	// Only the teardown lives here. The binary is deliberately *not* resolved
	// once and cached: auto_install fetches it in the background and the notice
	// promises the tools "next session", so resolution has to stay per-session —
	// caching it would push that to "once every tab on this project is closed".
	// Registering the stop unconditionally is safe and covers the session that
	// first finds the binary: StopDaemon with no pidfile is a no-op.
	if spec.Config.Codegraph.Enabled {
		root := spec.Root
		scope.Defer("codegraph-daemon", func() { codegraph.StopDaemon(root) })
	}
	return svc
}
