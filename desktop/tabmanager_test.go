package main

import (
	"fmt"
	"sync"
	"testing"

	"reasonix/internal/control"
)

// 这一组守住 Plan 02 的验收:标签运行时只有 tabs 一份真源,「活动」只是一个 id。
// 每个用例都刻意在「切走 / 关掉目标标签」的时机上做文章 —— 只要哪天有人把活动标签
// 的 ctrl/sink/model 再镜像回 App 上,这些断言就会红。

func TestManagerRegisterActivatesAndOrders(t *testing.T) {
	m := newTabManager()
	m.Register(&tabRuntime{id: "a"})
	if m.ActiveID() != "a" {
		t.Fatalf("Register 后活动标签 = %q, want a", m.ActiveID())
	}
	m.Register(&tabRuntime{id: "b"})
	if m.ActiveID() != "b" {
		t.Fatalf("新注册的标签应成为活动标签, got %q", m.ActiveID())
	}
	list := m.List()
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("标签顺序错: %+v", list)
	}
	if list[0].Active || !list[1].Active {
		t.Fatalf("active 标志错: %+v", list)
	}
}

// TestManagerEmptyIDResolvesToActive pins the compatibility rule for the
// frontend entry points that predate multi-tab: "" means "the active tab", and
// it is resolved at read time — never cached into a mirror.
func TestManagerEmptyIDResolvesToActive(t *testing.T) {
	m := newTabManager()
	c1 := control.New(control.Options{Label: "one"})
	defer c1.Close()
	c2 := control.New(control.Options{Label: "two"})
	defer c2.Close()
	m.Register(&tabRuntime{id: "a", ctrl: c1})
	m.Register(&tabRuntime{id: "b", ctrl: c2})

	if m.Ctrl("") != c2 {
		t.Fatal(`Ctrl("") 未解析到活动标签 b`)
	}
	m.SetActive("a")
	if m.Ctrl("") != c1 {
		t.Fatal(`切换后 Ctrl("") 未解析到 a — 说明读的是缓存而不是当前 active`)
	}
	if m.Ctrl("b") != c2 {
		t.Fatal("按 id 取后台标签的 controller 失败")
	}
}

// TestManagerCloseUnknownAndLast covers the edges: closing something that isn't
// there, and closing the last tab (no tab left to activate).
func TestManagerCloseUnknownAndLast(t *testing.T) {
	m := newTabManager()
	if _, _, ok := m.Close("nope"); ok {
		t.Fatal("Close 未知 id 应报 ok=false")
	}
	m.Register(&tabRuntime{id: "only"})
	if _, next, ok := m.Close("only"); !ok || next != "" {
		t.Fatalf("关掉最后一个标签后 active 应为空, got ok=%v next=%q", ok, next)
	}
	if m.Len() != 0 || len(m.List()) != 0 {
		t.Fatal("Close 未清空注册表")
	}
	// 关掉之后不带 id 的解析必须是「没有」,而不是打到已关闭的运行时。
	if _, ok := m.View(""); ok {
		t.Fatal(`空注册表的 View("") 应报 ok=false`)
	}
}

// TestManagerCloseRejectsInFlightBuildWriteback is the A6 invariant: CloseTab
// during a slow boot.Build must make the build's write-back fail, so the build
// closes the controller it just made instead of leaking it. Removal from the
// registry is that signal — there is deliberately no second "closed" flag to
// keep in step.
func TestManagerCloseRejectsInFlightBuildWriteback(t *testing.T) {
	m := newTabManager()
	m.Register(&tabRuntime{id: "building"})

	v, _, ok := m.Close("building")
	if !ok {
		t.Fatal("Close 应成功")
	}
	if v.ctrl != nil {
		t.Fatal("正在装配的标签不该有 controller —— CloseTab 会以为该由它 Close")
	}
	// 装配完成后的写回必须落空:标签已经不在注册表里,buildTab 据此自行 Close。
	if m.Update("building", func(*tabRuntime, bool) { t.Fatal("不该写回已关闭的标签") }) {
		t.Fatal("Update 不该找到已关闭的标签")
	}
}

