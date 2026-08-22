package main

// tabRuntimeService 负责标签运行时的**装配与重建**,以及「当前选中的项目文件夹」。
//
// 这两件事必须放在一起:切换项目要重建活动标签的 controller,而每次重建都得知道该
// 标签自己的项目根。它们过去是 App 上最重的一块逻辑,连同 a.mu 一起 —— App 现在只
// 剩转发与 DTO 组装。
//
// 三条红线在这里(见各方法注释,Plan 02 / Plan 05 的回归):
//   - boot.Build 是秒级操作,绝不在任何锁内调用;
//   - 结果一律按【发起时的标签 id】写回,不写「当前活动标签」;
//   - 重建期间用 hold 持住项目引用,免得工作区在两个 controller 之间被关掉再重开。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"reasonix/internal/account"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/provider"
	"reasonix/internal/workspace"
)

type tabRuntimeService struct {
	tabs    *tabManager
	factory *boot.Factory
	// gateway 是平台账号(与 App 共享同一个对象):每次 boot.Build 都传给会话,
	// 续期后的新 token 无需重建即可生效。
	gateway *account.Gateway
	shell   func() Shell
	// ctxFn 取 Wails / Web 壳的根 context。它在 startup 里才装上,所以按函数取而不是
	// 构造时固定。
	ctxFn func() context.Context
	// preTool 是每次工具调用前的观察回调(让出常驻串口),透传给 boot.Build。
	preTool func(context.Context, string, json.RawMessage)

	// mu 只保护 ws:当前选中的项目文件夹。每个标签在 tabRuntime.ws 里持有自己
	// 【实际】的 root,切换项目只改这里和活动标签,后台标签继续读写它们自己的目录。
	mu sync.RWMutex
	ws workspace.Context
}

func newTabRuntimeService(tabs *tabManager, factory *boot.Factory, gw *account.Gateway, shell func() Shell, ctx func() context.Context, preTool func(context.Context, string, json.RawMessage)) *tabRuntimeService {
	return &tabRuntimeService{tabs: tabs, factory: factory, gateway: gw, shell: shell, ctxFn: ctx, preTool: preTool}
}

func (r *tabRuntimeService) sh() Shell {
	if r == nil || r.shell == nil {
		return noopShell{}
	}
	return r.shell()
}

func (r *tabRuntimeService) ctx() context.Context {
	if r == nil || r.ctxFn == nil {
		return nil
	}
	return r.ctxFn()
}

// SetWorkspace 记下当前选中的项目文件夹(启动时解析出来的那个)。
func (r *tabRuntimeService) SetWorkspace(ws workspace.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.ws = ws
	r.mu.Unlock()
}

