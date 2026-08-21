package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/account"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
	"reasonix/internal/i18n"
)

// eventChannel is the Wails runtime event name the frontend subscribes to for the
// agent's typed event stream. One channel carries every event kind; the payload's
// `kind` field discriminates — the desktop analogue of the serve transport's SSE
// `data:` frames.
const eventChannel = "agent:event"

// App is the Wails-bound application object: the desktop frontend's command
// surface. Its exported methods (Submit/Cancel/Approve/…) are generated into JS
// bindings and call straight through to one transport-agnostic control.Controller
// — the same controller the chat TUI and the HTTP/SSE server drive, assembled by
// the shared internal/boot. Events flow the other way: the controller emits to an
// eventSink that forwards each one to the frontend via the Shell(Wails 事件 / SSE)。
type App struct {
	ctx context.Context

	// shell 是宿主外壳(见 shell.go):Wails 原生窗口或 Web 模式的 HTTP/SSE 服务。
	// 事件推送、原生对话框、开外链、窗口操作全部经它,App 本身不认识 Wails。
	// 没装它时统一由 a.sh() 兜底成 noopShell。
	shell Shell

	// 多标签多任务(像 Codex / Claude Code):每个 tab 一个独立 controller + sink +
	// session 文件 + workspace root,后台 tab 的 controller 照常在自己的 goroutine
	// 里跑,事件发到独立通道 agent:event:<tabID> —— 所以多个任务可以「真并行」。
	//
	// tabs 是标签运行时的**唯一真源**:App 上不再镜像活动标签的
	// ctrl/sink/model/label/ready/startupErr。「活动标签」只是 tabs 里的一个 id,
	// 沿用旧签名、不带 tabID 的前端入口在这一层解析成活动 id 再委托下去。
	tabs *tabManager

	// factory 持有「进程级 / 工作区级」资源(每个项目共享的 LSP manager 与 CodeGraph
	// 守护进程),由所有标签共用。它必须共享:同一个项目开两个标签时,关掉其中一个
	// 不能停掉另一个还在用的语言服务器和符号守护进程(Plan 05 / A06)。
	//
	// 在 NewApp 里就建好(而不是 startup),这样任何 goroutine 读它时都已经写完,
	// 不需要再加一把锁;它的生命周期由 shutdown 里的 Close 结束,不依赖 ctx 取消。
	// 单元测试(newBareApp)不设它 —— nil 表示「每个 controller 自带一份私有的」,
	// 即 Plan 05 之前的行为。
	factory *boot.Factory

	// gateway 是平台账号运行时:登录后的网关 URL / token / 档位。所有标签共享同一个
	// 对象,token 续期只改它,已建好的会话下一次请求就用上新 token —— 不再靠
	// os.Setenv 三个环境变量当状态总线(Plan 09 / A12)。
	gateway *account.Gateway

	// 领域服务。App 从 Plan 06 起只是它们的 transport facade:除了 ctx / shell /
	// tabs / factory 这几样装配层的东西,它自己不再持有任何领域状态,也不再有锁 ——
	// 硬件的互斥槽、MCP 的视图缓存、串口连接、自动落盘的单飞表、当前选中的项目
	// 文件夹,全部搬进了各自的服务(app_facade_test.go 会在它们爬回来时报错)。
	hw       *hardwareService
	mcp      *mcpService
	files    *fileService
	memory   *memoryService
	sessions *sessionService

	// rt 装配与重建标签的 controller,并持有「当前选中的项目文件夹」。
	rt *tabRuntimeService

	// serial 是「串口监视器」面板的常驻双向连接(见 serial_service.go)。串口状态
	// 归它,不再散在 App 上。
	serial *serialService
}

// TabMeta 是给前端的标签快照。
type TabMeta struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Ready      bool   `json:"ready"`
	StartupErr string `json:"startupErr,omitempty"`
	Active     bool   `json:"active"`
}

