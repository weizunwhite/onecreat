package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
	"reasonix/internal/workspace"
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
	// 单元测试里的裸 &App{} 不设它,统一由 a.sh() 兜底成 noopShell。
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
	// 单元测试里的裸 &App{} 不设它 —— nil 表示「每个 controller 自带一份私有的」,
	// 即 Plan 05 之前的行为。
	factory *boot.Factory

	// hw 是硬件面板的后端(检测 / 装工具链 / 编译 / 烧录 / 证据链),mcp 是 MCP
	// 抽屉的后端。两者各自持有自己的状态与锁 —— 过去它们的互斥槽和视图缓存都挤在
	// App 的字段里,和「当前项目文件夹」共用同一把 a.mu。
	hw       *hardwareService
	mcp      *mcpService
	files    *fileService
	memory   *memoryService
	sessions *sessionService

	// mu 只保护下面这个 App 自身的字段(当前选中的项目文件夹)。
	// 标签运行时由 tabs 自己的锁保护 —— 两把锁互不嵌套。
	mu sync.RWMutex

	// serial 是「串口监视器」面板的常驻双向连接(见 serial_service.go)。串口状态
	// 归它,不再散在 App 上。
	serial *serialService

	// ws 是「当前选中的项目文件夹」:新建标签在这里开,原生对话框以它为起点,
	// UI 的文件浏览/知识库/硬件面板按它解析相对路径。它不是 tabs 的镜像 —— 每个
	// 标签在 tabRuntime.ws 里持有自己【实际】的 root,切换项目只改这里和活动标签,
	// 后台标签继续读写它们自己的目录。由 a.mu 保护。
	ws workspace.Context
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

// workspace returns the currently selected project folder. Callers that resolve
// a path for the UI (file browsing, pickers, the hardware panel) use it instead
// of os.Getwd: the process working directory is fixed at startup and no longer
// tracks which project the user has open.
func (a *App) workspace() workspace.Context {
	if a == nil {
		return workspace.Context{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ws
}

// workspaceRoot is workspace().Root() with a process-cwd fallback, for the call
// sites that need a concrete directory (a subprocess argument, a browse base).
// The fallback only fires in tests that construct a bare &App{}.
func (a *App) workspaceRoot() string {
	if root := a.workspace().Root(); root != "" {
		return root
	}
	wd, _ := os.Getwd()
	return wd
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
	// 领域服务:各自持有自己的状态,App 只转发。shell 用函数取而不是直接传值,
	// 因为它在这一行之前才装上,且裸 &App{} 靠 a.sh() 兜底成 noopShell。
	a.serial = newSerialService(a.sh)
	a.mcp = newMCPService(a.activeCtrl)
	a.files = newFileService(a.workspaceRoot)
	a.memory = newMemoryService(a.activeCtrl)
	a.sessions = newSessionService(a.tabs)
	a.hw = newHardwareService(a.workspaceRoot, a.activeCtrl, a.serial)
	// 主标签在这里就注册进 tabs 并设为活动:标签运行时只有这一份,不再有「App 上的
	// 活动镜像」。它的 controller 由 buildController 异步装配,期间 Ready=false。
	a.tabs.Register(&tabRuntime{id: "main", kind: "chat", sink: &eventSink{app: a, tabID: "main"}})
	return a
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

	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	// 主标签在 NewApp 里已注册;这里只补上它的项目根。
	a.tabs.Update("main", func(rt *tabRuntime, _ bool) { rt.ws = ws })

	// 默认 local-first:清掉任何旧网关 env,让首个 controller 使用本地 provider/API key。
	// 只有显式 ONECREAT_ACCOUNT_MODE=platform 时才续期并改走平台 AI 网关。
	if platformAccountEnabled() {
		ensureFreshToken()
	}
	applyGatewayEnvFromSession()

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
			ensureFreshToken()
		}
	}
}

