package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	fileenc "reasonix/internal/fileutil/encoding"
	"reasonix/internal/i18n"
	"reasonix/internal/memory"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
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
// eventSink that forwards each one to the webview via runtime.EventsEmit.
type App struct {
	ctx  context.Context
	sink *eventSink
	ctrl *control.Controller

	// mu protects ctrl, label, model, startupErr, and ready during the async
	// boot sequence. startup() spawns a goroutine for boot.Build(); all methods
	// that touch the controller acquire the lock.
	mu          sync.RWMutex
	startupErr  string
	label       string
	model       string // active provider name (for the bottom model switcher)
	ready       bool   // true once boot.Build completes (success or failure)
	disabledMCP map[string]ServerView
	mcpOrder    []string

	// 多标签多任务(像 Codex / Claude Code):每个 tab 一个独立 controller + sink +
	// session 文件,后台 tab 的 controller 照常在自己的 goroutine 里跑,事件发到
	// 独立通道 agent:event:<tabID> —— 所以多个任务可以「真并行」。
	// ctrl/sink/label/model/ready/startupErr 这几个字段始终镜像「当前活动 tab」,
	// 既有的 52 处 a.ctrl 读取无需改动;SetActiveTab 在切换时重指镜像。
	tabs      map[string]*tabRuntime
	tabOrder  []string // 标签顺序(新建追加到末尾)
	activeTab string
	tabSeq    int // 生成新 tab id 的自增计数

	// Per-turn autosave runs off the event goroutine so disk I/O never delays
	// event delivery; overlapping requests coalesce into one trailing write.
	// 按 tab 单飞:后台 tab 完成一轮也各存各的 session,不串到活动 tab。
	saveMu    sync.Mutex
	saving    map[string]bool
	saveAgain map[string]bool
}

// tabRuntime 是一个独立任务标签的后端运行时:自己的 controller、事件 sink、
// session(session 路径存在 ctrl 里)。kind 仅供前端决定显示对话还是硬件视图,
// 后端不据此分支(硬件视图也只是往同一个 controller 注入提示词)。
type tabRuntime struct {
	id         string
	kind       string // "chat" | "hardware"
	sink       *eventSink
	ctrl       *control.Controller
	label      string
	model      string
	ready      bool
	startupErr string
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
		disabledMCP: map[string]ServerView{},
		tabs:        map[string]*tabRuntime{},
		saving:      map[string]bool{},
		saveAgain:   map[string]bool{},
	}
	// 主标签的 sink(tabID "main");同时作为「活动 tab 镜像」的初始 sink。
	a.sink = &eventSink{app: a, tabID: "main"}
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
	a.sink.ctx = ctx
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

func (a *App) showMainWindow(ctx context.Context) {
	runtime.WindowShow(ctx)
	runtime.WindowUnminimise(ctx)
	runtime.WindowCenter(ctx)
}

// buildController runs the full initialization sequence in a background goroutine:
// workspace resolution, config loading, i18n setup, and boot.Build. On success it
// wires up the controller and flips ready; on failure it stores the error so
// Meta().StartupErr surfaces it.
func (a *App) buildController() {
	// A GUI launch starts in "/" (read-only); move into a real, writable working
	// folder (the remembered one, else home) before anything reads/writes config,
	// .env, memory, or skills relative to cwd.
	ensureWorkspace()

	// Drive the Go-side catalogue (i18n.M) from the configured language so the
	// backend-provided slash UI — command descriptions, sub-command hints,
	// listing notices — comes through localized, matching the frontend.
	if cfg, err := config.Load(); err == nil {
		i18n.DetectLanguage(cfg.Language)
	}

	// 注册初始的「主标签」,沿用 NewApp 建好的 a.sink(tabID "main"),并设为活动。
	a.mu.Lock()
	rt := &tabRuntime{id: "main", kind: "chat", sink: a.sink}
	a.tabs["main"] = rt
	a.tabOrder = append(a.tabOrder, "main")
	a.activeTab = "main"
	a.mu.Unlock()

	a.buildTab(rt)
}

// buildTab 在一个标签运行时里装配一个独立 controller(boot.Build 可能较慢,所以由
// CreateTab 放到 goroutine 里调)。装配完成后:写回该 tab 的运行时;若它正是当前活动
// tab,则同步「活动镜像」字段;最后发 agent:ready:<tabID> 通知前端该标签可用。
func (a *App) buildTab(rt *tabRuntime) {
	// 解析当前文件夹的默认模型为规范的 "provider/model"。
	model := ""
	if cfg, err := config.Load(); err == nil {
		model = cfg.DefaultModel
		if e, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			model = e.Name + "/" + e.Model
		}
	}

	ctrl, err := boot.Build(a.ctx, boot.Options{Model: model, RequireKey: false, Sink: rt.sink})
	if err != nil {
		a.mu.Lock()
		rt.startupErr = err.Error()
		rt.ready = true
		if a.activeTab == rt.id {
			a.startupErr = rt.startupErr
			a.ready = true
		}
		a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "agent:ready:"+rt.id)
		return
	}

	a.mu.Lock()
	rt.ctrl = ctrl
	rt.model = model
	rt.label = ctrl.Label()
	rt.ready = true
	if a.activeTab == rt.id {
		a.ctrl = ctrl
		a.sink = rt.sink
		a.model = model
		a.label = ctrl.Label()
		a.ready = true
		a.startupErr = ""
	}
	a.mu.Unlock()

	// Desktop is interactive: route "ask" gate decisions to the frontend as
	// approval_request events, answered via Approve.
	ctrl.EnableInteractiveApproval()

	// Land auto-save in a fresh session file (same as a fresh chat/serve start).
	if dir := ctrl.SessionDir(); dir != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(dir, ctrl.Label()))
	}

	// Notify the frontend this tab is ready — it re-fetches Meta/Context/History.
	runtime.EventsEmit(a.ctx, "agent:ready:"+rt.id)
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
	a.mu.Lock()
	a.tabSeq++
	id := fmt.Sprintf("tab%d", a.tabSeq)
	rt := &tabRuntime{id: id, kind: kind, sink: &eventSink{ctx: a.ctx, app: a, tabID: id}}
	a.tabs[id] = rt
	a.tabOrder = append(a.tabOrder, id)
	// 新建即设为活动(用户刚开它就是要用),活动镜像先指向未就绪的新 tab。
	a.activeTab = id
	a.ctrl = nil
	a.sink = rt.sink
	a.label = ""
	a.model = ""
	a.ready = false
	a.startupErr = ""
	a.mu.Unlock()

	go a.buildTab(rt)
	return TabMeta{ID: id, Kind: kind, Ready: false, Active: true}, nil
}

// SetActiveTab 把「活动镜像」重指到目标标签:既有的会话类方法(读 a.ctrl)随之作用
// 到该标签。前端在切换标签、以及对某标签发指令前调用它。未知 id 是空操作。
func (a *App) SetActiveTab(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rt := a.tabs[id]
	if rt == nil {
		return
	}
	a.activeTab = id
	a.ctrl = rt.ctrl
	a.sink = rt.sink
	a.label = rt.label
	a.model = rt.model
	a.ready = rt.ready
	a.startupErr = rt.startupErr
}

// CloseTab 关闭一个标签:快照并关掉它的 controller,从注册表移除;若关的是活动标签,
// 自动切到末尾的另一个标签。
func (a *App) CloseTab(id string) {
	a.mu.Lock()
	rt := a.tabs[id]
	if rt == nil {
		a.mu.Unlock()
		return
	}
	delete(a.tabs, id)
	for i, x := range a.tabOrder {
		if x == id {
			a.tabOrder = append(a.tabOrder[:i], a.tabOrder[i+1:]...)
			break
		}
	}
	if a.activeTab == id {
		a.activeTab = ""
		a.ctrl, a.sink, a.label, a.model, a.ready, a.startupErr = nil, nil, "", "", false, ""
		if len(a.tabOrder) > 0 {
			next := a.tabOrder[len(a.tabOrder)-1]
			if nt := a.tabs[next]; nt != nil {
				a.activeTab = next
				a.ctrl, a.sink, a.label, a.model, a.ready, a.startupErr = nt.ctrl, nt.sink, nt.label, nt.model, nt.ready, nt.startupErr
			}
		}
	}
	a.mu.Unlock()
	if rt.ctrl != nil {
		_ = rt.ctrl.Snapshot()
		rt.ctrl.Close()
	}
}

// ListTabs 返回所有标签快照(按打开顺序),供前端渲染标签栏。
func (a *App) ListTabs() []TabMeta {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]TabMeta, 0, len(a.tabOrder))
	for _, id := range a.tabOrder {
		rt := a.tabs[id]
		if rt == nil {
			continue
		}
		out = append(out, TabMeta{ID: id, Kind: rt.kind, Label: rt.label, Ready: rt.ready, StartupErr: rt.startupErr, Active: id == a.activeTab})
	}
	return out
}

// shutdown snapshots the conversation and stops plugin subprocesses on close.
func (a *App) shutdown(context.Context) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil {
		_ = ctrl.Snapshot()
		ctrl.Close()
	}
}

// --- bound command surface (frontend → controller) ---
// Each method guards on a nil controller so a pre-startup or failed-build call is
// a no-op, never a panic.