// NewApp constructs the bound object. The controller is built later, in startup,
// once the Wails context exists.
func NewApp() *App {
	a := &App{
		tabs:    newTabManager(),
		factory: boot.NewFactory(context.Background()),
	}
	// 按构建标签选宿主实现:!web → wailsShell,web → webShell。
	a.shell = newShell(a)
	a.wireServices()
	a.hw = newHardwareService(a.workspaceRoot, a.activeCtrl, a.serial)
	// 主标签在这里就注册进 tabs 并设为活动:标签运行时只有这一份,不再有「App 上的
	// 活动镜像」。它的 controller 由 buildController 异步装配,期间 Ready=false。
	a.tabs.Register(&tabRuntime{id: "main", kind: "chat", sink: newEventSink(nil, a, "main")})
	return a
}

// wireServices 把领域服务接到 App 上。App 从 Plan 06 起只是这些服务的 transport
// facade,所以「没接服务的 App」不是一个有效对象 —— 单元测试也走这里(见
// newBareApp),而不是自己拼一个半成品。
//
// 依赖用函数注入而不是直接传值:shell 在此之前一行才装上,ctx 要等 startup,
// 而 workspaceRoot / activeCtrl 本来就是「每次调用现算」的。
func (a *App) wireServices() {
	if a.gateway == nil {
		// 兼容:进程若带着 ONECREAT_GATEWAY_* 启动(旧版脚本 / 打包器 / 测试),导入一次;
		// 之后这个对象就是唯一真源,env 不再被读回(Plan 09 / A12)。
		a.gateway = account.FromEnv()
	}
	a.serial = newSerialService(a.sh)
	a.mcp = newMCPService(a.activeCtrl)
	a.files = newFileService(a.workspaceRoot)
	a.memory = newMemoryService(a.activeCtrl)
	a.sessions = newSessionService(a.tabs)
	a.rt = newTabRuntimeService(a.tabs, a.factory, a.gateway, a.sh, func() context.Context { return a.ctx }, a.serialReleaseForToolUse)
}

// startup runs once the webview process is up, before the frontend can issue any
// bound call. It captures the Wails context (needed for EventsEmit), points the
// sink at it, then kicks off the entire initialization (workspace, config, build)
// in a background goroutine so the webview loads immediately. The frontend polls
// Meta() and sees Ready flip to true once the controller is assembled. RequireKey
// is false so a missing API key opens the window in a "set your key" state rather
// than failing to launch; a build error is surfaced through Meta instead of
// crashing the window.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// 主标签的 sink 在 NewApp 里就建好了(那时还没有 Wails ctx),这里补上。
	a.tabs.Update("main", func(rt *tabRuntime, _ bool) {
		if rt.sink != nil {
			rt.sink.ctx = ctx
		}
	})
	a.showMainWindow(ctx)
	go func() {
		time.Sleep(500 * time.Millisecond)
		a.showMainWindow(ctx)
	}()

	// Everything else — workspace resolution, config loading, i18n setup, and
	// boot.Build — runs in the background so the webview appears instantly.
	// During this window Meta().Ready is false and the frontend shows a loading
	// state; bound calls are no-ops (ctrl is nil).
	go a.buildController()
}

func (a *App) domReady(ctx context.Context) {
	a.showMainWindow(ctx)
}

func (a *App) showMainWindow(context.Context) {
	a.sh().RaiseWindow()
}

// buildController runs the full initialization sequence in a background goroutine:
// workspace resolution, config loading, i18n setup, and boot.Build. On success it
// wires up the controller and flips ready; on failure it stores the error so
// Meta().StartupErr surfaces it.
func (a *App) buildController() {
	// A GUI launch starts in "/" (read-only). Resolve the folder this launch
	// opens in (the remembered one, else home) — that Context is what config,
	// memory, skills and the tools resolve against from here on; it also chdirs
	// there so the remaining cwd-relative corners of the app land somewhere
	// writable. Switching projects later re-points the Context, never the process.
	ws := resolveStartupWorkspace()

	// Drive the Go-side catalogue (i18n.M) from the configured language so the
	// backend-provided slash UI — command descriptions, sub-command hints,
	// listing notices — comes through localized, matching the frontend.
	if cfg, err := config.LoadIn(ws); err == nil {
		i18n.DetectLanguage(cfg.Language)
	}

	a.rt.SetWorkspace(ws)
	// 主标签在 NewApp 里已注册;这里只补上它的项目根。
	a.tabs.Update("main", func(rt *tabRuntime, _ bool) { rt.ws = ws })

	// 默认 local-first:清掉任何旧网关 env,让首个 controller 使用本地 provider/API key。
	// 只有显式 ONECREAT_ACCOUNT_MODE=platform 时才续期并改走平台 AI 网关。
	if platformAccountEnabled() {
		ensureFreshToken(a.gateway)
	}
	applyGatewaySession(a.gateway)

	a.buildTab("main")

	if platformAccountEnabled() {
		// 后台定时续期:长时间开着 app 也不会因 access token 过期而掉线。
		go a.tokenRefreshLoop()
	}
}

