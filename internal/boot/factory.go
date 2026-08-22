package boot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	// entries is the **single authority** on an open workspace: its runtime
	// scope, its shared services and its hold count, all under this one lock
	// (AR-R11).
	//
	// 之前是两个注册表:refcount 在 runtime.Process 那边,services 在这边,而
	// Release 先关 scope、后删 services。中间那一瞬,一个并发的 OpenWorkspace 会
	// 拿到**新的 scope + 已经关掉的 services**(LSP manager 已 Close),然后前一个
	// 的 delete 再把它正在用的那条记录抹掉。两个注册表就是两个真源。
	//
	// 键与 runtime 给 workspace scope 的键一致(workspace.Context.Root()),这样
	// "同一个项目"在两边永远是同一件事。
	entries map[string]*wsEntry
}

// wsEntry 是一个已打开工作区的全部状态。所有字段都在 Factory.mu 下读写。
type wsEntry struct {
	scope *runtime.Workspace
	// svc 惰性创建:Hold 只占生命周期、不启动服务,所以第一个持有者可能是它。
	svc *workspaceServices
	// holds 是这个工作区当前的持有数(OpenWorkspace 与 Hold 都算)。
	holds int
	// closing 在最后一个持有者开始拆除时置起。此时不能让新的 OpenWorkspace 拿到
	// 这条正在死去的记录 —— 让它等拆完再重来,于是它会开到一个全新的 scope。
	closing chan struct{}
	// fingerprint 是第一次打开时那份 WorkspaceSpec 的指纹。之后带着不同配置来开
	// 同一个工作区,共享服务不会重新配置(它们正被别人用着)—— 这件事必须被说出来,
	// 而不是静默忽略。
	fingerprint string
}

// NewFactory opens a process scope. Close releases every workspace still open
// under it — call it when the app exits.
func NewFactory(ctx context.Context) *Factory {
	return &Factory{
		proc:    runtime.NewProcess(ctx),
		entries: map[string]*wsEntry{},
	}
}

// Close releases the whole tree: every open workspace and its shared services.
func (f *Factory) Close() {
	f.proc.Close()
	f.mu.Lock()
	f.entries = map[string]*wsEntry{}
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
//
// 拆除的顺序是这次修复的核心:先**在锁内**把这条记录标成 closing,再在锁外真正关
// scope(关闭会跑 LSP Close / StopDaemon,不该占着锁),最后删除记录并放行等待者。
// 于是任何并发的 OpenWorkspace 要么在拆除开始前就拿到了这条记录(那它就不是最后一个,
// 根本不会拆),要么看见 closing 并等到拆完 —— 绝不会拿到一个"新 scope + 旧服务"的
// 组合(AR-R11)。
func (h *WorkspaceHandle) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		f := h.f
		f.mu.Lock()
		e := f.entries[h.key]
		last := false
		if e != nil && e.scope == h.scope {
			e.holds--
			if e.holds <= 0 {
				last = true
				e.closing = make(chan struct{})
			}
		}
		f.mu.Unlock()

		h.scope.Release()

		if last && e != nil {
			f.mu.Lock()
			if f.entries[h.key] == e {
				delete(f.entries, h.key)
			}
			f.mu.Unlock()
			close(e.closing)
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
	e := f.acquire(ws, &spec)
	return &WorkspaceHandle{f: f, scope: e.scope, svc: e.svc, key: ws.Root()}
}

// acquire 取得(或建立)一个工作区记录并把持有数 +1。spec 为 nil 表示只占生命周期、
// 不启动共享服务(Hold 走这条)。
func (f *Factory) acquire(ws workspace.Context, spec *WorkspaceSpec) *wsEntry {
	key := ws.Root()
	for {
		f.mu.Lock()
		e, ok := f.entries[key]
		if ok && e.closing != nil {
			// 这条记录正在被拆除。等它拆完再来 —— 那时会开出一个全新的 scope,
			// 而不是接手一个服务已经关掉的空壳。
			wait := e.closing
			f.mu.Unlock()
			<-wait
			continue
		}
		if !ok {
			e = &wsEntry{scope: f.proc.OpenWorkspace(ws)}
			f.entries[key] = e
		} else {
			// runtime 侧的引用计数也要 +1,两个计数始终同进同出。
			f.proc.OpenWorkspace(ws)
		}
		if spec != nil {
			f.ensureServicesLocked(e, *spec)
		}
		e.holds++
		f.mu.Unlock()
		return e
	}
}

// ensureServicesLocked 保证记录上有共享服务;已经有了就检查配置是否变了。调用方持锁。
func (f *Factory) ensureServicesLocked(e *wsEntry, spec WorkspaceSpec) {
	fp := specFingerprint(spec)
	if e.svc == nil {
		e.svc = f.startWorkspaceServices(e.scope, spec)
		e.fingerprint = fp
		return
	}
	if e.fingerprint != fp {
		// 共享服务正被别的会话用着,不能就地换配置。静默忽略是原来的行为,也正是
		// 复核点名的问题:用户改了 LSP / CodeGraph 设置,却看不出来为什么没生效。
		slog.Warn("boot: 工作区共享服务已在运行,本次的配置改动要等最后一个会话关闭后才生效",
			"workspace", spec.Root, "holders", e.holds)
	}
}

// specFingerprint 概括 WorkspaceSpec 里**会影响共享服务**的那几项。只包含这几项是
// 有意的:别的字段变了不影响已启动的服务,把它们算进来只会制造假警报。
func specFingerprint(spec WorkspaceSpec) string {
	if spec.Config == nil {
		return "no-config|" + spec.Root
	}
	fp := fmt.Sprintf("root=%s|hostIntel=%t|lsp=%t|codegraph=%t",
		spec.Root, spec.HostProvidesCodeIntel,
		spec.Config.LSP.Enabled, spec.Config.Codegraph.Enabled)
	// 必须排序:LSPSpecs 的顺序不稳定(它从 map 出来),不排序的话同一份配置会算出
	// 不同的指纹,于是每开一个会话都弹一次"配置改了"的假警报。这个 bug 是被
	// TestFingerprintIgnoresSettingsThatDoNotAffectSharedServices 抓到的。
	servers := LSPSpecs(spec.Config.LSP)
	parts := make([]string, 0, len(servers))
	for _, srv := range servers {
		parts = append(parts, srv.LanguageID+":"+srv.Command)
	}
	sort.Strings(parts)
	return fp + "|" + strings.Join(parts, ",")
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
	// 与 OpenWorkspace 走同一套记录:否则 Hold 会在 Factory 的账本之外偷偷把一个
	// scope 复活,那正是两个真源的另一种形态。spec 为 nil = 只占生命周期,不启动服务。
	e := f.acquire(ws, nil)
	return &WorkspaceHandle{f: f, scope: e.scope, key: ws.Root()}
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
		// 语言服务器也是子进程:`.env` 不再 os.Setenv 之后,工作区的值只能从这份
		// 显式环境进来(复核 C1)。
		mgr := lsp.NewManagerWithEnv(spec.Root, LSPSpecs(spec.Config.LSP), spec.Config.Env().Environ())
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