// Submit runs raw user input as a turn; slash commands and @-references are
// resolved by the controller. Output arrives asynchronously on eventChannel.
func (a *App) Submit(input string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "/effort" || strings.HasPrefix(trimmed, "/effort ") {
		a.runEffortCommand(trimmed)
		return
	}
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil {
		ctrl.Submit(input)
	}
}

// SubmitDisplay runs input as a turn while recording a shorter UI-only display
// string for the saved desktop transcript. The model still receives input.
func (a *App) SubmitDisplay(display, input string) {
	if a.ctrl == nil {
		return
	}
	dir := config.SessionDir()
	sessionPath := a.ctrl.SessionPath()
	_ = recordSessionDisplay(dir, sessionPath, input, display)
	// 记录该 session 创建时所在的 workspace,用于侧栏按文件夹分组。
	// 只在首次有效消息时落,后续 workspace 切换不影响归属。
	if cwd, err := os.Getwd(); err == nil {
		_ = rememberSessionCwd(dir, sessionPath, cwd)
	}
	a.ctrl.Submit(input)
}

// Cancel aborts the in-flight turn.
func (a *App) Cancel() {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil {
		ctrl.Cancel()
	}
}

// Approve answers a pending approval_request by ID: allow runs the call, session
// also remembers the grant for the rest of the session.
func (a *App) Approve(id string, allow, session bool) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil {
		ctrl.Approve(id, allow, session)
	}
}

// SetPlanMode toggles read-only plan mode.
func (a *App) SetPlanMode(on bool) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil {
		ctrl.SetPlanMode(on)
	}
}

// SetCoachMode 设置当前会话的「协作模式」persona(空串=默认无 persona)。
// preamble 是前端选定的口径文案,作用于活动标签,随每个 turn 注入(见 Compose)。
func (a *App) SetCoachMode(preamble string) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
// user's selections per question.
func (a *App) AnswerQuestion(id string, answers []QuestionAnswer) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return
	}
	out := make([]event.AskAnswer, len(answers))
	for i, an := range answers {
		out[i] = event.AskAnswer{QuestionID: an.QuestionID, Selected: an.Selected}
	}
	ctrl.AnswerQuestion(id, out)
}

// Compact runs one compaction pass on demand.
// Compact runs a plain compaction pass (the "compact now" button). Focus-guided
// compaction goes through Submit("/compact <focus>") instead.
func (a *App) Compact() error {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	return ctrl.Compact(a.ctx, "")
}

// NewSession snapshots the current conversation and rotates to a fresh one.
func (a *App) NewSession() error {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeFrom(a.ctx, turn)
}

func (a *App) SummarizeUpTo(turn int) error {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeUpTo(a.ctx, turn)
}

// SessionMeta summarises one saved session for the history panel.
type SessionMeta struct {
	Path           string `json:"path"`
	Preview        string `json:"preview"`         // first user message
	Title          string `json:"title,omitempty"` // user-chosen name, when set (overrides preview)
	Turns          int    `json:"turns"`
	CreatedAt      int64  `json:"createdAt"`      // unix milliseconds
	LastActivityAt int64  `json:"lastActivityAt"` // unix milliseconds
	ModTime        int64  `json:"modTime"`        // compatibility alias for lastActivityAt
	Current        bool   `json:"current"`
	Cwd            string `json:"cwd,omitempty"` // workspace path at session creation, for sidebar grouping
}

type WorkspaceMeta struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// ListSessions returns the saved sessions newest-first for the history panel,
// marking the one the current conversation is writing to and attaching any
// user-chosen titles.
func (a *App) ListSessions() []SessionMeta {
	dir := config.SessionDir()
	infos, err := agent.ListSessions(dir)
	if err != nil {
		return []SessionMeta{}
	}
	titles := loadSessionTitles(dir)
	cwds := loadSessionCwds(dir)
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	cur := ""
	if ctrl != nil {
		cur = ctrl.SessionPath()
	}
	out := make([]SessionMeta, 0, len(infos))
	for _, s := range infos {
		out = append(out, SessionMeta{
			Path:           s.Path,
			Preview:        s.Preview,
			Title:          titles[filepath.Base(s.Path)],
			Turns:          s.Turns,
			CreatedAt:      s.CreatedAt.UnixMilli(),
			LastActivityAt: s.LastActivityAt.UnixMilli(),
			ModTime:        s.LastActivityAt.UnixMilli(),
			Current:        s.Path == cur,
			Cwd:            cwds[filepath.Base(s.Path)],
		})
	}
	return out
}

// DeleteSession removes a saved session (and its title). It refuses the active
// session — that's the conversation on screen, and auto-save would recreate the
// file on the next turn; start a new session first to retire it.
func (a *App) DeleteSession(path string) error {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl != nil && ctrl.SessionPath() == path {
		return errActiveSession
	}
	return deleteSessionFile(config.SessionDir(), path)
}

// RenameSession sets a custom display name for a session (empty clears it back to
// the preview). It only affects the history panel; the file on disk is unchanged.
func (a *App) RenameSession(path, title string) error {
	return setSessionTitle(config.SessionDir(), path, title)
}

// ResumeSession snapshots the current conversation, then loads the session at
// path and continues it — auto-save keeps appending to that file. The model and
// working folder are unchanged (same controller); only the transcript is swapped.
// Returns the resumed messages for the frontend to render.
func (a *App) ResumeSession(path string) ([]HistoryMessage, error) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return []HistoryMessage{}, nil
	}
	loaded, err := agent.LoadSession(path)
	if err != nil {
		return nil, err
	}
	_ = ctrl.Snapshot() // persist the current session before switching away
	ctrl.Resume(loaded, path)
	return a.History(), nil
}

// PreviewSession reads a saved session for display only. It does not snapshot or
// swap the active controller, so the history drawer can call it while a turn runs.
func (a *App) PreviewSession(path string) ([]HistoryMessage, error) {
	return previewSessionMessages(config.SessionDir(), path)
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
	cur, _ := os.Getwd()
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Choose working folder",
		DefaultDirectory: cur,
	})
	if err != nil || dir == "" {
		return "", err // cancelled or error → no change
	}
	return a.SwitchWorkspace(dir)
}

func (a *App) ListWorkspaces() []WorkspaceMeta {
	cur, _ := os.Getwd()
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
	cur, _ := os.Getwd()
	if dir == cur {
		saveWorkspace(dir)
		return dir, nil
	}
	// v1 限制防护:进程 cwd 是全局的,切换工作区只重建「活动」标签的 controller,
	// 后台标签会继续按旧目录的相对路径读写(静默错位,极难排查)。开着多个任务
	// 标签时直接拒绝并说明,让用户先收尾其他标签——诚实报错优于数据错位。
	a.mu.RLock()
	tabCount := len(a.tabs)
	a.mu.RUnlock()
	if tabCount > 1 {
		return "", fmt.Errorf("当前开着 %d 个任务标签;切换项目文件夹前请先关闭其他任务标签(工作目录是全局的,后台任务会读写到错误的目录)", tabCount)
	}
	if err := os.Chdir(dir); err != nil {
		return "", err
	}
	// Resolve the new folder's default model from its own config.
	model := ""
	if cfg, cerr := config.Load(); cerr == nil {
		model = cfg.DefaultModel
		if e, ok := cfg.ResolveModel(cfg.DefaultModel); ok {
			model = e.Name + "/" + e.Model
		}
	}
	ctrl, err := boot.Build(a.ctx, boot.Options{Model: model, RequireKey: false, Sink: a.sink})
	if err != nil {
		_ = os.Chdir(cur) // roll back; the current session stays intact
		return "", err
	}
	saveWorkspace(dir) // remember it so the next launch reopens here
	// Commit the switch: save and tear down the old session, then swap in the new
	// project's controller with a fresh session file.
	a.mu.Lock()
	if a.ctrl != nil {
		_ = a.ctrl.Snapshot()
		a.ctrl.Close()
	}
	a.ctrl = ctrl
	a.model = model
	a.label = ctrl.Label()
	a.startupErr = ""
	// 同步活动 tab 的运行时(SwitchWorkspace 只重建当前活动 tab 的 controller)。
	if rt := a.tabs[a.activeTab]; rt != nil {
		rt.ctrl = ctrl
		rt.model = model
		rt.label = ctrl.Label()
		rt.startupErr = ""
	}
	a.mu.Unlock()
	ctrl.EnableInteractiveApproval()
	if d := ctrl.SessionDir(); d != "" {
		ctrl.SetSessionPath(agent.NewSessionPath(d, ctrl.Label()))
	}
	return dir, nil
}

// HistoryMessage is one prior turn, for the frontend to repopulate its transcript
// after a reload.
type HistoryMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning,omitempty"`
}

// History returns the session's message log.
func (a *App) History() []HistoryMessage {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	msgs := ctrl.History()
	return historyMessages(msgs, sessionDisplayResolver(config.SessionDir(), ctrl.SessionPath()))
}

func historyMessages(msgs []provider.Message, resolveUserContent func(string) string) []HistoryMessage {
	out := make([]HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if m.Role == provider.RoleUser {
			content = resolveUserContent(m.Content)
		}
		reasoning := ""
		if m.Role == provider.RoleAssistant {
			reasoning = m.ReasoningContent
		}
		out = append(out, HistoryMessage{Role: string(m.Role), Content: content, Reasoning: reasoning})
	}
	return out
}