// tokenRefreshLoop 每 10 分钟尝试续期网关 token。ensureFreshToken 内部判断「快过期才真刷」,
// 所以多数 tick 只是廉价检查;refresh_token 缺失 / 失效时静默跳过。随 app 退出而结束。
func (a *App) tokenRefreshLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			ensureFreshToken(a.gateway)
		}
	}
}

// CreateTab 新建一个独立任务标签并设为活动;controller 异步装配(期间 Ready=false,
// 前端显示 loading),装配完发 agent:ready:<id>。后台标签照常在自己的 goroutine 里
// 跑、事件发到自己的通道,所以多个标签可真并行(像 Codex / Claude Code 的多任务)。
func (a *App) CreateTab(kind string) (TabMeta, error) {
	if a.ctx == nil {
		return TabMeta{}, fmt.Errorf("app not ready")
	}
	if kind == "" {
		kind = "chat"
	}
	id := a.tabs.NextID()
	// 新标签开在【当前选中的】项目上,并把这个 root 固定进标签自己的运行时:
	// 之后用户换项目只影响新标签和活动标签,这个标签继续读写它开的那个目录。
	// Register 同时把它设为活动(用户刚开它就是要用)——「活动」只是一个 id,
	// 没有需要一并重指的镜像状态。
	a.tabs.Register(&tabRuntime{
		id: id, kind: kind, ws: a.workspace(),
		sink: newEventSink(a.ctx, a, id),
	})

	go a.buildTab(id)
	return TabMeta{ID: id, Kind: kind, Ready: false, Active: true}, nil
}

// SetActiveTab 把「活动标签」指向目标标签:不带 tabID 的既有会话类方法随之作用到
// 该标签。前端在切换标签、以及对某标签发指令前调用它。未知 id 是空操作。
//
// 切换只改一个 id —— 没有 controller / sink / model 需要跟着重指,所以也就不存在
// 「漏同步某个镜像字段」这类 bug。
func (a *App) SetActiveTab(id string) { a.tabs.SetActive(id) }

// CloseTab 关闭一个标签:从注册表移除,快照并关掉它的 controller;若关的是活动标签,
// 自动切到末尾的另一个标签。
func (a *App) CloseTab(id string) {
	v, _, ok := a.tabs.Close(id)
	if !ok {
		return
	}
	// Snapshot/Close 是慢操作,在 tabs 锁外做。若此刻 buildTab 还在跑(ctrl==nil),
	// 这个标签已经从注册表里被移除 —— 「不在注册表里」本身就是关闭信号(Plan 02
	// 删掉了冗余的 closed 标志,两个信号意味着两处要同步)。buildTab 完成时写回落空,
	// 于是它自行 Close 掉刚建好的 controller 并不发 ready,避免 controller 泄漏(A6)。
	if v.ctrl != nil {
		_ = v.ctrl.Snapshot()
		v.ctrl.Close()
	}
}

// ListTabs 返回所有标签快照(按打开顺序),供前端渲染标签栏。
func (a *App) ListTabs() []TabMeta { return a.tabs.List() }

// shutdown snapshots the conversation and stops plugin subprocesses on close.
// 遍历所有标签(不只活动镜像):多标签并行时,后台标签进行中的最后一轮也要落盘,
// 它们的 MCP stdio 子进程也要关闭,否则退出/自更新会丢数据 + 留孤儿子进程(A7)。
// Quit 请求优雅退出整个程序。前端「退出 OneCreat」按钮调它(Web 模式);桌面版按钮隐藏,
// 但方法照常可用。真正的关闭动作交给宿主外壳:Wails 关窗触发 OnShutdown;Web 通知运行
// 循环走 app.shutdown + 停服。这里不直接 shutdown,好让本次 RPC 先把 200 回给浏览器。
func (a *App) Quit() { a.sh().Quit() }