// buildTab 为一个标签装配它自己的 controller(boot.Build 可能较慢,所以由 CreateTab
// 放到 goroutine 里调)。装配完成后写回该标签的运行时,并发 agent:ready:<tabID>
// 通知前端该标签可用。
//
// 全程按 tabID 路由、不碰任何「活动标签」状态:build 期间用户切走或再切回来都不影响
// 结果落在哪个标签上(A5)。boot.Build 是秒级操作,绝不在 tabs 锁内调用。
func (a *App) buildTab(tabID string) {
	v, ok := a.tabs.View(tabID)
	if !ok {
		return
	}
	// 这个标签的项目文件夹 —— 不是进程 cwd。config、模型默认值、以及下面 boot.Build
	// 装配出的工具/bash/MCP 全部按它解析,所以两个标签可以同时开在不同项目上。
	ws := v.ws
	sink := v.sink

	// 解析该文件夹的默认模型为规范的 "provider/model"。
	model := ""
	if cfg, err := config.LoadIn(ws); err == nil {
		model = cfg.DefaultModel
		if e, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			model = e.Name + "/" + e.Model
		}
	}

	ctrl, err := boot.Build(a.ctx, boot.Options{Model: model, RequireKey: false, Sink: sink, Workspace: ws, PreToolUse: a.serialReleaseForToolUse, Factory: a.factory})
	if err != nil {
		// 标签已在 build 期间被关闭时 Update 找不到它:什么都不写、不发 ready(A6)。
		if !a.tabs.Update(tabID, func(rt *tabRuntime, _ bool) {
			rt.startupErr = err.Error()
			rt.ready = true
		}) {
			return
		}
		a.sh().Emit("agent:ready:"+tabID, nil)
		return
	}

	// 「是否被采纳」必须在同一次加锁里定下来:分两次读会与 CloseTab 撞出
	// 「两边都以为该由自己 Close」的双重 Close。CloseTab 从注册表删除标签,所以
	// Update 找得到 ⟺ 标签还活着 ⟺ 这个 controller 归它。
	var wantPlan, wantBypass bool
	var coachWant string
	adopted := a.tabs.Update(tabID, func(rt *tabRuntime, _ bool) {
		rt.ctrl = ctrl
		rt.model = model
		rt.label = ctrl.Label()
		rt.ready = true
		rt.startupErr = ""
		wantPlan, wantBypass, coachWant = rt.wantPlan, rt.wantBypass, rt.coachWant
	})
	if !adopted {
		// 标签在 build 期间被关闭:关掉刚建好的 controller(含 MCP 子进程 / session /
		// goroutine),不发 ready —— 否则这个 controller 永远没人 Close(A6)。
		ctrl.Close()
		return
	}

	// Desktop is interactive: route "ask" gate decisions to the frontend as
	// approval_request events, answered via Approve.
	ctrl.EnableInteractiveApproval()

	// 施加标签期望的运行时门控:新标签异步装配完成后,把「切标签 / 新建标签时已记录」的
	// plan/YOLO/coach 状态真正作用到 controller,保证底部 pill 与实际门控一致——避免
	// 「显示 plan(只读),实际 normal(会改文件)」的安全错觉(A8)。
	if wantPlan {
		ctrl.SetPlanMode(true)
	}
	if wantBypass {
		ctrl.SetBypass(true)
	}
	if coachWant != "" {
		ctrl.SetCoachMode(coachWant)
	}

	// Land auto-save in a fresh session file (same as a fresh chat/serve start).
	if dir := ctrl.SessionDir(); dir != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(dir, ctrl.Label()))
	}

	// Notify the frontend this tab is ready — it re-fetches Meta/Context/History.
	a.sh().Emit("agent:ready:"+tabID, nil)
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
		sink: &eventSink{ctx: a.ctx, app: a, tabID: id},
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
	// Snapshot/Close 是慢操作,在 tabs 锁外做。若此刻 buildTab 还在跑
	// (ctrl==nil),Close 已经置了 closed=true,buildTab 完成时会自行 Close 掉刚
	// 建好的 controller 并不发 ready,避免 controller 泄漏(A6)。
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

// holdWorkspace 在「换掉某个标签的 controller」期间持住它的项目引用。
//
// 重建路径(SetModel / rebuildTabByID / 设置变更)都是先 Close 旧 controller、再 Build
// 新的:中间那一小段时间里没有任何会话引用这个工作区,引用计数会 1→0→1 —— 项目的
// 语言服务器和 CodeGraph 守护进程被停掉再立刻重启。持有一份显式引用就把意图写清楚了:
// 「我在换这个项目上的会话,项目本身别关」。裸 &App{}(无 factory)返回 nil,Release
// 对 nil 是空操作。
func (a *App) holdWorkspace(ws workspace.Context) *boot.WorkspaceHandle {
	if a == nil || a.factory == nil {
		return nil
	}
	return a.factory.Hold(ws)
}

// activeCtrl 是活动标签的 controller(未就绪 / 无标签时为 nil)。不带 tabID 的旧
// 前端入口都经它解析,这是「活动标签」在应用层唯一的落点。
func (a *App) activeCtrl() *control.Controller { return a.tabs.Ctrl("") }

func isGatewayManagedSlash(trimmed string) bool {
	return trimmed == "/model" || strings.HasPrefix(trimmed, "/model ") ||
		trimmed == "/effort" || strings.HasPrefix(trimmed, "/effort ")
}