func previewSessionMessages(sessionDir, path string) ([]HistoryMessage, error) {
	loaded, err := agent.LoadSession(path)
	if err != nil {
		return nil, err
	}
	return historyMessages(loaded.Snapshot(), sessionDisplayResolver(sessionDir, path)), nil
}

// ContextInfo is the prompt-vs-window gauge payload. Both zero means no data yet.
type ContextInfo struct {
	Used   int `json:"used"`
	Window int `json:"window"`
}

// ContextUsage returns the latest context-window gauge numbers.
func (a *App) ContextUsage() ContextInfo {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	Bypass       bool   `json:"bypass"` // YOLO mode on (auto-approve every tool call)
}

// Meta reports the model label, readiness, any startup error, the working
// directory (for the status line), and the runtime event channel the frontend
// subscribes to.
func (a *App) Meta() Meta {
	a.mu.RLock()
	label := a.label
	startupErr := a.startupErr
	ready := a.ready
	ctrl := a.ctrl
	a.mu.RUnlock()
	cwd, _ := os.Getwd()
	return Meta{
		Label:        label,
		Ready:        ready,
		StartupErr:   startupErr,
		EventChannel: eventChannel,
		Cwd:          cwd,
		Bypass:       ctrl != nil && ctrl.Bypass(),
	}
}

// SetBypass toggles YOLO mode for the session: auto-approve every tool call
// (writers and bash run without asking). Deny rules still apply. Runtime-only —
// not written to config, so it resets on relaunch.
func (a *App) SetBypass(on bool) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
		{Name: "model", Description: i18n.M.CmdModel, Kind: "builtin"},
		{Name: "effort", Description: i18n.M.CmdEffort, Kind: "builtin"},
		{Name: "memory", Description: i18n.M.CmdMemory, Kind: "builtin"},
		{Name: "mcp", Description: i18n.M.CmdMcp, Kind: "builtin"},
		{Name: "hooks", Description: i18n.M.CmdHooks, Kind: "builtin"},
		{Name: "theme", Description: i18n.M.CmdTheme, Kind: "builtin"},
		{Name: "skill", Description: i18n.M.CmdSkill, Kind: "builtin"},
	}
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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
	a.mu.RLock()
	ctrl := a.ctrl
	model := a.model
	a.mu.RUnlock()
	if ctrl == nil {
		return SlashArgsResult{}
	}
	data := control.ArgData{
		Skills:       ctrl.Skills(),
		CurrentModel: model,
	}
	for _, m := range a.Models() {
		data.ModelRefs = append(data.ModelRefs, m.Ref)
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
func (a *App) Capabilities() CapabilitiesView {
	out := CapabilitiesView{Servers: []ServerView{}, Skills: []SkillView{}, SkillRoots: []SkillRootView{}}
	a.mu.RLock()
	ctrl := a.ctrl
	disabled := make(map[string]ServerView, len(a.disabledMCP))
	for name, s := range a.disabledMCP {
		disabled[name] = s
	}
	order := append([]string(nil), a.mcpOrder...)
	a.mu.RUnlock()
	if ctrl == nil {
		return out
	}
	seen := map[string]bool{}
	connected := map[string]bool{}
	retainedDisabled := map[string]ServerView{}
	codegraphConfigured := false
	if h := ctrl.Host(); h != nil {
		for _, s := range h.Servers() {
			seen[s.Name] = true
			connected[s.Name] = true
			out.Servers = append(out.Servers, ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				ToolList: pluginToolsToView(s.ToolList),
			})
		}
		for _, f := range h.Failures() {
			seen[f.Name] = true
			out.Servers = append(out.Servers, ServerView{
				Name: f.Name, Transport: f.Transport, Status: "failed", Error: f.Error,
			})
		}
	}
	// Configured servers that are neither connected nor failed are toggled off
	// (disconnected this session, or auto_start=false) — shown with an off switch.
	if cfg, err := config.Load(); err == nil {
		codegraphConfigured = cfg.Codegraph.Enabled
		for _, p := range cfg.Plugins {
			if seen[p.Name] {
				continue
			}
			tt := p.Type
			if tt == "" {
				tt = "stdio"
			}
			if s, ok := disabled[p.Name]; ok {
				s.Status = "disabled"
				s.Transport = tt
				s.Error = ""
				out.Servers = append(out.Servers, s)
				retainedDisabled[p.Name] = s
				seen[p.Name] = true
				delete(disabled, p.Name)
				continue
			}
			out.Servers = append(out.Servers, ServerView{Name: p.Name, Transport: tt, Status: "disabled"})
			seen[p.Name] = true
		}
	}
	for name, s := range disabled {
		if seen[name] {
			continue
		}
		if name != "codegraph" || !codegraphConfigured {
			continue
		}
		s.Status = "disabled"
		s.Error = ""
		out.Servers = append(out.Servers, s)
		retainedDisabled[name] = s
	}
	out.Servers = orderServerViews(out.Servers, order)

	a.mu.Lock()
	for name := range connected {
		delete(retainedDisabled, name)
	}
	a.disabledMCP = retainedDisabled
	a.mcpOrder = mergeServerOrder(a.mcpOrder, out.Servers)
	a.mu.Unlock()

	for _, s := range ctrl.Skills() {
		out.Skills = append(out.Skills, SkillView{
			Name: s.Name, Description: s.Description,
			Scope: string(s.Scope), RunAs: string(s.RunAs),
		})
	}
	out.SkillRoots = skillRootsView()
	return out
}

func skillRootsView() []SkillRootView {
	cwd, _ := os.Getwd()
	cfg, _ := config.Load()
	userCfg := config.LoadForEdit(config.UserConfigPath())
	var custom []string
	if cfg != nil {
		custom = cfg.SkillCustomPaths()
	}
	st := skill.New(skill.Options{ProjectRoot: cwd, CustomPaths: custom, DisableBuiltins: true, Stderr: io.Discard})
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
	cur, _ := os.Getwd()
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Choose skills folder",
		DefaultDirectory: cur,
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

// MCPServerInput is the drawer's "add server" form. Transport is "stdio" (Command
// + Args + Env) or "http"/"sse" (URL). Mirrors config.PluginEntry's writable shape.
type MCPServerInput struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	URL       string            `json:"url"`
	Env       map[string]string `json:"env"`
}

// HardwareMCPView describes the local hardware MCP binary the desktop can use.
type HardwareMCPView struct {
	Name       string `json:"name"`
	Available  bool   `json:"available"`
	Command    string `json:"command"`
	Source     string `json:"source"`
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Error      string `json:"error,omitempty"`
}

// HardwareDetectView is a desktop-friendly projection of the hardware_detect MCP
// tool output. The drawer can show real local readiness before the user asks the
// agent to build or flash anything.
type HardwareDetectView struct {
	Available       bool                    `json:"available"`
	Workspace       string                  `json:"workspace,omitempty"`
	ProjectDir      string                  `json:"projectDir,omitempty"`
	ProjectTypes    []string                `json:"projectTypes"`
	SerialPorts     []string                `json:"serialPorts"`
	Boards          []HardwareBoardView     `json:"boards"`
	Devices         []HardwareDeviceView    `json:"devices"`
	Toolchains      []HardwareToolchainView `json:"toolchains"`
	Recommendations []string                `json:"recommendations"`
	ESPIDFOfficial  map[string]string       `json:"espIdfOfficialMcp,omitempty"`
	Error           string                  `json:"error,omitempty"`
}

// HardwareEvidenceStatusView is a compact projection of hardware_evidence_status
// for the drawer. It makes the local/real-hardware verification boundary visible
// without asking the model to summarize it first.
type HardwareEvidenceStatusView struct {
	Available          bool     `json:"available"`
	ProjectDir         string   `json:"projectDir,omitempty"`
	Platform           string   `json:"platform,omitempty"`
	Board              string   `json:"board,omitempty"`
	EvidenceFile       string   `json:"evidenceFile,omitempty"`
	RecordCount        int      `json:"recordCount"`
	CurrentRecordCount int      `json:"currentRecordCount"`
	StaleRecordCount   int      `json:"staleRecordCount"`
	Status             string   `json:"status"`
	Summary            string   `json:"summary"`
	MissingGroups      []string `json:"missingGroups"`
	Recommendations    []string `json:"recommendations"`
	Error              string   `json:"error,omitempty"`
}

type HardwareToolchainView struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type HardwareBoardView struct {
	Port       string `json:"port"`
	Protocol   string `json:"protocol,omitempty"`
	BoardName  string `json:"boardName,omitempty"`
	FQBN       string `json:"fqbn,omitempty"`
	Core       string `json:"core,omitempty"`
	Properties string `json:"properties,omitempty"`
}

type HardwareDeviceView struct {
	Port        string `json:"port"`
	Description string `json:"description,omitempty"`
	HWID        string `json:"hwid,omitempty"`
}