func (a *App) shutdown(context.Context) {
	for _, ctrl := range a.tabs.Controllers() {
		_ = ctrl.Snapshot()
		ctrl.Close()
	}
	// 每个 controller 关闭时释放它自己那份工作区引用;最后一个走掉时工作区随之关闭。
	// 这里再关一次工厂,兜住「没有任何标签、或某个标签没建起来」时仍开着的进程级作用域。
	if a.factory != nil {
		a.factory.Close()
	}
}

// --- bound command surface (frontend → controller) ---
// Each method guards on a nil controller so a pre-startup or failed-build call is
// a no-op, never a panic.

// ctrlForTab 按标签 id 取该标签的 controller。空 id 解析为活动标签,兼容尚未带
// tabID 的旧路径。用于审批/问答/门控等「必须打到事件来源标签」的方法(A2/A8)。
func (a *App) ctrlForTab(tabID string) *control.Controller { return a.tabs.Ctrl(tabID) }

// activeCtrl 是活动标签的 controller(未就绪 / 无标签时为 nil)。不带 tabID 的旧
// 前端入口都经它解析,这是「活动标签」在应用层唯一的落点。
func (a *App) activeCtrl() *control.Controller { return a.tabs.Ctrl("") }

// Submit runs raw user input as a turn; slash commands and @-references are
// resolved by the controller. Output arrives asynchronously on eventChannel.
func (a *App) Submit(input string) {
	trimmed := strings.TrimSpace(input)
	if a.gatewayActive() && isGatewayManagedSlash(trimmed) {
		a.notice("AI 由 OneCreat 平台智能档位统一调度；当前账号只显示档位，不显示底层模型、服务商或路由。")
		return
	}
	if trimmed == "/effort" || strings.HasPrefix(trimmed, "/effort ") {
		a.runEffortCommand(trimmed)
		return
	}
	ctrl := a.activeCtrl()
	if ctrl != nil {
		ctrl.Submit(input)
	}
}

// SubmitDisplay runs input as a turn while recording a shorter UI-only display
// string for the saved desktop transcript. The model still receives input.
func (a *App) SubmitDisplay(display, input string) {
	trimmed := strings.TrimSpace(input)
	if a.gatewayActive() && isGatewayManagedSlash(trimmed) {
		a.notice("AI 由 OneCreat 平台智能档位统一调度；当前账号只显示档位，不显示底层模型、服务商或路由。")
		return
	}
	// 取一次 ctrl 局部变量后全程用它,消除「检查与 Submit 之间 controller 被换/Close」
	// 的 TOCTOU 数据竞争(A9)。SubmitDisplay 是知识库增强与硬件面板提交的主路径,频繁调用。
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return
	}
	sessionPath := ctrl.SessionPath()
	_ = recordSessionDisplay(a.sessions.reg, sessionPath, input, display)
	// 记录该 session 创建时所在的 workspace,用于侧栏按文件夹分组。
	// 只在首次有效消息时落,后续 workspace 切换不影响归属。
	if root := a.workspaceRoot(); root != "" {
		_ = rememberSessionCwd(a.sessions.reg, sessionPath, root)
	}
	ctrl.Submit(input)
}

// Cancel aborts the in-flight turn.
func (a *App) Cancel() {
	ctrl := a.activeCtrl()
	if ctrl != nil {
		ctrl.Cancel()
	}
}

// Approve answers a pending approval_request by ID: allow runs the call, session
// also remembers the grant for the rest of the session. tabID 指定事件来源标签——
// 后台标签的审批必须打到它自己的 controller,而不是当前活动标签(A2)。
func (a *App) Approve(tabID, id string, allow, session bool) {
	if ctrl := a.ctrlForTab(tabID); ctrl != nil {
		ctrl.Approve(id, allow, session)
	}
}

// SetPlanMode toggles read-only plan mode for one tab. 记录到标签的 want 状态(即使
// controller 还在异步装配也不丢),并施加到已就绪的 controller——避免「pill 显示只读、
// 实际可写」的安全错觉(A8)。
func (a *App) SetPlanMode(tabID string, on bool) {
	var ctrl *control.Controller
	a.tabUpdate(tabID, func(rt *tabRuntime) {
		rt.wantPlan = on
		ctrl = rt.ctrl
	})
	if ctrl != nil {
		ctrl.SetPlanMode(on)
	}
}