// Submit runs raw user input as a turn; slash commands and @-references are
// resolved by the controller. Output arrives asynchronously on eventChannel.
func (a *App) Submit(input string) {
	trimmed := strings.TrimSpace(input)
	if gatewayActive() && isGatewayManagedSlash(trimmed) {
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
	if gatewayActive() && isGatewayManagedSlash(trimmed) {
		a.notice("AI 由 OneCreat 平台智能档位统一调度；当前账号只显示档位，不显示底层模型、服务商或路由。")
		return
	}
	// 取一次 ctrl 局部变量后全程用它,消除「检查与 Submit 之间 controller 被换/Close」
	// 的 TOCTOU 数据竞争(A9)。SubmitDisplay 是知识库增强与硬件面板提交的主路径,频繁调用。
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return
	}
	dir := config.SessionDir()
	sessionPath := ctrl.SessionPath()
	_ = recordSessionDisplay(dir, sessionPath, input, display)
	// 记录该 session 创建时所在的 workspace,用于侧栏按文件夹分组。
	// 只在首次有效消息时落,后续 workspace 切换不影响归属。
	if root := a.workspaceRoot(); root != "" {
		_ = rememberSessionCwd(dir, sessionPath, root)
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

// tabUpdate 在 tabs 锁内改一个标签的运行时(空 tabID=活动标签),并回报该标签是否
// 存在。门控类方法用它记录 want 状态,再在锁外施加到 controller。
func (a *App) tabUpdate(tabID string, fn func(rt *tabRuntime)) bool {
	return a.tabs.Update(tabID, func(rt *tabRuntime, _ bool) { fn(rt) })
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

type WorkspaceMeta struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// PickWorkspace opens a folder chooser and, on a pick, switches the agent to that
// project: it re-roots the process there, rebuilds the controller from that
// folder's reasonix.toml + REASONIX.md, and starts a fresh session — the desktop
// analogue of opening a different project. The new controller is built before the
// old one is torn down, so a folder whose config can't load leaves the current
// session untouched. Returns the chosen path ("" if cancelled).
func (a *App) PickWorkspace() (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	dir, err := a.sh().OpenDirectoryDialog(DialogOptions{
		Title:            "Choose working folder",
		DefaultDirectory: a.workspaceRoot(),
	})
	if err != nil || dir == "" {
		return "", err // cancelled or error → no change
	}
	return a.SwitchWorkspace(dir)
}

func (a *App) ListWorkspaces() []WorkspaceMeta {
	cur := a.workspaceRoot()
	seen := map[string]bool{}
	paths := make([]string, 0, 8)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if seen[path] {
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	add(cur)
	for _, path := range loadWorkspaces() {
		add(path)
	}
	out := make([]WorkspaceMeta, 0, len(paths))
	for _, path := range paths {
		out = append(out, WorkspaceMeta{
			Path:    path,
			Name:    workspaceName(path),
			Current: path == cur,
		})
	}
	return out
}

func workspaceName(path string) string {
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return path
	}
	return name
}

func (a *App) SwitchWorkspace(dir string) (string, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = home
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		return "", err
	}
	cur := a.workspace()
	// 活动标签的快照(sink 用于新 controller,ctrl 用于运行态守卫 D3)。tabs 在自己
	// 的锁内取,不会与 CreateTab/SetActiveTab/CloseTab/buildTab 竞态(M3)。
	activeTab, _ := a.tabs.View("")
	sink, activeCtrl, targetTab := activeTab.sink, activeTab.ctrl, activeTab.id
	if ws.Root() == cur.Root() {
		saveWorkspace(dir)
		return dir, nil
	}
	// 运行态守卫:活动标签有回合在跑时,重建它的 controller 会把在途工具调用连同
	// 会话状态一起丢掉。工作目录本身已经不再是进程级的(每个标签持有自己的
	// workspace.Context),所以这里只是拦「别在跑的时候换掉脚下的 session」。
	// 前端 disabled={running} 只覆盖 UI 入口,这里补后端防线(与 SetEffort 同款)。
	if activeCtrl != nil && activeCtrl.Running() {
		return "", fmt.Errorf("有任务正在运行,请先停止再切换项目文件夹")
	}
	// 只重建【活动】标签。后台标签各自持有自己的 workspace.Context,继续读写它们
	// 打开的那个项目 —— 这正是过去必须靠 os.Chdir 才做不到、因而只能拒绝多标签
	// 切换的地方。
	//
	// Resolve the new folder's default model from its own config.
	model := ""
	if cfg, cerr := config.LoadIn(ws); cerr == nil {
		model = cfg.DefaultModel
		if e, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			model = e.Name + "/" + e.Model
		}
	}
	ctrl, err := boot.Build(a.ctx, boot.Options{Model: model, RequireKey: false, Sink: sink, Workspace: ws, PreToolUse: a.serialReleaseForToolUse, Factory: a.factory})
	if err != nil {
		// 装配失败:什么都没提交,当前会话原样保留(不再需要回滚进程 cwd)。
		return "", err
	}
	saveWorkspace(dir) // remember it so the next launch reopens here
	// Commit the switch: save and tear down the old session, then swap in the new
	// project's controller with a fresh session file.
	if activeCtrl != nil {
		_ = activeCtrl.Snapshot()
		activeCtrl.Close()
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	// 只写发起切换的那个标签(SwitchWorkspace 只重建它的 controller)。
	a.tabUpdate(targetTab, func(rt *tabRuntime) {
		rt.ws = ws
		rt.ctrl = ctrl
		rt.model = model
		rt.label = ctrl.Label()
		rt.startupErr = ""
	})
	ctrl.EnableInteractiveApproval()
	if d := ctrl.SessionDir(); d != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(d, ctrl.Label()))
	}
	return dir, nil
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
	if gatewayActive() {
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

// CommandInfo describes one available slash command for the composer's "/" menu.
type CommandInfo struct {
	Name        string `json:"name"` // without the leading slash
	Description string `json:"description"`
	Hint        string `json:"hint,omitempty"` // argument hint, if any
	Kind        string `json:"kind"`           // "builtin" | "custom" | "mcp"
}

// Commands lists the slash commands available this session — built-in actions,
// custom commands (.reasonix/commands), and MCP prompts — for the composer's "/"
// autocomplete menu.
func (a *App) Commands() []CommandInfo {
	out := []CommandInfo{
		{Name: "new", Description: i18n.M.CmdNew, Kind: "builtin"},
		{Name: "compact", Description: i18n.M.CmdCompact, Kind: "builtin"},
	}
	if !gatewayActive() {
		out = append(out,
			CommandInfo{Name: "model", Description: i18n.M.CmdModel, Kind: "builtin"},
			CommandInfo{Name: "effort", Description: i18n.M.CmdEffort, Kind: "builtin"},
		)
	}
	out = append(out,
		CommandInfo{Name: "memory", Description: i18n.M.CmdMemory, Kind: "builtin"},
		CommandInfo{Name: "mcp", Description: i18n.M.CmdMcp, Kind: "builtin"},
		CommandInfo{Name: "hooks", Description: i18n.M.CmdHooks, Kind: "builtin"},
		CommandInfo{Name: "theme", Description: i18n.M.CmdTheme, Kind: "builtin"},
		CommandInfo{Name: "skill", Description: i18n.M.CmdSkill, Kind: "builtin"},
	)
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return out
	}
	// Skills are invocable as /<name> (the model runs inline ones; subagent ones
	// run isolated). Listing them here is what surfaces /init, /explore, … in the
	// composer's slash menu; selecting one submits "/<name>", which the controller
	// resolves via RunSkill.
	for _, s := range ctrl.Skills() {
		out = append(out, CommandInfo{Name: s.Name, Description: s.Description, Kind: "skill"})
	}
	for _, c := range ctrl.Commands() {
		out = append(out, CommandInfo{Name: c.Name, Description: c.Description, Hint: c.ArgHint, Kind: "custom"})
	}
	if h := ctrl.Host(); h != nil {
		for _, p := range h.Prompts() {
			out = append(out, CommandInfo{Name: p.Name, Description: p.Description, Kind: "mcp"})
		}
	}
	return out
}

// SlashArgItem is one sub-command / argument suggestion for the composer's slash
// menu (the part after the command word). Mirrors the CLI's arg completion via
// the shared control.SlashArgItems, so desktop and CLI offer the same hints.
type SlashArgItem struct {
	Label   string `json:"label"`
	Insert  string `json:"insert"`
	Hint    string `json:"hint"`
	Descend bool   `json:"descend"`
}

// SlashArgsResult carries the suggestions plus the byte offset in the input where
// the current token begins, so the composer replaces just that token.
type SlashArgsResult struct {
	Items []SlashArgItem `json:"items"`
	From  int            `json:"from"`
}

// SlashArgs completes the arguments of a management slash command (/mcp, /model,
// /skill, /hooks) for the composer — the same logic the chat TUI uses. Empty
// Items means the input has no structured arguments to complete.
func (a *App) SlashArgs(input string) SlashArgsResult {
	if gatewayActive() && isGatewayManagedSlash(strings.TrimSpace(input)) {
		return SlashArgsResult{Items: []SlashArgItem{}}
	}
	v, _ := a.tabs.View("")
	ctrl, model := v.ctrl, v.model
	if ctrl == nil {
		return SlashArgsResult{}
	}
	data := control.ArgData{
		Skills:       ctrl.Skills(),
		CurrentModel: model,
	}
	if !gatewayActive() {
		for _, m := range a.Models() {
			data.ModelRefs = append(data.ModelRefs, m.Ref)
		}
	}
	if h := ctrl.Host(); h != nil {
		data.ServerNames = h.ServerNames()
	}
	items, from := control.SlashArgItems(input, data)
	// Non-nil so it serializes as a JSON array, never null — the frontend filters
	// over it directly.
	out := SlashArgsResult{Items: []SlashArgItem{}, From: from}
	for _, it := range items {
		out.Items = append(out.Items, SlashArgItem{Label: it.Label, Insert: it.Insert, Hint: it.Hint, Descend: it.Descend})
	}
	return out
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

// SkillView is one discoverable skill for the drawer.
type SkillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	RunAs       string `json:"runAs"`
}

// SkillRootView is one skill discovery root for the drawer's Sources section.
type SkillRootView struct {
	Dir        string `json:"dir"`
	Scope      string `json:"scope"`
	Priority   int    `json:"priority"`
	Status     string `json:"status"`
	Configured bool   `json:"configured"`
	Skills     int    `json:"skills"`
	Warning    string `json:"warning,omitempty"`
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

// skillRootsView lists the skill roots for one project. ws is the workspace whose
// project-level skills are counted, so the panel shows the open project's skills
// rather than whatever directory the process happens to stand in.
func skillRootsView(ws workspace.Context) []SkillRootView {
	cfg, _ := config.LoadIn(ws)
	userCfg := config.LoadForEdit(config.UserConfigPath())
	var custom []string
	if cfg != nil {
		custom = cfg.SkillCustomPaths()
	}
	st := skill.New(skill.Options{ProjectRoot: ws.Root(), CustomPaths: custom, DisableBuiltins: true, Stderr: io.Discard})
	counts := map[string]int{}
	for _, sk := range st.List() {
		counts[config.CanonicalSkillPath(filepath.Dir(skillRootPath(sk.Path)))]++
	}
	userConfigured := map[string]bool{}
	if userCfg != nil {
		for _, p := range userCfg.Skills.Paths {
			userConfigured[config.CanonicalSkillPath(p)] = true
		}
	}
	var out []SkillRootView
	for _, r := range st.Roots() {
		dir := config.CanonicalSkillPath(r.Dir)
		view := SkillRootView{
			Dir:        r.Dir,
			Scope:      string(r.Scope),
			Priority:   r.Priority + 1,
			Status:     string(r.Status),
			Configured: r.Scope == skill.ScopeCustom && userConfigured[dir],
			Skills:     counts[dir],
		}
		out = append(out, view)
	}
	if userCfg != nil {
		for _, p := range userCfg.Skills.Paths {
			if rootActive(out, p) {
				continue
			}
			out = append(out, SkillRootView{
				Dir:        p,
				Scope:      string(skill.ScopeCustom),
				Status:     "inactive",
				Configured: true,
				Warning:    "configured in user config but not active in this workspace; project [skills].paths may override it",
			})
		}
	}
	return out
}

func rootActive(roots []SkillRootView, path string) bool {
	want := config.CanonicalSkillPath(path)
	for _, r := range roots {
		if r.Scope == string(skill.ScopeCustom) && config.CanonicalSkillPath(r.Dir) == want {
			return true
		}
	}
	return false
}

// PickSkillFolder opens a directory picker for adding custom skill roots. It only
// returns a path; AddSkillPath performs normalization and writes config.
func (a *App) PickSkillFolder() (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	dir, err := a.sh().OpenDirectoryDialog(DialogOptions{
		Title:            "Choose skills folder",
		DefaultDirectory: a.workspaceRoot(),
	})
	if err != nil || dir == "" {
		return "", err
	}
	return normalizeSkillPath(dir), nil
}

// AddSkillPath adds a custom skill root to the user config and rebuilds the
// controller so the skills index and slash menu reflect it immediately.
func (a *App) AddSkillPath(path string) error {
	path = normalizeSkillPath(path)
	return a.applyConfigChange(func(c *config.Config) error {
		return c.AddSkillPath(path)
	})
}

// RemoveSkillPath removes a custom skill root from the user config and rebuilds.
func (a *App) RemoveSkillPath(path string) error {
	path = normalizeSkillPath(path)
	return a.applyConfigChange(func(c *config.Config) error {
		_, err := c.RemoveSkillPath(path)
		return err
	})
}

// RefreshSkills rebuilds the controller without changing config, reloading skill
// discovery, the system prompt index, and slash completions.
func (a *App) RefreshSkills() error {
	return a.rebuild()
}

func normalizeSkillPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if info.Mode().IsRegular() {
		if filepath.Base(path) == skill.SkillFile {
			return filepath.Clean(filepath.Dir(filepath.Dir(path)))
		}
		return filepath.Clean(filepath.Dir(path))
	}
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(path, skill.SkillFile)); err == nil {
			return filepath.Clean(filepath.Dir(path))
		}
	}
	return filepath.Clean(path)
}