// AddMCPServer connects a server live and persists it to config (Customize → MCP →
// Add). Returns the number of tools it exposed.
func (a *App) AddMCPServer(in MCPServerInput) (int, error) {
	if a.ctrl == nil {
		return 0, fmt.Errorf("no active session")
	}
	return a.ctrl.AddMCPServer(config.PluginEntry{
		Name:    in.Name,
		Type:    in.Transport,
		Command: in.Command,
		Args:    in.Args,
		URL:     in.URL,
		Env:     in.Env,
	})
}

// HardwareMCP reports whether the bundled or locally built hardware MCP server is
// available and whether the current session already has it connected.
func (a *App) HardwareMCP() HardwareMCPView {
	cmd, source, err := resolveHardwareMCP()
	view := HardwareMCPView{Name: "hardware", Command: cmd, Source: source, Available: err == nil}
	if err != nil {
		view.Error = err.Error()
	}
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return view
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == "hardware" {
			view.Configured = true
			view.Connected = true
			return view
		}
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == "hardware" {
			view.Configured = true
			view.Error = f.Error
			return view
		}
	}
	return view
}

// HardwareDetect runs the bundled MCP's detection tool directly so the hardware
// drawer can display local toolchain and serial-port status even before the
// server is connected to the current agent session.
func (a *App) HardwareDetect() HardwareDetectView {
	view := HardwareDetectView{
		Available:       false,
		ProjectTypes:    []string{},
		SerialPorts:     []string{},
		Boards:          []HardwareBoardView{},
		Devices:         []HardwareDeviceView{},
		Toolchains:      []HardwareToolchainView{},
		Recommendations: []string{},
	}
	command, _, err := resolveHardwareMCP()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	cwd, _ := os.Getwd()
	text, err := callHardwareMCPTool(command, "hardware_detect", map[string]any{"project_dir": cwd}, 20*time.Second)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		view.Error = "hardware_detect returned invalid JSON: " + err.Error()
		return view
	}
	normalizeHardwareDetectView(&view)
	view.Available = true
	return view
}

// HardwareEvidenceStatus runs hardware_evidence_status directly so the drawer can
// show whether the current project has only local validation or real-device proof.
func (a *App) HardwareEvidenceStatus() HardwareEvidenceStatusView {
	view := HardwareEvidenceStatusView{
		MissingGroups:   []string{},
		Recommendations: []string{},
	}
	command, _, err := resolveHardwareMCP()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	cwd, _ := os.Getwd()
	text, err := callHardwareMCPTool(command, "hardware_evidence_status", map[string]any{"project_dir": cwd}, 20*time.Second)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		view.Error = "hardware_evidence_status returned invalid JSON: " + err.Error()
		return view
	}
	normalizeHardwareEvidenceStatusView(&view)
	view.Available = true
	return view
}

// evidenceRecordMirror 反序列化 tests/hardware_evidence.jsonl 的每一行。
// 真源是 hardware MCP 的 hardware_evidence_record；这里只读出来汇总成可读文本。
type evidenceRecordMirror struct {
	TimestampUTC  string `json:"timestampUtc"`
	Platform      string `json:"platform"`
	Board         string `json:"board"`
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	Summary       string `json:"summary"`
	Command       string `json:"command"`
	Port          string `json:"port"`
	OutputExcerpt string `json:"outputExcerpt"`
}

// HardwareEvidenceExport 把 tests/hardware_evidence.jsonl 里的真机验证记录汇总成
// 一段学生可直接粘进研究日志/论文的 Markdown——目的是让竞赛材料用「真实采集的
// 编译/烧录/串口/部署证据」，而不是凭记忆编数字（这条是项目红线）。
// 返回空字符串表示还没有任何验证记录。
func (a *App) HardwareEvidenceExport(projectDir string) (string, error) {
	dir := resolveHardwareProjectDir(projectDir)
	path := filepath.Join(dir, "tests", "hardware_evidence.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // 还没有证据文件，不算错误
		}
		return "", err
	}
	records := make([]evidenceRecordMirror, 0, 16)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec evidenceRecordMirror
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Stage != "" {
			records = append(records, rec)
		}
	}
	if len(records) == 0 {
		return "", nil
	}
	return renderEvidenceMarkdown(records), nil
}

// evidenceStageLabel 把英文阶段名翻成学生能懂的中文。
func evidenceStageLabel(stage string) string {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "compile", "build", "validate":
		return "编译/语法"
	case "upload", "flash":
		return "烧录"
	case "monitor", "serial":
		return "串口/运行日志"
	case "ssh", "ssh_deploy", "deploy":
		return "真机部署"
	case "mpremote":
		return "MicroPython 部署"
	default:
		return stage
	}
}