// SetCoachMode 设置某标签的「协作模式」persona(空串=默认无 persona)。preamble 随每个
// turn 注入(见 Compose),按 tabID 路由并记录 want 状态(A8)。
func (a *App) SetCoachMode(tabID, preamble string) {
	var ctrl *control.Controller
	a.tabUpdate(tabID, func(rt *tabRuntime) {
		rt.coachWant = strings.TrimSpace(preamble)
		ctrl = rt.ctrl
	})
	if ctrl != nil {
		ctrl.SetCoachMode(preamble)
	}
}

// QuestionAnswer is the frontend's reply to one question in an ask_request.
type QuestionAnswer struct {
	QuestionID string   `json:"questionId"`
	Selected   []string `json:"selected"`
}

// AnswerQuestion resolves a pending ask_request (the `ask` tool) by ID with the
// user's selections per question. tabID 指定事件来源标签(A2)。
func (a *App) AnswerQuestion(tabID, id string, answers []QuestionAnswer) {
	ctrl := a.ctrlForTab(tabID)
	if ctrl == nil {
		return
	}
	out := make([]event.AskAnswer, len(answers))
	for i, an := range answers {
		out[i] = event.AskAnswer{QuestionID: an.QuestionID, Selected: an.Selected}
	}
	ctrl.AnswerQuestion(id, out)
}

// PendingPrompts 返回某标签当前「已发出、尚未应答」的审批 / ask(promptMu 保证同一时刻
// 至多一个)。切回标签时前端据此补显审批弹窗——后台标签的审批事件在它无人订阅时发出,
// 重订阅不会重放,只能靠这个查询补回来(A2)。
type PendingPrompts struct {
	Approvals []wireApproval `json:"approvals"`
	Asks      []wireAsk      `json:"asks"`
}

func (a *App) PendingPrompts(tabID string) PendingPrompts {
	out := PendingPrompts{Approvals: []wireApproval{}, Asks: []wireAsk{}}
	ctrl := a.ctrlForTab(tabID)
	if ctrl == nil {
		return out
	}
	for _, ap := range ctrl.PendingApprovals() {
		out.Approvals = append(out.Approvals, wireApproval{ID: ap.ID, Tool: ap.Tool, Subject: ap.Subject})
	}
	for _, ak := range ctrl.PendingAsks() {
		out.Asks = append(out.Asks, *toWireAsk(ak))
	}
	return out
}

// Compact runs one compaction pass on demand.
// Compact runs a plain compaction pass (the "compact now" button). Focus-guided
// compaction goes through Submit("/compact <focus>") instead.
func (a *App) Compact() error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	return ctrl.Compact(a.ctx, "")
}

// NewSession snapshots the current conversation and rotates to a fresh one.
func (a *App) NewSession() error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	return ctrl.NewSession()
}

// CheckpointMeta summarises one rewind point (a user turn) for the desktop.
type CheckpointMeta struct {
	Turn   int      `json:"turn"`
	Prompt string   `json:"prompt"`
	Files  []string `json:"files"` // paths changed during the turn
	Time   int64    `json:"time"`  // unix milliseconds
}

// Checkpoints lists the session's rewind points, oldest first, for the rewind UI.
func (a *App) Checkpoints() []CheckpointMeta {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return []CheckpointMeta{}
	}
	metas := ctrl.Checkpoints()
	out := make([]CheckpointMeta, 0, len(metas))
	for _, m := range metas {
		out = append(out, CheckpointMeta{Turn: m.Turn, Prompt: m.Prompt, Files: m.Paths, Time: m.Time.UnixMilli()})
	}
	return out
}

// Rewind restores the session to the start of turn. scope is "code",
// "conversation", or "both" (anything else is treated as "both"). The frontend
// re-reads History after this resolves.
func (a *App) Rewind(turn int, scope string) error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	s := control.RewindBoth
	switch scope {
	case "code":
		s = control.RewindCode
	case "conversation":
		s = control.RewindConversation
	}
	return ctrl.Rewind(turn, s)
}