func skillRootPath(path string) string {
	if filepath.Base(path) == skill.SkillFile {
		return filepath.Dir(path)
	}
	return path
}

// --- helpers for the 3 buttons ---

// ModelInfo is one (provider, model) the bottom switcher can pick. Ref ("provider/
// model") is what SetModel takes; Provider/Model are for display.
type ModelInfo struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Current  bool   `json:"current"`
}

type EffortInfo struct {
	Supported bool     `json:"supported"`
	Current   string   `json:"current"`
	Default   string   `json:"default"`
	Levels    []string `json:"levels"`
}

// Models flattens the configured providers into their (provider, model) pairs —
// the switcher's options — marking the active one. A vendor with a `models` list
// yields one entry per model, all sharing the same endpoint/key. Unconfigured
// providers are skipped. Result is non-nil: the frontend reads .length, so a nil
// slice (JSON null) would crash the switcher on an empty list.
func (a *App) Models() []ModelInfo {
	if gatewayActive() {
		return []ModelInfo{}
	}
	active, _ := a.tabs.View("")
	curModel := active.model
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	out := []ModelInfo{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		for _, m := range p.ModelList() {
			ref := p.Name + "/" + m
			out = append(out, ModelInfo{Ref: ref, Provider: p.Name, Model: m, Current: ref == curModel})
		}
	}
	return out
}

