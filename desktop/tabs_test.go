package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/event"
)

// newTabTestApp 构造一个 maps 已初始化的空 App(不经 boot.Build,直接塞 stub 标签)。
func newTabTestApp() *App {
	return &App{
		tabs:      map[string]*tabRuntime{},
		saving:    map[string]bool{},
		saveAgain: map[string]bool{},
	}
}

// TestTabRegistryActiveMirrorAndClose 验证多标签注册表的核心正确性:
// SetActiveTab 把「活动镜像」重指到目标标签;ListTabs 顺序与 active 标志正确;
// 未知 id 空操作;CloseTab 关掉活动标签后自动切到剩余标签。
func TestTabRegistryActiveMirrorAndClose(t *testing.T) {
	a := newTabTestApp()
	dir := t.TempDir()
	c1 := controllerWithContent(t, filepath.Join(dir, "s1.jsonl"))
	c2 := controllerWithContent(t, filepath.Join(dir, "s2.jsonl"))

	a.tabs["main"] = &tabRuntime{id: "main", kind: "chat", sink: &eventSink{app: a, tabID: "main"}, ctrl: c1, label: "m", model: "x", ready: true}
	a.tabs["tab1"] = &tabRuntime{id: "tab1", kind: "hardware", sink: &eventSink{app: a, tabID: "tab1"}, ctrl: c2, label: "h", model: "y", ready: true}
	a.tabOrder = []string{"main", "tab1"}
	a.activeTab = "main"
	a.ctrl = c1

	// 切到 tab1 → 镜像字段全部重指。
	a.SetActiveTab("tab1")
	if a.activeTab != "tab1" || a.ctrl != c2 || a.label != "h" || a.model != "y" {
		t.Fatalf("SetActiveTab 未把活动镜像重指到 tab1: active=%q ctrl==c2:%v label=%q", a.activeTab, a.ctrl == c2, a.label)
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
	if a.activeTab != "tab1" {
		t.Fatalf("SetActiveTab 对未知 id 应为空操作,却变成了 %q", a.activeTab)
	}

	// 关掉当前活动标签 tab1 → 自动切到剩余的 main。
	a.CloseTab("tab1")
	if _, ok := a.tabs["tab1"]; ok {
		t.Fatal("CloseTab 未从注册表移除 tab1")
	}
	if a.activeTab != "main" || a.ctrl != c1 {
		t.Fatalf("CloseTab 关掉活动标签后未切到剩余标签 main: active=%q ctrl==c1:%v", a.activeTab, a.ctrl == c1)
	}
	if len(a.tabOrder) != 1 || a.tabOrder[0] != "main" {
		t.Fatalf("CloseTab 未从 tabOrder 移除 tab1: %v", a.tabOrder)
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
	a.tabs["main"] = &tabRuntime{id: "main", sink: &eventSink{app: a, tabID: "main"}, ctrl: cMain, ready: true}
	a.tabs["tab1"] = &tabRuntime{id: "tab1", sink: sinkTab1, ctrl: cTab1, ready: true}
	a.tabOrder = []string{"main", "tab1"}
	a.activeTab = "main" // 活动是 main;却在后台 tab1 的 sink 上发 TurnDone
	a.ctrl = cMain

	sinkTab1.Emit(event.Event{Kind: event.TurnDone})

	// tab1 的 session 应被存盘。
	waitForFile(t, tab1Path, "acknowledged")
	// main 不应被这次后台 tab1 的 TurnDone 触发存盘。
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Fatalf("后台 tab1 的 TurnDone 不应存到活动标签 main 的 session(err=%v)", err)
	}
}