// Fork branches the conversation at the start of turn into a new session
// (preserving the current one), keeping code intact, and switches to the branch.
// The frontend re-reads History after this resolves.
func (a *App) Fork(turn int) error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	_, err := ctrl.Fork(turn)
	return err
}

// SummarizeFrom / SummarizeUpTo compress the conversation from / up to the start
// of turn into one summary (Claude Code's "summarize from/up to here"), keeping
// code intact. The frontend re-reads History after this resolves.
func (a *App) SummarizeFrom(turn int) error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeFrom(a.ctx, turn)
}

func (a *App) SummarizeUpTo(turn int) error {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeUpTo(a.ctx, turn)
}

// ContextInfo is the prompt-vs-window gauge payload. Both zero means no data yet.
type ContextInfo struct {
	Used   int `json:"used"`
	Window int `json:"window"`
}

// ContextUsage returns the latest context-window gauge numbers.
func (a *App) ContextUsage() ContextInfo {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return ContextInfo{}
	}
	used, window := ctrl.ContextSnapshot()
	return ContextInfo{Used: used, Window: window}
}

// BalanceInfo is the wallet-balance readout for the status bar. Available is true
// only when a balance was fetched; Display is the formatted amount (e.g. "¥110.00")
// and is "" when the active provider declares no balance_url — the frontend then
// omits the readout. Err carries a fetch failure for an optional tooltip.
type BalanceInfo struct {
	Available bool   `json:"available"`
	Display   string `json:"display"`
	Err       string `json:"err,omitempty"`
}

// Balance queries the active provider's wallet balance (a network call). It
// returns an empty (unavailable) readout when no provider balance_url is set, the
// controller is down, or the fetch fails — so the status bar simply shows nothing
// rather than an error.
func (a *App) Balance() BalanceInfo {
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return BalanceInfo{}
	}
	b, err := ctrl.Balance(a.ctx)
	if err != nil {
		return BalanceInfo{Err: err.Error()}
	}
	if b == nil {
		return BalanceInfo{} // provider declares no balance endpoint
	}
	return BalanceInfo{Available: true, Display: b.Display()}
}

// JobView is one running background job (bash/task started with
// run_in_background) for the status-bar indicator.
type JobView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
}

// Jobs returns the still-running background jobs for the status bar. It refreshes
// on demand (mount, turn end, and on each notice the frontend receives).
func (a *App) Jobs() []JobView {
	out := []JobView{}
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return out
	}
	for _, v := range ctrl.Jobs() {
		out = append(out, JobView{ID: v.ID, Kind: v.Kind, Label: v.Label, Status: v.Status, StartedAt: v.StartedAt})
	}
	return out
}

// Meta describes the session for the frontend's header and status line.
type Meta struct {
	Label        string `json:"label"`
	Ready        bool   `json:"ready"`
	StartupErr   string `json:"startupErr,omitempty"`
	EventChannel string `json:"eventChannel"`
	Cwd          string `json:"cwd"`
	Bypass       bool   `json:"bypass"`   // YOLO mode on (auto-approve every tool call)
	PlanMode     bool   `json:"planMode"` // 该标签 controller 的真实 plan(只读)门控状态
	Running      bool   `json:"running"`  // 该标签是否有 turn 正在跑(切回正在跑的标签时恢复真值)
}

// Meta reports the model label, readiness, any startup error, the working
// directory (for the status line), and the runtime event channel the frontend
// subscribes to. Bypass/PlanMode/Running 读自活动标签 controller 的真实状态——切回
// 标签时前端据此恢复 pill 与 spinner,而不是凭全局 UI 态猜测(A3/A8)。
func (a *App) Meta() Meta {
	v, _ := a.tabs.View("")
	label, startupErr, ready, ctrl := v.label, v.startupErr, v.ready, v.ctrl
	if a.gatewayActive() {
		label = "OneCreat 智能档位"
	}
	cwd := a.workspaceRoot()
	return Meta{
		Label:        label,
		Ready:        ready,
		StartupErr:   startupErr,
		EventChannel: eventChannel,
		Cwd:          cwd,
		Bypass:       ctrl != nil && ctrl.Bypass(),
		PlanMode:     ctrl != nil && ctrl.PlanMode(),
		Running:      ctrl != nil && ctrl.Running(),
	}
}