func renderEvidenceMarkdown(records []evidenceRecordMirror) string {
	var b strings.Builder
	b.WriteString("# 真机验证证据（onecreat 自动导出）\n\n")
	b.WriteString("> 本文件由 onecreat 从真实的编译 / 烧录 / 串口 / 部署记录（tests/hardware_evidence.jsonl）自动汇总，")
	b.WriteString("可作为研究日志、论文的原始验证依据。请勿手工编造数据。\n\n")
	fmt.Fprintf(&b, "共 %d 条验证记录。\n", len(records))
	for i, rec := range records {
		fmt.Fprintf(&b, "\n## %d. 【%s】%s\n", i+1, evidenceStageLabel(rec.Stage), strings.TrimSpace(rec.Status))
		if t := strings.TrimSpace(rec.TimestampUTC); t != "" {
			fmt.Fprintf(&b, "- 时间（UTC）：%s\n", t)
		}
		plat := strings.TrimSpace(rec.Platform)
		if board := strings.TrimSpace(rec.Board); board != "" {
			plat = strings.TrimSpace(plat + " / " + board)
		}
		if plat != "" {
			fmt.Fprintf(&b, "- 平台 / 板卡：%s\n", plat)
		}
		if p := strings.TrimSpace(rec.Port); p != "" {
			fmt.Fprintf(&b, "- 端口：%s\n", p)
		}
		if s := strings.TrimSpace(rec.Summary); s != "" {
			fmt.Fprintf(&b, "- 结果：%s\n", s)
		}
		if c := strings.TrimSpace(rec.Command); c != "" {
			fmt.Fprintf(&b, "- 命令：`%s`\n", c)
		}
		if o := strings.TrimSpace(rec.OutputExcerpt); o != "" {
			// 串口输出本身可能含 ```（罕见但会发生），用动态围栏避免提前闭合代码块、
			// 破坏导出文档，同时原样保留输出内容。
			fence := codeFence(o)
			fmt.Fprintf(&b, "- 输出片段：\n\n%s\n%s\n%s\n", fence, o, fence)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeFence 返回一段比 content 里最长连续反引号还长一位的反引号围栏（至少 3 个），
// 保证 Markdown 代码块不会被内容里的 ``` 提前闭合。
func codeFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// HardwareBoardFactsView 是「写代码前」要硬注入 prompt 的板卡事实串。
// Found=false 表示当前板卡没有可注入的确定事实（自定义板/检测到的裸板），
// 前端据此跳过注入、回退到原有「让模型自己调工具」的流程。
type HardwareBoardFactsView struct {
	Found bool   `json:"found"`
	Facts string `json:"facts"`
}

// boardProfileMirror / moduleSpecMirror 只反序列化我们要渲染的字段子集。
// 真源仍是 hardware MCP 的 catalog（platform-api.json / sensor-catalog.json /
// builtInBoardProfiles）——这里不复制数据，只解析它的 JSON 输出。
type boardProfileMirror struct {
	Profile *struct {
		Label                string   `json:"label"`
		ArduinoFQBN          string   `json:"arduinoFqbn"`
		LogicVoltage         string   `json:"logicVoltage"`
		PowerNotes           string   `json:"powerNotes"`
		RecommendedProtocols []string `json:"recommendedProtocols"`
		DefaultPins          []struct {
			Name  string   `json:"name"`
			Pins  []string `json:"pins"`
			Notes string   `json:"notes"`
		} `json:"defaultPins"`
		RiskyPins []struct {
			Name  string   `json:"name"`
			Pins  []string `json:"pins"`
			Notes string   `json:"notes"`
		} `json:"riskyPins"`
		CommonFailures []string `json:"commonFailures"`
		TeachingNotes  []string `json:"teachingNotes"`
	} `json:"profile"`
}

type moduleSpecMirror struct {
	Modules []struct {
		Matched  bool     `json:"matched"`
		Kind     string   `json:"kind"`
		Function string   `json:"function"`
		Imports  []string `json:"imports"`
		Gotchas  []string `json:"gotchas"`
		Snippet  string   `json:"snippet"`
	} `json:"modules"`
}

// HardwareBoardFacts 在「写代码前」确定性地取出已选板卡的校验事实，拼成一段文本
// 供 HardwarePanel 直接注入 prompt——不再依赖弱模型自觉去调 board_profile /
// module_spec（项目实测：flash 裸写国产生态必幻觉，只有把事实压进上下文才对）。
// 事实来自两个已有 MCP 工具（单一真源）：
//   - hardware_board_profile：电平、默认/风险引脚、推荐协议、常见失败、教学提示
//   - hardware_module_spec（板卡名当 module 查）：冷门平台 API 的正确 import、
//     gotchas、最小示例（ESP32 LEDC / 行空板 pinpong / MaixCAM K230 maix.*）——
//     这正是 flash 最容易编错库名和 API 的地方。
// boardFactsCache 缓存按板卡查到的事实。catalog 内嵌在 MCP 二进制里、运行期不变,
// 同一板卡反复点「写代码」没必要反复拉起 MCP 子进程(每次两个调用、各 15s 超时,
// MCP 卡顿时按钮会冻很久)。只缓存「两个调用都成功」的结果——MCP 暂时性失败
// 不能被记成永久没有事实。
var (
	boardFactsMu    sync.Mutex
	boardFactsCache = map[string]HardwareBoardFactsView{}
)

func (a *App) HardwareBoardFacts(board, platform string) HardwareBoardFactsView {
	board = strings.TrimSpace(board)
	// 自定义板和「检测到的裸板」没有 catalog 事实，直接跳过
	if board == "" || strings.EqualFold(board, "custom") || strings.HasPrefix(board, "detected:") {
		return HardwareBoardFactsView{}
	}
	if strings.TrimSpace(platform) == "" {
		platform = "auto"
	}
	cacheKey := strings.ToLower(board) + "|" + strings.ToLower(platform)
	boardFactsMu.Lock()
	if v, ok := boardFactsCache[cacheKey]; ok {
		boardFactsMu.Unlock()
		return v
	}
	boardFactsMu.Unlock()

	command, _, err := resolveHardwareMCP()
	if err != nil {
		return HardwareBoardFactsView{}
	}

	// 两个 MCP 调用互不依赖,并行跑:最坏等待从 30s 降到 15s。
	var (
		profileSection, apiSection string
		profileErr, apiErr         error
		wg                         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		// 板卡 profile：电平 / 引脚 / 协议 / 常见失败
		text, e := callHardwareMCPTool(command, "hardware_board_profile",
			map[string]any{"board": board, "platform": platform}, 15*time.Second)
		profileErr = e
		if e != nil {
			return
		}
		var bp boardProfileMirror
		if json.Unmarshal([]byte(text), &bp) == nil && bp.Profile != nil {
			profileSection = renderBoardProfileFacts(bp)
		}
	}()
	go func() {
		defer wg.Done()
		// 平台 API：把板卡名当 module 查，命中 platform_api 就拿到正确 import/坑/示例
		text, e := callHardwareMCPTool(command, "hardware_module_spec",
			map[string]any{"modules": []string{board}, "board": board, "platform": platform}, 15*time.Second)
		apiErr = e
		if e != nil {
			return
		}
		var ms moduleSpecMirror
		if json.Unmarshal([]byte(text), &ms) == nil {
			apiSection = renderPlatformAPIFacts(ms)
		}
	}()
	wg.Wait()

	sections := make([]string, 0, 2)
	if profileSection != "" {
		sections = append(sections, profileSection)
	}
	if apiSection != "" {
		sections = append(sections, apiSection)
	}
	view := HardwareBoardFactsView{}
	if len(sections) > 0 {
		view = HardwareBoardFactsView{Found: true, Facts: strings.Join(sections, "\n")}
	}
	// 两个调用都成功才缓存(含「确实没有该板事实」的合法空结果)
	if profileErr == nil && apiErr == nil {
		boardFactsMu.Lock()
		boardFactsCache[cacheKey] = view
		boardFactsMu.Unlock()
	}
	return view
}

// renderBoardProfileFacts 把板卡 profile 渲染成紧凑、学生可读的中文事实块。
func renderBoardProfileFacts(bp boardProfileMirror) string {
	p := bp.Profile
	var b strings.Builder
	title := p.Label
	if title == "" {
		title = "目标板卡"
	}
	if p.ArduinoFQBN != "" {
		fmt.Fprintf(&b, "板卡：%s（FQBN %s）\n", title, p.ArduinoFQBN)
	} else {
		fmt.Fprintf(&b, "板卡：%s\n", title)
	}
	if p.LogicVoltage != "" {
		line := "逻辑电平：" + p.LogicVoltage
		if p.PowerNotes != "" {
			line += "。" + p.PowerNotes
		}
		b.WriteString(line + "\n")
	}
	if len(p.RecommendedProtocols) > 0 {
		fmt.Fprintf(&b, "推荐通信：%s\n", strings.Join(p.RecommendedProtocols, "、"))
	}
	for _, pin := range p.DefaultPins {
		if len(pin.Pins) == 0 {
			continue
		}
		seg := "默认引脚 " + pin.Name + "：" + strings.Join(pin.Pins, "/")
		if pin.Notes != "" {
			seg += "（" + pin.Notes + "）"
		}
		b.WriteString(seg + "\n")
	}
	for _, pin := range p.RiskyPins {
		seg := "⚠️ 风险引脚 " + pin.Name
		if len(pin.Pins) > 0 {
			seg += " " + strings.Join(pin.Pins, "/")
		}
		if pin.Notes != "" {
			seg += "：" + pin.Notes
		}
		b.WriteString(seg + "\n")
	}
	if len(p.CommonFailures) > 0 {
		fmt.Fprintf(&b, "常见失败：%s\n", strings.Join(p.CommonFailures, "；"))
	}
	if len(p.TeachingNotes) > 0 {
		fmt.Fprintf(&b, "教学提示：%s\n", strings.Join(p.TeachingNotes, "；"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPlatformAPIFacts 从 module_spec 结果里挑出 platform_api（冷门平台软件写法），
// 渲染成「正确 import + 坑 + 最小示例」——这是防 flash 幻觉最关键的一段。
func renderPlatformAPIFacts(ms moduleSpecMirror) string {
	for _, m := range ms.Modules {
		if !m.Matched || m.Kind != "platform_api" {
			continue
		}
		var b strings.Builder
		name := m.Function
		if name == "" {
			name = "平台 API"
		}
		fmt.Fprintf(&b, "平台 API（%s）——务必照此写，别凭记忆猜库名/方法：\n", name)
		if len(m.Imports) > 0 {
			fmt.Fprintf(&b, "正确 import：%s\n", strings.Join(m.Imports, "；"))
		}
		if len(m.Gotchas) > 0 {
			fmt.Fprintf(&b, "注意：%s\n", strings.Join(m.Gotchas, "；"))
		}
		if strings.TrimSpace(m.Snippet) != "" {
			fmt.Fprintf(&b, "最小示例：\n%s\n", strings.TrimSpace(m.Snippet))
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return ""
}

// HardwareRunInput is the shared input for the one-click compile/upload/monitor
// buttons in HardwarePanel. Fields are optional unless required by the underlying
// MCP tool — the dispatch picks the right tool by Platform.
type HardwareRunInput struct {
	ProjectDir string `json:"projectDir"`
	Platform   string `json:"platform"`
	Board      string `json:"board,omitempty"`
	Port       string `json:"port,omitempty"`
	Seconds    int    `json:"seconds,omitempty"`
}

// HardwareRunResult is the normalized result the frontend renders into the
// one-click action UI. RootCause + FixHint come from hardware_project_validate's
// error-distillation; they are empty on success or when an underlying tool doesn't
// distill (upload/monitor). Output is the truncated command output for the drawer.
type HardwareRunResult struct {
	Status    string `json:"status"`
	Kind      string `json:"kind,omitempty"` // 验证子类(如 python_syntax)，前端据此区分「真编译」与「仅语法检查」
	Summary   string `json:"summary"`
	Output    string `json:"output,omitempty"`
	RootCause string `json:"rootCause,omitempty"`
	FixHint   string `json:"fixHint,omitempty"`
	NextStep  string `json:"nextStep,omitempty"`
	Error     string `json:"error,omitempty"`
	Command   string `json:"command,omitempty"`
}

// HardwareValidate runs hardware_project_validate. Returns the first failed
// validationResult, otherwise the last one. The frontend's "编译" button calls it.
func (a *App) HardwareValidate(input HardwareRunInput) HardwareRunResult {
	command, err := a.requireHardwareMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	args := map[string]any{
		"project_dir":     resolveHardwareProjectDir(input.ProjectDir),
		"timeout_seconds": 180,
	}
	if input.Platform != "" {
		args["platform"] = input.Platform
	}
	if input.Board != "" {
		args["board"] = input.Board
	}
	text, err := callHardwareMCPTool(command, "hardware_project_validate", args, 200*time.Second)
	if err != nil {
		return HardwareRunResult{Status: "failed", Summary: "编译/验证调用失败", Error: err.Error()}
	}
	var report struct {
		Summary         string             `json:"summary"`
		Results         []validationResult `json:"results"`
		Recommendations []string           `json:"recommendations"`
	}
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		return HardwareRunResult{Status: "failed", Summary: "无法解析验证结果", Error: err.Error()}
	}
	pick := pickValidationResult(report.Results)
	res := HardwareRunResult{
		Status:    coalesceStatus(pick.Status, "skipped"),
		Kind:      pick.Kind,
		Summary:   strings.TrimSpace(report.Summary),
		Output:    truncateOutput(pick.Output, 4096),
		RootCause: pick.RootCause,
		FixHint:   pick.FixHint,
		NextStep:  pick.NextStep,
		Error:     pick.Error,
		Command:   pick.Command,
	}
	// py_compile 只是语法检查，不是真编译(不解析 import/API)。别让「N passed」
	// 的绿勾被学生当成「已验证」——把摘要降级成诚实表述。
	if res.Kind == "python_syntax" && res.Status == "passed" {
		res.Summary = "仅语法检查通过（py_compile），API / 真机未验证"
	}
	if res.Summary == "" {
		res.Summary = res.Status
	}
	return res
}

// HardwareUpload dispatches to the platform-appropriate upload MCP tool.
// Arduino/PlatformIO/ESP-IDF/MicroPython are wired here; SSH-deployed Python
// platforms (Unihiker / MaixCAM / RPi) fall back to "use chat" since they need
// host + remote_path the student hasn't entered yet.
func (a *App) HardwareUpload(input HardwareRunInput) HardwareRunResult {
	command, err := a.requireHardwareMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	projectDir := resolveHardwareProjectDir(input.ProjectDir)
	switch input.Platform {
	case "arduino":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "在硬件面板选择上传端口后再点烧录。"}
		}
		args := map[string]any{
			"sketch_dir":      projectDir,
			"port":            input.Port,
			"timeout_seconds": 120,
		}
		if input.Board != "" {
			args["fqbn"] = arduinoFQBNFromBoard(input.Board)
		}
		return runHardwareSimple(command, "arduino_upload", args, 150*time.Second, "Arduino 烧录")
	case "platformio":
		args := map[string]any{
			"project_dir":     projectDir,
			"targets":         []string{"upload"},
			"timeout_seconds": 180,
		}
		if input.Port != "" {
			args["upload_port"] = input.Port
		}
		return runHardwareSimple(command, "platformio_run", args, 220*time.Second, "PlatformIO 烧录")
	case "esp_idf":
		args := map[string]any{
			"project_dir":     projectDir,
			"action":          "flash",
			"timeout_seconds": 180,
		}
		if input.Port != "" {
			args["port"] = input.Port
		}
		return runHardwareSimple(command, "esp_idf_run", args, 220*time.Second, "ESP-IDF 烧录")
	case "micropython":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "选择 MicroPython 设备端口后再点烧录。"}
		}
		args := map[string]any{
			"port":            input.Port,
			"project_dir":     projectDir,
			"timeout_seconds": 60,
		}
		return runHardwareSimple(command, "mpremote_run", args, 80*time.Second, "MicroPython 部署")
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return HardwareRunResult{
			Status:   "skipped",
			Summary:  "该平台需要 SSH 部署,先在对话里完成",
			NextStep: "Unihiker / MaixCAM / 树莓派项目用 SSH 烧录:在对话框输入 ssh 主机和路径,让 AI 调用 ssh_deploy_run。",
		}
	default:
		return HardwareRunResult{Status: "failed", Summary: "未知平台", Error: "unsupported platform: " + input.Platform}
	}
}

// HardwareMonitor dispatches to the platform-appropriate serial-monitor MCP tool
// for a short sampling window. The frontend's "看串口" button calls it.
func (a *App) HardwareMonitor(input HardwareRunInput) HardwareRunResult {
	command, err := a.requireHardwareMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	seconds := input.Seconds
	if seconds <= 0 {
		seconds = 8
	}
	if seconds > 30 {
		seconds = 30 // 别让前端按钮一按等半分钟
	}
	switch input.Platform {
	case "arduino":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "选择串口后再点查看串口。"}
		}
		args := map[string]any{
			"port":            input.Port,
			"seconds":         seconds,
			"baud":            115200,
			"timeout_seconds": seconds + 5,
		}
		return runHardwareSimple(command, "arduino_monitor_sample", args, time.Duration(seconds+10)*time.Second, "串口采样")
	case "platformio":
		args := map[string]any{
			"project_dir":     resolveHardwareProjectDir(input.ProjectDir),
			"targets":         []string{"monitor"},
			"timeout_seconds": seconds + 5,
		}
		if input.Port != "" {
			args["monitor_port"] = input.Port
		}
		return runHardwareSimple(command, "platformio_run", args, time.Duration(seconds+10)*time.Second, "PlatformIO 串口")
	case "esp_idf":
		args := map[string]any{
			"project_dir":     resolveHardwareProjectDir(input.ProjectDir),
			"action":          "monitor",
			"timeout_seconds": seconds + 5,
		}
		if input.Port != "" {
			args["port"] = input.Port
		}
		return runHardwareSimple(command, "esp_idf_run", args, time.Duration(seconds+10)*time.Second, "ESP-IDF 串口")
	default:
		return HardwareRunResult{
			Status:   "skipped",
			Summary:  "该平台暂无一键串口",
			NextStep: "Python/MicroPython/Unihiker/MaixCAM 项目请在对话框里让 AI 执行 SSH 或 mpremote 调试。",
		}
	}
}

// --- helpers for the 3 buttons ---

func (a *App) requireHardwareMCP() (string, error) {
	command, _, err := resolveHardwareMCP()
	if err != nil {
		return "", err
	}
	return command, nil
}

func resolveHardwareProjectDir(dir string) string {
	if d := strings.TrimSpace(dir); d != "" {
		return d
	}
	cwd, _ := os.Getwd()
	return cwd
}

// validationResult mirrors cmd/reasonix-hardware-mcp's structure for unmarshalling.
type validationResult struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Status    string `json:"status"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	RootCause string `json:"rootCause,omitempty"`
	FixHint   string `json:"fixHint,omitempty"`
	NextStep  string `json:"nextStep,omitempty"`
}

// pickValidationResult picks the first failure (so the UI surfaces a problem),
// or the last result when everything passed/skipped (so we still show something).
func pickValidationResult(results []validationResult) validationResult {
	if len(results) == 0 {
		return validationResult{Status: "skipped"}
	}
	for _, r := range results {
		if r.Status == "failed" {
			return r
		}
	}
	return results[len(results)-1]
}

func coalesceStatus(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncateOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n…(已截断)"
}

// runHardwareSimple wraps "call MCP tool + return result" for upload/monitor where
// the underlying tool doesn't return structured validation results — we treat the
// raw text as the output and let the model help if it fails.
func runHardwareSimple(command, tool string, args map[string]any, timeout time.Duration, label string) HardwareRunResult {
	text, err := callHardwareMCPTool(command, tool, args, timeout)
	if err != nil {
		return HardwareRunResult{
			Status:  "failed",
			Summary: label + "失败",
			Error:   err.Error(),
			Output:  truncateOutput(text, 4096),
		}
	}
	return HardwareRunResult{
		Status:  "passed",
		Summary: label + "完成",
		Output:  truncateOutput(text, 4096),
	}
}

// arduinoFQBNFromBoard maps short board ids the frontend uses to arduino-cli FQBNs.
// 覆盖范围必须与 hardware MCP 的 arduinoFQBN 保持一致：前端预设板传的是
// 「arduino_uno」「arduino_nano」这种带前缀的 value，validate 走 MCP 的 arduinoFQBN、
// upload 走这里——少一个别名就会出现「编译能过但烧录拿到非法 FQBN」的撕裂。
func arduinoFQBNFromBoard(board string) string {
	switch strings.ToLower(strings.TrimSpace(board)) {
	case "uno", "arduino_uno":
		return "arduino:avr:uno"
	case "nano", "arduino_nano", "nanoatmega328":
		return "arduino:avr:nano"
	case "mega", "mega2560", "arduino_mega":
		return "arduino:avr:mega"
	case "esp32", "esp32dev", "esp32_devkit", "esp32_arduino":
		return "esp32:esp32:esp32"
	case "esp32s3", "esp32_s3":
		return "esp32:esp32:esp32s3"
	case "esp32c3", "esp32_c3":
		return "esp32:esp32:esp32c3"
	}
	return board
}

// AddHardwareMCPServer connects the first available hardware MCP binary and
// persists it to config. The frontend should use this rather than hardcoding a
// developer-machine path.
func (a *App) AddHardwareMCPServer() (int, error) {
	command, _, err := resolveHardwareMCP()
	if err != nil {
		return 0, err
	}
	return a.AddMCPServer(MCPServerInput{
		Name:      "hardware",
		Transport: "stdio",
		Command:   command,
		Args:      []string{},
		Env:       map[string]string{},
	})
}

// RemoveMCPServer disconnects a live server and drops it from config (the row's ✕).
func (a *App) RemoveMCPServer(name string) error {
	if a.ctrl == nil {
		return fmt.Errorf("no active session")
	}
	_, err := a.ctrl.RemoveMCPServer(name)
	if err == nil {
		a.mu.Lock()
		delete(a.disabledMCP, name)
		a.mcpOrder = removeServerOrder(a.mcpOrder, name)
		a.mu.Unlock()
	}
	return err
}

// RetryMCPServer reconnects a configured server that failed or was disconnected,
// without touching config (the failed row's retry button).
func (a *App) RetryMCPServer(name string) error {
	if a.ctrl == nil {
		return fmt.Errorf("no active session")
	}
	_, err := a.ctrl.ConnectConfiguredMCPServer(name)
	return err
}

// SetMCPServerEnabled is the connector toggle: on reconnects a configured server
// for this session, off disconnects it (config untouched either way — like Claude
// Code's per-conversation enable/disable, it resets on the next session start).
func (a *App) SetMCPServerEnabled(name string, enabled bool) error {
	if a.ctrl == nil {
		return fmt.Errorf("no active session")
	}
	if enabled {
		_, err := a.ctrl.ConnectConfiguredMCPServer(name)
		if err == nil {
			a.mu.Lock()
			delete(a.disabledMCP, name)
			a.mu.Unlock()
		}
		return err
	}
	if s, ok := findMCPServerView(a.ctrl, name); ok {
		s.Status = "disabled"
		s.Error = ""
		a.mu.Lock()
		if a.disabledMCP == nil {
			a.disabledMCP = map[string]ServerView{}
		}
		a.disabledMCP[name] = s
		a.mcpOrder = mergeServerOrder(a.mcpOrder, []ServerView{s})
		a.mu.Unlock()
	}
	a.ctrl.DisconnectMCPServer(name)
	return nil
}

type hardwareMCPRPCResponse struct {
	Error  *hardwareMCPRPCError `json:"error,omitempty"`
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

type hardwareMCPRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func callHardwareMCPTool(command, name string, args map[string]any, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	if err := json.NewEncoder(stdin).Encode(req); err != nil {
		_ = cmd.Process.Kill()
		return "", err
	}
	_ = stdin.Close()
	err = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("hardware MCP timed out after %s", timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("hardware MCP failed: %s", msg)
	}
	line := strings.TrimSpace(firstOutputLine(stdout.String()))
	if line == "" {
		return "", fmt.Errorf("hardware MCP returned no output")
	}
	var resp hardwareMCPRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return "", fmt.Errorf("hardware MCP returned invalid JSON-RPC: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("hardware MCP RPC error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result.IsError {
		return "", fmt.Errorf("%s", firstTextContent(resp.Result.Content))
	}
	text := firstTextContent(resp.Result.Content)
	if text == "" {
		return "", fmt.Errorf("hardware MCP returned no text content")
	}
	return text, nil
}

func firstOutputLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func firstTextContent(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	for _, item := range content {
		if item.Type == "text" {
			return item.Text
		}
	}
	return ""
}

func normalizeHardwareDetectView(view *HardwareDetectView) {
	if view.ProjectTypes == nil {
		view.ProjectTypes = []string{}
	}
	if view.SerialPorts == nil {
		view.SerialPorts = []string{}
	}
	if view.Boards == nil {
		view.Boards = []HardwareBoardView{}
	}
	if view.Devices == nil {
		view.Devices = []HardwareDeviceView{}
	}
	if view.Toolchains == nil {
		view.Toolchains = []HardwareToolchainView{}
	}
	if view.Recommendations == nil {
		view.Recommendations = []string{}
	}
}

func normalizeHardwareEvidenceStatusView(view *HardwareEvidenceStatusView) {
	if view.MissingGroups == nil {
		view.MissingGroups = []string{}
	}
	if view.Recommendations == nil {
		view.Recommendations = []string{}
	}
}

func findMCPServerView(ctrl *control.Controller, name string) (ServerView, bool) {
	if ctrl == nil || ctrl.Host() == nil {
		return ServerView{}, false
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == name {
			return ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				ToolList: pluginToolsToView(s.ToolList),
			}, true
		}
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == name {
			return ServerView{Name: f.Name, Transport: f.Transport, Status: "failed", Error: f.Error}, true
		}
	}
	return ServerView{}, false
}

func pluginToolsToView(tools []plugin.ToolInfo) []ToolView {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{Name: t.Name, Description: t.Description})
	}
	return out
}

func orderServerViews(servers []ServerView, order []string) []ServerView {
	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	sort.SliceStable(servers, func(i, j int) bool {
		pi, iok := pos[servers[i].Name]
		pj, jok := pos[servers[j].Name]
		switch {
		case iok && jok:
			return pi < pj
		case iok:
			return true
		case jok:
			return false
		default:
			return false
		}
	})
	return servers
}

func mergeServerOrder(order []string, servers []ServerView) []string {
	seen := make(map[string]bool, len(order)+len(servers))
	next := make([]string, 0, len(order)+len(servers))
	for _, name := range order {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		next = append(next, name)
	}
	for _, s := range servers {
		if s.Name == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		next = append(next, s.Name)
	}
	return next
}

func removeServerOrder(order []string, name string) []string {
	if name == "" || len(order) == 0 {
		return order
	}
	next := order[:0]
	for _, n := range order {
		if n != name {
			next = append(next, n)
		}
	}
	return next
}

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
	a.mu.RLock()
	curModel := a.model
	a.mu.RUnlock()
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
	a.mu.RLock()
	curModel := a.model
	ctrl := a.ctrl
	a.mu.RUnlock()
	if name == curModel {
		return nil
	}

	var carried []provider.Message
	if ctrl != nil {
		_ = ctrl.Snapshot()
		carried = ctrl.History()
		ctrl.Close()
	}

	newCtrl, err := boot.Build(a.ctx, boot.Options{Model: name, RequireKey: false, Sink: a.sink})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ctrl = newCtrl
	a.model = name
	a.label = newCtrl.Label()
	// 同步活动 tab 的运行时(SetModel 只作用于当前活动 tab)。
	if rt := a.tabs[a.activeTab]; rt != nil {
		rt.ctrl = newCtrl
		rt.model = name
		rt.label = newCtrl.Label()
	}
	a.mu.Unlock()
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

func resolveHardwareMCP() (command, source string, err error) {
	const bin = "reasonix-hardware-mcp"
	if override := strings.TrimSpace(os.Getenv("REASONIX_HARDWARE_MCP")); override != "" {
		if executable(override) {
			return override, "REASONIX_HARDWARE_MCP", nil
		}
		return "", "REASONIX_HARDWARE_MCP", fmt.Errorf("REASONIX_HARDWARE_MCP points to a missing or non-executable file: %s", override)
	}
	if exe, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exe)
		// Dev 模式优先回溯到 repo 根的 bin/(make build 的产物)。
		// wails dev 的 bundle 是 reasonix-desktop.app(production 是 onecreat.app),
		// dev bundle 在重建/cp 后路径不稳定,而 repo/bin/ 是稳定的开发期路径。
		// production 走原 exe-based 路径,这段不会命中。
		if strings.Contains(exeDir, "reasonix-desktop.app") {
			// .../desktop/build/bin/reasonix-desktop.app/Contents/MacOS → 回溯 6 层到 repo 根
			devCandidate := filepath.Join(exeDir, "..", "..", "..", "..", "..", "..", "bin", bin)
			if executable(devCandidate) {
				return filepath.Clean(devCandidate), "dev bin", nil
			}
		}
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
	if p, e := exec.LookPath(bin); e == nil {
		return p, "PATH", nil
	}
	if cwd, e := os.Getwd(); e == nil {
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
	return "", "", fmt.Errorf("hardware MCP binary not found; run `make build` or set REASONIX_HARDWARE_MCP")
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
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
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

// DirEntry is one entry in the "@" file-reference menu.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// FilePreview is a bounded, read-only file payload for the workspace side panel.
type FilePreview struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Err       string `json:"err,omitempty"`
}

// atSkip are entries the "@" menu hides as noise.
var atSkip = map[string]bool{".git": true, "node_modules": true, ".DS_Store": true}

const filePreviewLimit = 256 * 1024

func trimUTF8PartialSuffix(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	for i := len(data) - 1; i >= 0 && len(data)-i <= utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.Valid(data[:i]) || utf8.FullRune(data[i:]) {
			return data
		}
		return data[:i]
	}
	return data
}

func workspacePath(rel string) (string, bool, error) {
	base, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	if rel == "" {
		return "", false, os.ErrInvalid
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, rel)
	}
	path = filepath.Clean(path)
	r, err := filepath.Rel(base, path)
	if err != nil {
		return "", false, err
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", false, os.ErrPermission
	}
	return path, true, nil
}

// ListDir lists one directory level (directories first, then files, each
// alphabetical) for the "@" file-reference menu. rel resolves against the process
// cwd; "" lists the cwd. The menu navigates one level at a time, never
// recursively — bounded for huge trees.
func (a *App) ListDir(rel string) []DirEntry {
	base, err := os.Getwd()
	if err != nil {
		return nil
	}
	dir := base
	if rel != "" {
		if filepath.IsAbs(rel) {
			dir = filepath.Clean(rel)
		} else {
			dir = filepath.Join(base, rel)
		}
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var dirs, files []DirEntry
	for _, e := range es {
		name := e.Name()
		if atSkip[name] {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, DirEntry{Name: name, IsDir: true})
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, DirEntry{Name: name, IsDir: false})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return append(dirs, files...)
}

// ReadFile returns a small text preview for a file under the current workspace.
func (a *App) ReadFile(rel string) FilePreview {
	out := FilePreview{Path: rel}
	path, ok, err := workspacePath(rel)
	if err != nil || !ok {
		out.Err = "invalid path"
		return out
	}
	info, err := os.Stat(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if info.IsDir() {
		out.Err = "path is a directory"
		return out
	}
	if !info.Mode().IsRegular() {
		out.Err = "path is not a regular file"
		return out
	}
	out.Size = info.Size()
	f, err := os.Open(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	defer f.Close()

	buf := make([]byte, filePreviewLimit+1)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		out.Err = err.Error()
		return out
	}
	data := buf[:n]
	if len(data) > filePreviewLimit {
		data = data[:filePreviewLimit]
		out.Truncated = true
	}

	// Check for BOM first (just the first 2-3 bytes — always complete
	// even at a truncation boundary). BOM-prefixed files skip the NUL
	// check since UTF-16 normally contains 0x00 for ASCII characters.
	bomKind := fileenc.DetectQuick(data)
	if bomKind != fileenc.UTF8 {
		enc, _ := fileenc.Detect(data)
		if enc == fileenc.LossyUTF8 {
			out.Binary = true
			return out
		}
		decoded := fileenc.Decode(data, enc)
		out.Body = string(decoded)
		return out
	}

	// No BOM — NUL in raw bytes is a binary signal.
	if bytes.Contains(data, []byte{0}) {
		out.Binary = true
		return out
	}

	// Trim any partial multi-byte rune at the truncation boundary BEFORE
	// encoding detection. Without this, a large UTF-8 file truncated
	// mid-character would fail utf8.Valid and be misdetected as GB18030
	// or LossyUTF8, producing mojibake or a false binary classification.
	if out.Truncated {
		data = trimUTF8PartialSuffix(data)
	}
	enc, _ := fileenc.Detect(data)
	if enc == fileenc.LossyUTF8 {
		out.Binary = true
		return out
	}
	out.Body = string(fileenc.Decode(data, enc))
	return out
}

// OpenWorkspacePath opens a file or folder from the workspace in the OS default app.
func (a *App) OpenWorkspacePath(rel string) error {
	path, ok, err := workspacePath(rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	return openWorkspacePath(path)
}

// OpenFolder 在系统文件管理器里打开一个绝对路径的文件夹(供侧栏「在文件夹中打开」用)。
// 与 OpenWorkspacePath 不同:不限制在当前 workspace 内,可打开任意已存在的目录。
func (a *App) OpenFolder(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return os.ErrInvalid
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return os.ErrInvalid
	}
	return openWorkspacePath(path)
}

// RevealWorkspacePath shows a workspace file in the native file manager.
func (a *App) RevealWorkspacePath(rel string) error {
	path, ok, err := workspacePath(rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	default:
		dir := path
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			dir = filepath.Dir(path)
		}
		return exec.Command("xdg-open", dir).Start()
	}
}

func (a *App) notice(text string) {
	if a.sink != nil {
		a.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text})
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
	a.mu.RLock()
	ref := a.model
	a.mu.RUnlock()
	if strings.TrimSpace(ref) == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", ref)
	}
	return entry, nil
}

// SavePastedImage stores a browser clipboard image data URL under
// .reasonix/attachments and returns the relative @-reference path.
func (a *App) SavePastedImage(dataURL string) (string, error) {
	return control.SaveImageDataURL(dataURL)
}

// SavePastedFile stores a dropped non-image file (the browser exposes its bytes
// as a data URL but not a real path) under .reasonix/attachments and returns the
// relative @-reference path.
func (a *App) SavePastedFile(name, dataURL string) (string, error) {
	return control.SaveAttachmentDataURL(name, dataURL)
}

// AttachmentDataURL returns a safe data URL for a stored image attachment.
func (a *App) AttachmentDataURL(path string) (string, error) {
	return control.ImageDataURL(path)
}

// --- memory panel (frontend ⇄ controller) ---

// MemoryDoc is one loaded doc-memory file for the panel: path, scope, and body.
type MemoryDoc struct {
	Path  string `json:"path"`
	Scope string `json:"scope"`
	Body  string `json:"body"`
}

// MemoryFact is one saved auto-memory, surfaced read-only in the panel.
type MemoryFact struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Body        string `json:"body"`
}

// MemoryScope is one writable quick-add target (scope id + the file it writes to).
type MemoryScope struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

// MemoryView is the whole memory panel payload: hierarchical docs, saved facts,
// and the writable scopes for the quick-add selector.
type MemoryView struct {
	Docs      []MemoryDoc   `json:"docs"`
	Facts     []MemoryFact  `json:"facts"`
	Scopes    []MemoryScope `json:"scopes"`
	StoreDir  string        `json:"storeDir"`
	Available bool          `json:"available"`
}

// writableScopes are the quick-add targets the panel offers, broad → specific.
var writableScopes = []memory.Scope{memory.ScopeUser, memory.ScopeProject, memory.ScopeLocal}

// Memory returns the loaded memory for the panel: the REASONIX.md hierarchy, the
// saved auto-memories, and the writable scopes. Read-only; mutations go through
// Remember / SaveDoc.
func (a *App) Memory() MemoryView {
	// Always return non-nil slices: a nil Go slice marshals to JSON `null`, which
	// would crash the panel's `view.facts.length` / `.map`.
	view := MemoryView{Docs: []MemoryDoc{}, Facts: []MemoryFact{}, Scopes: []MemoryScope{}}
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return view
	}
	set := ctrl.Memory()
	if set == nil {
		return view
	}
	view.StoreDir = set.Store.Dir
	view.Available = true
	for _, d := range set.Docs {
		view.Docs = append(view.Docs, MemoryDoc{Path: d.Path, Scope: string(d.Scope), Body: d.Body})
	}
	for _, f := range set.Store.List() {
		view.Facts = append(view.Facts, MemoryFact{
			Name: f.Name, Title: f.Title, Description: f.Description, Type: string(f.Type), Body: f.Body,
		})
	}
	for _, sc := range writableScopes {
		if p := set.DocPath(sc); p != "" { // user scope yields "" when no config dir
			view.Scopes = append(view.Scopes, MemoryScope{Scope: string(sc), Path: p})
		}
	}
	return view
}

// Remember quick-adds a one-line note to the doc-memory file for scope — the
// panel's explicit "remember" action, equivalent to typing "#<note>". An unknown
// scope falls back to project. Returns the file written.
func (a *App) Remember(scope, note string) (string, error) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return "", nil
	}
	return ctrl.QuickAdd(parseScope(scope), note)
}

// Forget deletes a saved auto-memory by name — the panel's delete action for a
// fact the model owns. A no-op when no controller is attached.
func (a *App) Forget(name string) error {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	return ctrl.ForgetMemory(name)
}

// SaveDoc overwrites a memory doc with the panel editor's contents. The controller
// validates path against the recognized memory files. Returns the file written.
func (a *App) SaveDoc(path, body string) (string, error) {
	a.mu.RLock()
	ctrl := a.ctrl
	a.mu.RUnlock()
	if ctrl == nil {
		return "", nil
	}
	return ctrl.SaveDoc(path, body)
}

// parseScope maps a frontend scope id to a memory.Scope, defaulting to project.
func parseScope(s string) memory.Scope {
	switch memory.Scope(s) {
	case memory.ScopeUser:
		return memory.ScopeUser
	case memory.ScopeLocal:
		return memory.ScopeLocal
	default:
		return memory.ScopeProject
	}
}

// eventSink is the controller's event.Sink in desktop mode: it forwards every
// agent event to the webview as one runtime event, JSON-shaped by toWire. It is a
// type distinct from App so App's bound method set stays the clean command surface
// — Emit must not be exposed to JS. Emit runs on the agent goroutine;
// runtime.EventsEmit is goroutine-safe, and the ctx guard covers the brief window
// before startup assigns it.
type eventSink struct {
	ctx   context.Context
	app   *App
	tabID string // 该 sink 归属的标签;事件发到 agent:event:<tabID>,空则发旧的全局通道
}

func (s *eventSink) Emit(e event.Event) {
	if s.ctx != nil {
		ch := eventChannel
		if s.tabID != "" {
			ch = eventChannel + ":" + s.tabID
		}
		runtime.EventsEmit(s.ctx, ch, toWire(e))
	}
	// Persist after each turn so a force-kill of a long session loses at most the
	// in-flight prompt, not every turn back to the last workspace switch.
	// 存的是「本 sink 所属标签」的 session,后台标签完成一轮也各存各的。
	if e.Kind == event.TurnDone && s.app != nil {
		s.app.scheduleSnapshot(s.tabID)
	}
}

// scheduleSnapshot kicks a single-flight background save of one tab's session;
// a request arriving while one runs sets a trailing pass so the final state lands.
func (a *App) scheduleSnapshot(tabID string) {
	a.saveMu.Lock()
	if a.saving[tabID] {
		a.saveAgain[tabID] = true
		a.saveMu.Unlock()
		return
	}
	a.saving[tabID] = true
	a.saveMu.Unlock()
	go a.snapshotLoop(tabID)
}

func (a *App) snapshotLoop(tabID string) {
	for {
		a.mu.RLock()
		var ctrl *control.Controller
		if rt := a.tabs[tabID]; rt != nil {
			ctrl = rt.ctrl
		}
		a.mu.RUnlock()
		if ctrl != nil {
			if err := ctrl.Snapshot(); err != nil {
				slog.Warn("desktop: per-turn snapshot", "err", err)
			}
		}
		a.saveMu.Lock()
		if a.saveAgain[tabID] {
			a.saveAgain[tabID] = false
			a.saveMu.Unlock()
			continue
		}
		delete(a.saving, tabID)
		delete(a.saveAgain, tabID)
		a.saveMu.Unlock()
		return
	}
}
