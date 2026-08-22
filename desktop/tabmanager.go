package main

import (
	"fmt"
	"sync"

	"reasonix/internal/control"
	"reasonix/internal/workspace"
)

// tabRuntime is everything one task tab owns: its own controller, event sink,
// session file and workspace root. Background tabs keep running in their own
// goroutines and emit to their own channel, so several tasks are genuinely
// parallel (like Codex / Claude Code's multi-tasking).
//
// kind only tells the frontend whether to show the chat or the hardware view;
// the backend does not branch on it (the hardware view just injects prompts into
// the same controller).
//
// Every field is guarded by the owning tabManager's lock. Read them through
// tabManager (View / Ctrl / Update) rather than holding the pointer.
type tabRuntime struct {
	id   string
	kind string // "chat" | "hardware"
	// ws 是这个标签实际工作的项目根目录。它是该标签运行时的真源:controller、
	// 工具、bash、MCP 全部按它解析,与进程 cwd 无关,所以两个标签可以同时开在
	// 不同项目上。
	ws         workspace.Context
	sink       *eventSink
	ctrl       *control.Controller
	label      string
	model      string
	ready      bool
	startupErr string
	// 期望的运行时门控状态(plan/YOLO/coach persona)。这些是 per-controller 运行时
	// 状态,但门控开关是全局 UI 态;存到标签上,使其在 controller 异步装配完成、或切回
	// 标签时都能被重新施加,而不是打到 nil controller 被静默吞(A8)。
	wantPlan   bool
	wantBypass bool
	coachWant  string
}

// tabView is an immutable snapshot of one tab's state, taken under the manager's
// lock. Callers work from a view instead of dereferencing a *tabRuntime, so a
// concurrent CloseTab / buildTab cannot race the read — and there is no second
// copy of the state to drift out of step.
type tabView struct {
	id         string
	kind       string
	ws         workspace.Context
	sink       *eventSink
	ctrl       *control.Controller
	label      string
	model      string
	ready      bool
	startupErr string
	active     bool
}

func (rt *tabRuntime) view(active bool) tabView {
	return tabView{
		id: rt.id, kind: rt.kind, ws: rt.ws, sink: rt.sink, ctrl: rt.ctrl,
		label: rt.label, model: rt.model, ready: rt.ready,
		startupErr: rt.startupErr, active: active,
	}
}

// tabManager owns every tab's runtime and which one is active.
//
// It exists to remove a double source of truth: the app used to keep
// `tabs[id]` *and* a mirrored copy of the active tab's controller/sink/model/
// label/ready/startupErr on the App itself, re-pointed on every switch. Two
// copies of the same state means every lifecycle path (create, build, switch,
// close, model change, workspace change) has to remember to update both, and a
// missed update shows up as the worst possible symptom: commands landing on a
// stale or already-closed controller.
//
// Here `tabs[id]` is the only runtime state; "active" is just an id. Callers
// that have a tab in hand pass its id; the transport-facing methods that
// predate multi-tab pass "" and are resolved to the active id at the edge.
//
// The manager has its own lock, held only for map/field access — never across a
// slow call like boot.Build. Do not call back into App methods while holding it.
//
// Read methods tolerate a nil receiver and report "no tabs", matching the
// package's bare &App{} test idiom (same convention as App.sh()'s noopShell
// fallback). Register does not: registering into a non-existent manager is a
// construction bug and should fail loudly.
type tabManager struct {
	mu     sync.RWMutex
	tabs   map[string]*tabRuntime
	order  []string // 标签顺序(新建追加到末尾)
	active string
	seq    int // 生成新 tab id 的自增计数
}

func newTabManager() *tabManager {
	return &tabManager{tabs: map[string]*tabRuntime{}}
}

// resolve maps "" to the active id. Callers hold the lock.
func (m *tabManager) resolve(id string) string {
	if id == "" {
		return m.active
	}
	return id
}