// SetModel switches the active model and carries the current conversation into the
// new model's session, so the chat continues seamlessly and subsequent turns use
// the new model. (Switching models necessarily resets the prompt cache; that's the
// cost of the switch.) No-op if name is already active or the controller is down.
func (a *App) SetModel(name string) error {
	if a.ctx == nil || name == "" {
		return nil
	}
	// 网关模式下模型由平台统一分配,拒绝本地切模型(H4 兜底;前端 ModelSwitcher 在网关模式
	// 已不暴露真实模型,这里防直接调 IPC 绕过)。档位切换走 SetOnecreatTier,不经这里。
	if gatewayActive() {
		return errGatewayManaged
	}
	// 固定「发起切换时的活动标签」+ 它自己的 sink:boot.Build 是秒级操作,期间用户可能
	// 切走 activeTab。build 完成后只回写这个标签,并用它的 sink 绑定新 controller,避免
	// 新 controller 装进错误标签、事件串到别的标签通道(A5)。
	// 重建沿用【该标签自己的】workspace:换模型不该把标签的项目根偷偷挪回进程 cwd
	// (那正是 Plan 01 要根除的隐式状态)。
	active, _ := a.tabs.View("")
	curModel, ctrl := active.model, active.ctrl
	targetTab, targetSink, targetWS := active.id, active.sink, active.ws
	if name == curModel {
		return nil
	}
	// 跨重建持住该标签的项目,免得工作区在两个 controller 之间被关掉再重开。
	defer a.holdWorkspace(targetWS).Release()

	var carried []provider.Message
	if ctrl != nil {
		_ = ctrl.Snapshot()
		carried = ctrl.History()
		ctrl.Close()
	}

	newCtrl, err := boot.Build(a.ctx, boot.Options{Model: name, RequireKey: false, Sink: targetSink, Workspace: targetWS, PreToolUse: a.serialReleaseForToolUse, Factory: a.factory})
	if err != nil {
		return err
	}
	// 只写发起切换的那个标签(SetModel 只作用于它)。用户在 build 期间切走也不影响:
	// 结果按 id 落回原标签,不再有「活动镜像」需要一并考虑(A5)。
	adopted := a.tabUpdate(targetTab, func(rt *tabRuntime) {
		rt.ctrl = newCtrl
		rt.model = name
		rt.label = newCtrl.Label()
	})
	if !adopted {
		// 发起标签在秒级 boot.Build 期间被 CloseTab 关掉了:没有任何标签引用新
		// controller,必须 Close 它,否则它的 MCP 子进程 / LSP / goroutine 会泄漏到
		// 进程退出(与 buildTab 的 A6 closed 检查同类)。
		newCtrl.Close()
		return nil
	}
	newCtrl.EnableInteractiveApproval()

	path := ""
	if dir := newCtrl.SessionDir(); dir != "" {
		path = agent.NewSessionPath(dir, newCtrl.Label())
	}
	// Carry the prior conversation (full provider.Message log, incl. the system
	// prompt) into the new session so history is preserved across the switch.
	if len(carried) > 0 {
		newCtrl.Resume(&agent.Session{Messages: carried}, path)
	} else if path != "" {
		newCtrl.SetSessionPath(path)
	}
	return nil
}