// SetBypass toggles YOLO mode for the session: auto-approve every tool call
// (writers and bash run without asking). Deny rules still apply. Runtime-only —
// not written to config, so it resets on relaunch.
func (a *App) SetBypass(tabID string, on bool) {
	var ctrl *control.Controller
	a.tabUpdate(tabID, func(rt *tabRuntime) {
		rt.wantBypass = on
		ctrl = rt.ctrl
	})
	if ctrl != nil {
		ctrl.SetBypass(on)
	}
}

// CapabilitiesView is the MCP & Skills drawer's data: connected/failed MCP
// servers and the discoverable skills, the GUI counterpart to `/mcp` + `/skill`.
type CapabilitiesView struct {
	Servers    []ServerView    `json:"servers"`
	Skills     []SkillView     `json:"skills"`
	SkillRoots []SkillRootView `json:"skillRoots"`
}

// ServerView is one MCP server for the drawer. Status is "connected" (with
// tool/prompt/resource counts) or "failed" (with the connection error).
type ServerView struct {
	Name      string     `json:"name"`
	Transport string     `json:"transport"`
	Status    string     `json:"status"`
	Tools     int        `json:"tools"`
	Prompts   int        `json:"prompts"`
	Resources int        `json:"resources"`
	Error     string     `json:"error,omitempty"`
	ToolList  []ToolView `json:"toolList,omitempty"`
}

type ToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Capabilities projects the session's MCP servers (connected + failed) and skills
// for the MCP & Skills drawer. Non-nil slices so the frontend can map over them.
//
// 这是一处纯 DTO 组装:服务器那半来自 mcpService,技能那半来自活动标签的 controller
// 与当前项目的技能根 —— App 只负责把两半拼成前端要的一个视图。
func (a *App) Capabilities() CapabilitiesView {
	out := CapabilitiesView{Servers: []ServerView{}, Skills: []SkillView{}, SkillRoots: []SkillRootView{}}
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return out
	}
	out.Servers = a.mcp.ServerViews(ctrl)
	for _, s := range ctrl.Skills() {
		out.Skills = append(out.Skills, SkillView{
			Name: s.Name, Description: s.Description,
			Scope: string(s.Scope), RunAs: string(s.RunAs),
		})
	}
	out.SkillRoots = skillRootsView(a.workspace())
	return out
}

// --- helpers for the 3 buttons ---

// --- memory panel (frontend ⇄ controller) ---

// eventSink is the controller's event.Sink in desktop mode: it forwards every
// agent event to the frontend as one shell event, stamped and JSON-shaped by its
// own eventwire.Stamper. It is a type distinct from App so App's bound method set
// stays the clean command surface — Emit must not be exposed to JS. Emit runs on the agent goroutine; Shell.Emit
// is goroutine-safe, and the ctx guard covers the brief window before startup
// assigns it.
type eventSink struct {
	ctx   context.Context
	app   *App
	tabID string // 该 sink 归属的标签;事件发到 agent:event:<tabID>,空则发旧的全局通道
	// stamp 给这条标签流编号:schemaVersion / sequence / eventId / timestamp。
	// 每个标签一条独立的流,所以每个 sink 一个 stamper(Plan 10 / A11)。
	stamp *eventwire.Stamper
}

// newEventSink 建一个标签的事件出口。
func newEventSink(ctx context.Context, app *App, tabID string) *eventSink {
	return &eventSink{ctx: ctx, app: app, tabID: tabID, stamp: eventwire.NewStamper("", tabID)}
}

func (s *eventSink) Emit(e event.Event) {
	if s.ctx != nil && s.app != nil {
		ch := eventChannel
		if s.tabID != "" {
			ch = eventChannel + ":" + s.tabID
		}
		s.app.sh().Emit(ch, s.stamp.Wire(e))
	}
	// Persist after each turn so a force-kill of a long session loses at most the
	// in-flight prompt, not every turn back to the last workspace switch.
	// 存的是「本 sink 所属标签」的 session,后台标签完成一轮也各存各的。
	if e.Kind == event.TurnDone && s.app != nil {
		s.app.sessions.ScheduleSnapshot(s.tabID)
	}
}
