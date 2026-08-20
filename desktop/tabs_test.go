package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/event"
)

// newTabTestApp 构造一个空 App(不经 boot.Build,直接塞 stub 标签)。
func newTabTestApp() *App {
	tabs := newTabManager()
	return &App{tabs: tabs, sessions: newSessionService(tabs)}
}

// seedTab 直接注册一个 stub 标签(注册即成为活动标签,与 CreateTab 一致)。
func seedTab(a *App, rt *tabRuntime) *tabRuntime {
	a.tabs.Register(rt)
	return rt
}

// TestTabRegistryActiveAndClose 验证多标签注册表的核心正确性:
// SetActiveTab 只改「哪个 id 是活动的」,不带任何镜像状态;ListTabs 顺序与 active
// 标志正确;未知 id 空操作;CloseTab 关掉活动标签后自动切到剩余标签。
func TestTabRegistryActiveAndClose(t *testing.T) {
	a := newTabTestApp()
	dir := t.TempDir()
	c1 := controllerWithContent(t, filepath.Join(dir, "s1.jsonl"))
	c2 := controllerWithContent(t, filepath.Join(dir, "s2.jsonl"))

	seedTab(a, &tabRuntime{id: "main", kind: "chat", sink: &eventSink{app: a, tabID: "main"}, ctrl: c1, label: "m", model: "x", ready: true})
	seedTab(a, &tabRuntime{id: "tab1", kind: "hardware", sink: &eventSink{app: a, tabID: "tab1"}, ctrl: c2, label: "h", model: "y", ready: true})
	a.SetActiveTab("main")

	// 切到 tab1 → 不带 tabID 的解析随之落到 tab1,且状态是从 tab1 读出来的(不是复制的)。
	a.SetActiveTab("tab1")
	v, ok := a.tabs.View("")
	if !ok || v.id != "tab1" || v.ctrl != c2 || v.label != "h" || v.model != "y" {
		t.Fatalf("SetActiveTab 后活动标签解析错: %+v (ctrl==c2:%v)", v, v.ctrl == c2)
	}
	if a.activeCtrl() != c2 {
		t.Fatal("activeCtrl 未解析到 tab1 的 controller")
	}

	// ListTabs 保持打开顺序,active 只标在 tab1。
	list := a.ListTabs()
	if len(list) != 2 || list[0].ID != "main" || list[1].ID != "tab1" {
		t.Fatalf("ListTabs 顺序错: %+v", list)
	}
	if list[0].Active || !list[1].Active {
		t.Fatalf("ListTabs active 标志错: %+v", list)
	}

	// 未知 id 空操作,不动当前活动标签。
	a.SetActiveTab("nope")
	if a.tabs.ActiveID() != "tab1" {
		t.Fatalf("SetActiveTab 对未知 id 应为空操作,却变成了 %q", a.tabs.ActiveID())
	}

	// 关掉当前活动标签 tab1 → 自动切到剩余的 main。
	a.CloseTab("tab1")
	if _, ok := a.tabs.View("tab1"); ok {
		t.Fatal("CloseTab 未从注册表移除 tab1")
	}
	if a.tabs.ActiveID() != "main" || a.activeCtrl() != c1 {
		t.Fatalf("CloseTab 关掉活动标签后未切到剩余标签 main: active=%q ctrl==c1:%v", a.tabs.ActiveID(), a.activeCtrl() == c1)
	}
	list = a.ListTabs()
	if len(list) != 1 || list[0].ID != "main" {
		t.Fatalf("CloseTab 未从标签顺序中移除 tab1: %+v", list)
	}
}

// TestPerTabAutosaveTargetsOwnSession 验证「后台标签各存各的 session」:即便活动标签是
// main,在后台 tab1 的 sink 上发 TurnDone 也只应存盘 tab1 的 session,不串到 main。
// 这是多控制器并行下自动保存最容易写错的点。
func TestPerTabAutosaveTargetsOwnSession(t *testing.T) {
	a := newTabTestApp()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.jsonl")
	tab1Path := filepath.Join(dir, "tab1.jsonl")
	cMain := controllerWithContent(t, mainPath)
	cTab1 := controllerWithContent(t, tab1Path)
	sinkTab1 := &eventSink{app: a, tabID: "tab1"}
	seedTab(a, &tabRuntime{id: "main", sink: &eventSink{app: a, tabID: "main"}, ctrl: cMain, ready: true})
	seedTab(a, &tabRuntime{id: "tab1", sink: sinkTab1, ctrl: cTab1, ready: true})
	a.SetActiveTab("main") // 活动是 main;却在后台 tab1 的 sink 上发 TurnDone

	sinkTab1.Emit(event.Event{Kind: event.TurnDone})

	// tab1 的 session 应被存盘。
	waitForFile(t, tab1Path, "acknowledged")
	// main 不应被这次后台 tab1 的 TurnDone 触发存盘。
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Fatalf("后台 tab1 的 TurnDone 不应存到活动标签 main 的 session(err=%v)", err)
	}
}