// workspace returns the currently selected project folder. Callers that resolve
// a path for the UI (file browsing, pickers, the hardware panel) use it instead
// of os.Getwd: the process working directory is fixed at startup and no longer
// tracks which project the user has open.
func (r *tabRuntimeService) Workspace() workspace.Context {
	if r == nil {
		return workspace.Context{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ws
}

// workspaceRoot is workspace().Root() with a process-cwd fallback, for the call
// sites that need a concrete directory (a subprocess argument, a browse base).
// The fallback only fires in tests that construct a bare service.
func (r *tabRuntimeService) Root() string {
	if root := r.Workspace().Root(); root != "" {
		return root
	}
	wd, _ := os.Getwd()
	return wd
}

// buildTab 为一个标签装配它自己的 controller(boot.Build 可能较慢,所以由 CreateTab
// 放到 goroutine 里调)。装配完成后写回该标签的运行时,并发 agent:ready:<tabID>
// 通知前端该标签可用。
//
// 全程按 tabID 路由、不碰任何「活动标签」状态:build 期间用户切走或再切回来都不影响
// 结果落在哪个标签上(A5)。boot.Build 是秒级操作,绝不在 tabs 锁内调用。
func (r *tabRuntimeService) BuildTab(tabID string) {
	if r == nil {
		return
	}
	v, ok := r.tabs.View(tabID)
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

	ctrl, err := boot.Build(r.ctx(), boot.Options{Model: model, RequireKey: false, Sink: sink, Workspace: ws, PreToolUse: r.preTool, Factory: r.factory, Gateway: r.gateway})
	if err != nil {
		// 标签已在 build 期间被关闭时 Update 找不到它:什么都不写、不发 ready(A6)。
		if !r.tabs.Update(tabID, func(rt *tabRuntime, _ bool) {
			rt.startupErr = err.Error()
			rt.ready = true
		}) {
			return
		}
		r.sh().Emit("agent:ready:"+tabID, nil)
		return
	}

	// 「是否被采纳」必须在同一次加锁里定下来:分两次读会与 CloseTab 撞出
	// 「两边都以为该由自己 Close」的双重 Close。CloseTab 从注册表删除标签,所以
	// Update 找得到 ⟺ 标签还活着 ⟺ 这个 controller 归它。
	var wantPlan, wantBypass bool
	var coachWant string
	adopted := r.tabs.Update(tabID, func(rt *tabRuntime, _ bool) {
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
	r.sh().Emit("agent:ready:"+tabID, nil)
}

// holdWorkspace 在「换掉某个标签的 controller」期间持住它的项目引用。
//
// 重建路径(SetModel / rebuildTabByID / 设置变更)都是先 Close 旧 controller、再 Build
// 新的:中间那一小段时间里没有任何会话引用这个工作区,引用计数会 1→0→1 —— 项目的
// 语言服务器和 CodeGraph 守护进程被停掉再立刻重启。持有一份显式引用就把意图写清楚了:
// 「我在换这个项目上的会话,项目本身别关」。裸 &App{}(无 factory)返回 nil,Release
// 对 nil 是空操作。
func (r *tabRuntimeService) hold(ws workspace.Context) *boot.WorkspaceHandle {
	if r == nil || r.factory == nil {
		return nil
	}
	return r.factory.Hold(ws)
}

// tabUpdate 在 tabs 锁内改一个标签的运行时(空 tabID=活动标签),并回报该标签是否
// 存在。门控类方法用它记录 want 状态,再在锁外施加到 controller。
func (r *tabRuntimeService) update(tabID string, fn func(rt *tabRuntime)) bool {
	if r == nil {
		return false
	}
	return r.tabs.Update(tabID, func(rt *tabRuntime, _ bool) { fn(rt) })
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
func (r *tabRuntimeService) PickWorkspace() (string, error) {
	if r.ctx() == nil {
		return "", nil
	}
	dir, err := r.sh().OpenDirectoryDialog(DialogOptions{
		Title:            "Choose working folder",
		DefaultDirectory: r.Root(),
	})
	if err != nil || dir == "" {
		return "", err // cancelled or error → no change
	}
	return r.SwitchWorkspace(dir)
}

func (r *tabRuntimeService) ListWorkspaces() []WorkspaceMeta {
	cur := r.Root()
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

func (r *tabRuntimeService) SwitchWorkspace(dir string) (string, error) {
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
	cur := r.Workspace()
	// 活动标签的快照(sink 用于新 controller,ctrl 用于运行态守卫 D3)。tabs 在自己
	// 的锁内取,不会与 CreateTab/SetActiveTab/CloseTab/buildTab 竞态(M3)。
	activeTab, _ := r.tabs.View("")
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
	ctrl, err := boot.Build(r.ctx(), boot.Options{Model: model, RequireKey: false, Sink: sink, Workspace: ws, PreToolUse: r.preTool, Factory: r.factory, Gateway: r.gateway})
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
	r.mu.Lock()
	r.ws = ws
	r.mu.Unlock()
	// 只写发起切换的那个标签(SwitchWorkspace 只重建它的 controller)。
	r.update(targetTab, func(rt *tabRuntime) {
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

// SetModel switches the active model and carries the current conversation into the
// new model's session, so the chat continues seamlessly and subsequent turns use
// the new model. (Switching models necessarily resets the prompt cache; that's the
// cost of the switch.) No-op if name is already active or the controller is down.
func (r *tabRuntimeService) SetModel(name string) error {
	if r == nil || r.ctx() == nil || name == "" {
		return nil
	}
	// 网关模式下模型由平台统一分配,拒绝本地切模型(H4 兜底;前端 ModelSwitcher 在网关模式
	// 已不暴露真实模型,这里防直接调 IPC 绕过)。档位切换走 SetOnecreatTier,不经这里。
	if platformAccountEnabled() && r.gateway.Active() {
		return errGatewayManaged
	}
	// 固定「发起切换时的活动标签」+ 它自己的 sink:boot.Build 是秒级操作,期间用户可能
	// 切走 activeTab。build 完成后只回写这个标签,并用它的 sink 绑定新 controller,避免
	// 新 controller 装进错误标签、事件串到别的标签通道(A5)。
	// 重建沿用【该标签自己的】workspace:换模型不该把标签的项目根偷偷挪回进程 cwd
	// (那正是 Plan 01 要根除的隐式状态)。
	active, _ := r.tabs.View("")
	curModel, ctrl := active.model, active.ctrl
	targetTab, targetSink, targetWS := active.id, active.sink, active.ws
	if name == curModel {
		return nil
	}
	// 跨重建持住该标签的项目,免得工作区在两个 controller 之间被关掉再重开。
	defer r.hold(targetWS).Release()

	var carried []provider.Message
	if ctrl != nil {
		_ = ctrl.Snapshot()
		carried = ctrl.History()
		ctrl.Close()
	}

	newCtrl, err := boot.Build(r.ctx(), boot.Options{Model: name, RequireKey: false, Sink: targetSink, Workspace: targetWS, PreToolUse: r.preTool, Factory: r.factory, Gateway: r.gateway})
	if err != nil {
		return err
	}
	// 只写发起切换的那个标签(SetModel 只作用于它)。用户在 build 期间切走也不影响:
	// 结果按 id 落回原标签,不再有「活动镜像」需要一并考虑(A5)。
	adopted := r.update(targetTab, func(rt *tabRuntime) {
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
// 标签自己的 model 并带过它的历史。boot.Build 是秒级操作,绝不持 r.mu 跨它:锁内只快照
// 该 tab 的 ctrl/model/sink,锁外重建,再回锁写回(boot.Build-under-r.mu 红线)。
func (r *tabRuntimeService) RebuildTab(tabID string) {
	if r == nil || r.ctx() == nil {
		return
	}
	v, ok := r.tabs.View(tabID)
	if !ok {
		return
	}
	ctrl := v.ctrl
	model := v.model
	targetWS := v.ws // 重建沿用该标签自己的项目根,不回落到进程 cwd
	targetSink := v.sink
	if targetSink == nil {
		targetSink = r.tabs.Sink("")
	}
	if model == "" {
		// 该 tab 尚未记录 model 时退回活动标签的。
		active, _ := r.tabs.View("")
		model = active.model
	}
	if model == "" {
		return // 还没 build 过(极早期登录):startup 的 applyGatewayEnvFromSession 已兜底
	}
	// 跨重建持住该标签的项目(同 SetModel)。
	defer r.hold(targetWS).Release()

	var carried []provider.Message
	if ctrl != nil {
		_ = ctrl.Snapshot()
		carried = ctrl.History()
		ctrl.Close()
	}
	newCtrl, err := boot.Build(r.ctx(), boot.Options{Model: model, RequireKey: false, Sink: targetSink, Workspace: targetWS, PreToolUse: r.preTool, Factory: r.factory, Gateway: r.gateway})
	if err != nil {
		return // 重建失败:旧 ctrl 已 Close,下条消息会报错但 app 不崩(与 SetModel 行为一致)
	}
	adopted := r.update(tabID, func(rt *tabRuntime) {
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
// ctrl,锁外调 Running()(与 SetEffort 同款,避免在 r.mu 下调 controller 方法)。
func (r *tabRuntimeService) AnyTabRunning() bool {
	if r == nil {
		return false
	}
	for _, ctrl := range r.tabs.Controllers() {
		if ctrl.Running() {
			return true
		}
	}
	return false
}

func (r *tabRuntimeService) RebuildAllTabs() {
	if r == nil || r.ctx() == nil {
		return
	}
	for _, t := range r.tabs.List() {
		r.RebuildTab(t.ID)
	}
}