// TestManagerUpdateReportsActive proves a write can tell whether it landed on the
// active tab without a second, racy lookup.
func TestManagerUpdateReportsActive(t *testing.T) {
	m := newTabManager()
	m.Register(&tabRuntime{id: "a"})
	m.Register(&tabRuntime{id: "b"})

	var sawActive bool
	if !m.Update("b", func(_ *tabRuntime, active bool) { sawActive = active }) {
		t.Fatal("Update(b) 应找到标签")
	}
	if !sawActive {
		t.Fatal("b 是活动标签,Update 应报 active=true")
	}
	m.Update("a", func(_ *tabRuntime, active bool) { sawActive = active })
	if sawActive {
		t.Fatal("a 不是活动标签,Update 应报 active=false")
	}
}

// TestManagerSwitchDuringWriteLandsOnTheOriginatingTab is the A5 shape: a slow
// rebuild writes back by id, so a user switching tabs mid-build cannot make the
// result land on the wrong tab.
func TestManagerSwitchDuringWriteLandsOnTheOriginatingTab(t *testing.T) {
	m := newTabManager()
	m.Register(&tabRuntime{id: "a"})
	m.Register(&tabRuntime{id: "b"})
	m.SetActive("a")

	// "a" starts a rebuild; the user switches to "b" while it runs.
	origin := m.ActiveID()
	m.SetActive("b")

	c := control.New(control.Options{Label: "rebuilt"})
	defer c.Close()
	m.Update(origin, func(rt *tabRuntime, _ bool) { rt.ctrl = c; rt.label = "rebuilt" })

	if m.Ctrl("a") != c {
		t.Fatal("重建结果没落回发起标签 a")
	}
	if m.Ctrl("b") != nil {
		t.Fatal("重建结果串到了用户切过去的标签 b")
	}
}

// TestManagerConcurrentLifecycleIsRaceFree hammers create/switch/close/read from
// several goroutines. Run under -race this is the guard that the single source of
// truth is actually locked, not merely single.
func TestManagerConcurrentLifecycleIsRaceFree(t *testing.T) {
	m := newTabManager()
	m.Register(&tabRuntime{id: "main"})

	var wg sync.WaitGroup
	const workers = 8
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := m.NextID()
				m.Register(&tabRuntime{id: id, kind: "chat", label: fmt.Sprintf("w%d-%d", i, j)})
				m.Update(id, func(rt *tabRuntime, _ bool) { rt.ready = true })
				_, _ = m.View(id)
				_ = m.List()
				_ = m.ActiveID()
				_ = m.Controllers()
				m.SetActive("main")
				m.Close(id)
			}
		}(i)
	}
	wg.Wait()

	if m.Len() != 1 {
		t.Fatalf("并发收敛后应只剩 main, got %d 个标签", m.Len())
	}
	if m.ActiveID() != "main" {
		t.Fatalf("活动标签 = %q, want main", m.ActiveID())
	}
}

// TestManagerNilReceiverReadsAreSafe pins the bare &App{} test idiom: reads on a
// manager-less app report "no tabs" instead of panicking.
func TestManagerNilReceiverReadsAreSafe(t *testing.T) {
	var m *tabManager
	if m.ActiveID() != "" || m.Ctrl("") != nil || m.Sink("") != nil {
		t.Fatal("nil manager 的读取应返回零值")
	}
	if _, ok := m.View("x"); ok {
		t.Fatal("nil manager 的 View 应报 ok=false")
	}
	if m.Update("x", func(*tabRuntime, bool) {}) {
		t.Fatal("nil manager 的 Update 应报 false")
	}
	if _, _, ok := m.Close("x"); ok {
		t.Fatal("nil manager 的 Close 应报 false")
	}
	if m.Len() != 0 || m.List() != nil || m.Controllers() != nil {
		t.Fatal("nil manager 应报空")
	}
	if _, ok := m.SetActive("x"); ok {
		t.Fatal("nil manager 的 SetActive 应报 false")
	}
}