// Register adds rt (appending it to the tab order) and makes it active.
func (m *tabManager) Register(rt *tabRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tabs[rt.id] = rt
	m.order = append(m.order, rt.id)
	m.active = rt.id
}

// NextID mints the next tab id.
func (m *tabManager) NextID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("tab%d", m.seq)
}

// ActiveID is the active tab's id ("" when there is none).
func (m *tabManager) ActiveID() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// View snapshots one tab ("" = the active tab).
func (m *tabManager) View(id string) (tabView, bool) {
	if m == nil {
		return tabView{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	target := m.resolve(id)
	rt := m.tabs[target]
	if rt == nil {
		return tabView{}, false
	}
	return rt.view(target == m.active), true
}

// Ctrl is the controller of one tab ("" = the active tab), or nil when the tab
// does not exist or has not finished building.
func (m *tabManager) Ctrl(id string) *control.Controller {
	v, ok := m.View(id)
	if !ok {
		return nil
	}
	return v.ctrl
}

// Sink is the event sink of one tab ("" = the active tab), or nil.
func (m *tabManager) Sink(id string) *eventSink {
	v, ok := m.View(id)
	if !ok {
		return nil
	}
	return v.sink
}

// Update mutates one tab under the lock ("" = the active tab). fn also receives
// whether this tab is currently the active one, so a caller can decide what to
// publish without a second, racy lookup. Reports whether the tab existed.
func (m *tabManager) Update(id string, fn func(rt *tabRuntime, active bool)) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	target := m.resolve(id)
	rt := m.tabs[target]
	if rt == nil {
		return false
	}
	fn(rt, target == m.active)
	return true
}

// SetActive re-points the active tab. Unknown ids are a no-op; it reports the
// newly active tab's view.
func (m *tabManager) SetActive(id string) (tabView, bool) {
	if m == nil {
		return tabView{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.tabs[id]
	if rt == nil {
		return tabView{}, false
	}
	m.active = id
	return rt.view(true), true
}

// Close removes one tab and returns a snapshot of it plus the id that became
// active. The caller is responsible for snapshotting and closing the returned
// view's controller — those are slow calls and must not happen under this lock.
func (m *tabManager) Close(id string) (v tabView, nextActive string, ok bool) {
	if m == nil {
		return tabView{}, "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.tabs[id]
	if rt == nil {
		return tabView{}, m.active, false
	}
	v = rt.view(m.active == id)
	// 从注册表移除【就是】「已关闭」的信号:在途的 buildTab 之后 Update 会落空,
	// 它据此 Close 掉刚建好的 controller,不留泄漏(A6)。不再另存一个 closed 标志
	// —— 两个信号意味着两处要记得同步。
	delete(m.tabs, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	if m.active == id {
		m.active = ""
		// 关掉活动标签后激活最后一个剩余标签(与前端「关闭后落到相邻标签」一致)。
		if len(m.order) > 0 {
			next := m.order[len(m.order)-1]
			if _, exists := m.tabs[next]; exists {
				m.active = next
			}
		}
	}
	return v, m.active, true
}

// List snapshots every tab in order, for the frontend's tab bar.
func (m *tabManager) List() []TabMeta {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TabMeta, 0, len(m.order))
	for _, id := range m.order {
		rt := m.tabs[id]
		if rt == nil {
			continue
		}
		out = append(out, TabMeta{
			ID: id, Kind: rt.kind, Label: rt.label, Ready: rt.ready,
			StartupErr: rt.startupErr, Active: id == m.active,
		})
	}
	return out
}

// Controllers snapshots every live tab's controller, for shutdown. Returned
// outside the lock so the caller can do the slow Snapshot/Close work freely.
func (m *tabManager) Controllers() []*control.Controller {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*control.Controller, 0, len(m.tabs))
	for _, rt := range m.tabs {
		if rt != nil && rt.ctrl != nil {
			out = append(out, rt.ctrl)
		}
	}
	return out
}

// Len is the number of live tabs.
func (m *tabManager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tabs)
}