// rebuildTabByID 用当前 config + 环境(含网关 env)重建「指定」标签的 controller,沿用该
// 标签自己的 model 并带过它的历史。boot.Build 是秒级操作,绝不持 a.mu 跨它:锁内只快照
// 该 tab 的 ctrl/model/sink,锁外重建,再回锁写回(boot.Build-under-a.mu 红线)。
func (a *App) rebuildTabByID(tabID string) {
	if a.ctx == nil {
		return
	}
	v, ok := a.tabs.View(tabID)
	if !ok {
		return
	}
	ctrl := v.ctrl
	model := v.model
	targetWS := v.ws // 重建沿用该标签自己的项目根,不回落到进程 cwd
	targetSink := v.sink
	if targetSink == nil {
		targetSink = a.tabs.Sink("")
	}
	if model == "" {
		// 该 tab 尚未记录 model 时退回活动标签的。
		active, _ := a.tabs.View("")
		model = active.model
	}
	if model == "" {
		return // 还没 build 过(极早期登录):startup 的 applyGatewayEnvFromSession 已兜底
	}
	// 跨重建持住该标签的项目(同 SetModel)。
	defer a.holdWorkspace(targetWS).Release()

	var carried []provider.Message
	if ctrl != nil {
		_ = ctrl.Snapshot()
		carried = ctrl.History()
		ctrl.Close()
	}
	newCtrl, err := boot.Build(a.ctx, boot.Options{Model: model, RequireKey: false, Sink: targetSink, Workspace: targetWS, PreToolUse: a.serialReleaseForToolUse, Factory: a.factory})
	if err != nil {
		return // 重建失败:旧 ctrl 已 Close,下条消息会报错但 app 不崩(与 SetModel 行为一致)
	}
	adopted := a.tabUpdate(tabID, func(rt *tabRuntime) {
		rt.ctrl = newCtrl
		rt.label = newCtrl.Label()
	})
	if !adopted {
		// 目标标签在秒级 boot.Build 期间被关闭:没有任何标签引用新 controller,必须
		// Close 它,否则 MCP 子进程 / LSP / goroutine 会泄漏到进程退出。
		newCtrl.Close()
		return
	}
	newCtrl.EnableInteractiveApproval()

	path := ""
	if dir := newCtrl.SessionDir(); dir != "" {
		path = agent.NewSessionPath(dir, newCtrl.Label())
	}
	if len(carried) > 0 {
		newCtrl.Resume(&agent.Session{Messages: carried}, path)
	} else if path != "" {
		newCtrl.SetSessionPath(path)
	}
}

// rebuildAllTabs 用当前网关 env 重建「每一个」标签的 controller —— 不止活动 tab。切档 /
// 登录 / 登出后必须全量重建:tier/token/URL 在 boot.Build 时被固化进每个 tab 的 provider
// (boot.go applyOnecreatGateway 把 model 烤成 tier-N、key 烤成网关 token,provider 不按
// 每条请求重读),只重建活动 tab 会让后台 tab 继续按「旧档」计费、甚至登出后仍持已撤销
// 的 token 继续打计费端点(H2)。boot.Build 秒级,锁内只快照 tab id 列表,锁外逐个重建。
// 注:正在跑的后台 tab 会被 Close 重建(其历史已带过)—— 登出时这正是要的(撤销 token 不
// 能再被使用);切档时会中断该 tab 当前这一轮,与既有「活动 tab 切模型即重建」行为一致。
// anyTabRunning 报告是否有任意标签(含后台)正在跑回合。切档前的运行态守卫用(E1):切档
// 会全量重建每个 tab 的 controller,Close 掉运行中 tab 会丢在途流式回合。锁内捕获各 tab 的
// ctrl,锁外调 Running()(与 SetEffort 同款,避免在 a.mu 下调 controller 方法)。
func (a *App) anyTabRunning() bool {
	for _, ctrl := range a.tabs.Controllers() {
		if ctrl.Running() {
			return true
		}
	}
	return false
}

func (a *App) rebuildAllTabs() {
	if a.ctx == nil {
		return
	}
	for _, t := range a.tabs.List() {
		a.rebuildTabByID(t.ID)
	}
}

func resolveHardwareMCP() (command, source string, err error) {
	// 新名优先,回退旧名:老安装的 .app 内 / PATH 上可能仍是 reasonix-hardware-mcp(读旧)。
	bins := []string{"onecreat-hardware-mcp", "reasonix-hardware-mcp"}
	if override := strings.TrimSpace(os.Getenv("REASONIX_HARDWARE_MCP")); override != "" {
		if executable(override) {
			return override, "REASONIX_HARDWARE_MCP", nil
		}
		return "", "REASONIX_HARDWARE_MCP", fmt.Errorf("REASONIX_HARDWARE_MCP points to a missing or non-executable file: %s", override)
	}
	if exe, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exe)
		// Dev 模式优先回溯到 repo 根的 bin/(make build 的产物)。wails dev 的 bundle 名随
		// outputfilename(onecreat-desktop;旧版 reasonix-desktop),production 是 onecreat.app
		// 走下面 exe-based 路径,这段不命中。
		if strings.Contains(exeDir, "onecreat-desktop.app") || strings.Contains(exeDir, "reasonix-desktop.app") {
			for _, bin := range bins {
				// .../desktop/build/bin/<name>.app/Contents/MacOS → 回溯 6 层到 repo 根
				devCandidate := filepath.Join(exeDir, "..", "..", "..", "..", "..", "..", "bin", bin)
				if executable(devCandidate) {
					return filepath.Clean(devCandidate), "dev bin", nil
				}
			}
		}
		for _, bin := range bins {
			for _, candidate := range []string{
				filepath.Join(exeDir, bin),
				filepath.Join(exeDir, bin+".exe"),
				filepath.Join(exeDir, "..", "Resources", bin),
				filepath.Join(exeDir, "..", "Resources", bin+".exe"),
			} {
				if executable(candidate) {
					return filepath.Clean(candidate), "app bundle", nil
				}
			}
		}
	}
	for _, bin := range bins {
		if p, e := exec.LookPath(bin); e == nil {
			return p, "PATH", nil
		}
	}
	if cwd, e := os.Getwd(); e == nil {
		for _, bin := range bins {
			for _, candidate := range []string{
				filepath.Join(cwd, "bin", bin),
				filepath.Join(cwd, "..", "bin", bin),
				filepath.Join(cwd, "..", "..", "bin", bin),
			} {
				if executable(candidate) {
					return filepath.Clean(candidate), "workspace bin", nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("硬件助手未就绪:找不到 reasonix-hardware-mcp 程序。请重启应用;若仍出现,重新安装 OneCreat(开发环境则运行 make build,或设置 REASONIX_HARDWARE_MCP 指向该程序)")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if goruntime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func (a *App) Effort() EffortInfo {
	entry, err := a.currentProviderEntry()
	if err != nil {
		return EffortInfo{Current: "auto", Levels: []string{}}
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		return EffortInfo{Supported: false, Current: "auto", Default: cap.Default, Levels: []string{}}
	}
	return EffortInfo{Supported: true, Current: config.EffortDisplay(entry), Default: cap.Default, Levels: cap.Levels}
}

func (a *App) SetEffort(level string) error {
	ctrl := a.activeCtrl()
	if ctrl != nil && ctrl.Running() {
		return fmt.Errorf("finish or cancel the current turn before changing effort")
	}
	entry, err := a.currentProviderEntry()
	if err != nil {
		return err
	}
	effort, err := config.NormalizeEffort(entry, level)
	if err != nil {
		return err
	}
	return a.applyConfigChange(func(cfg *config.Config) error {
		if _, ok := cfg.Provider(entry.Name); !ok {
			if err := cfg.UpsertProvider(*entry); err != nil {
				return err
			}
		}
		if entry.Kind == "anthropic" && effort != "" && entry.Thinking == "" {
			if err := cfg.SetProviderThinking(entry.Name, "adaptive"); err != nil {
				return err
			}
		}
		return cfg.SetProviderEffort(entry.Name, effort)
	})
}

func (a *App) notice(text string) {
	// 活动标签的 sink 由 tabs 在自己的锁内解析(M3:/effort 命令经 Submit→
	// runEffortCommand 调到这里,与 tab 生命周期方法并发,裸读指针是数据竞态)。
	sink := a.tabs.Sink("")
	if sink != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text})
	}
}

func (a *App) runEffortCommand(input string) {
	entry, err := a.currentProviderEntry()
	if err != nil {
		a.notice("effort: " + err.Error())
		return
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		a.notice(fmt.Sprintf("effort is not configurable for %s", entry.Name))
		return
	}
	args := strings.Fields(input)
	if len(args) < 2 {
		a.notice(fmt.Sprintf("effort for %s: %s (default: %s; options: %s)", entry.Name, config.EffortDisplay(entry), cap.Default, strings.Join(cap.Levels, "|")))
		return
	}
	if len(args) > 2 {
		a.notice("usage: /effort " + strings.Join(cap.Levels, "|"))
		return
	}
	effort, err := config.NormalizeEffort(entry, args[1])
	if err != nil {
		a.notice(err.Error())
		return
	}
	if err := a.SetEffort(args[1]); err != nil {
		a.notice("effort: " + err.Error())
		return
	}
	display := effort
	if display == "" {
		display = "auto"
	}
	a.notice(fmt.Sprintf("effort for %s set to %s", entry.Name, display))
}

func (a *App) currentProviderEntry() (*config.ProviderEntry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	v, _ := a.tabs.View("")
	ref := v.model
	if strings.TrimSpace(ref) == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", ref)
	}
	return entry, nil
}

// --- memory panel (frontend ⇄ controller) ---

// eventSink is the controller's event.Sink in desktop mode: it forwards every
// agent event to the frontend as one shell event, JSON-shaped by toWire. It is a
// type distinct from App so App's bound method set stays the clean command surface
// — Emit must not be exposed to JS. Emit runs on the agent goroutine; Shell.Emit
// is goroutine-safe, and the ctx guard covers the brief window before startup
// assigns it.
type eventSink struct {
	ctx   context.Context
	app   *App
	tabID string // 该 sink 归属的标签;事件发到 agent:event:<tabID>,空则发旧的全局通道
}

func (s *eventSink) Emit(e event.Event) {
	if s.ctx != nil && s.app != nil {
		ch := eventChannel
		if s.tabID != "" {
			ch = eventChannel + ":" + s.tabID
		}
		s.app.sh().Emit(ch, toWire(e))
	}
	// Persist after each turn so a force-kill of a long session loses at most the
	// in-flight prompt, not every turn back to the last workspace switch.
	// 存的是「本 sink 所属标签」的 session,后台标签完成一轮也各存各的。
	if e.Kind == event.TurnDone && s.app != nil {
		s.app.sessions.ScheduleSnapshot(s.tabID)
	}
}
