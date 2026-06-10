package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	ch := make(chan provider.Chunk, 1)
	close(ch)
	return ch, nil
}

func controllerWithContent(t *testing.T, path string) *control.Controller {
	t.Helper()
	sess := agent.NewSession("system")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "remember this turn"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "acknowledged"})
	ag := agent.New(stubProvider{}, tool.NewRegistry(), sess, agent.Options{}, event.Discard)
	return control.New(control.Options{Executor: ag, SessionPath: path, Sink: event.Discard})
}

// newAutosaveTestApp 按多标签结构装好一个测试 App:maps 初始化 + 注册 main 标签 +
// sink 归属 main,这样 per-tab 自动保存(scheduleSnapshot/snapshotLoop 按 tab 查 ctrl)
// 能命中。替代旧的 &App{ctrl: ...} 直接构造。
func newAutosaveTestApp(ctrl *control.Controller) *App {
	a := &App{
		tabs:      map[string]*tabRuntime{},
		saving:    map[string]bool{},
		saveAgain: map[string]bool{},
	}
	a.sink = &eventSink{app: a, tabID: "main"}
	a.tabs["main"] = &tabRuntime{id: "main", kind: "chat", sink: a.sink, ctrl: ctrl, ready: true}
	a.tabOrder = []string{"main"}
	a.activeTab = "main"
	a.ctrl = ctrl
	return a
}

func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session file %q never contained %q", path, want)
}

// TestTurnDonePersistsSession proves a completed turn is written to disk without
// any explicit Snapshot call — the desktop autosave the data-loss fix adds. A
// nil sink ctx (no webview) must not disable persistence.
func TestTurnDonePersistsSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	a := newAutosaveTestApp(controllerWithContent(t, path))

	a.sink.Emit(event.Event{Kind: event.TurnDone})

	waitForFile(t, path, "remember this turn")
}

// TestNonTurnDoneDoesNotPersist confirms only TurnDone triggers a save, so the
// per-token event storm doesn't thrash the disk.
func TestNonTurnDoneDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	a := newAutosaveTestApp(controllerWithContent(t, path))

	a.sink.Emit(event.Event{Kind: event.Text, Text: "tok"})

	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a non-TurnDone event wrote the session file (err=%v)", err)
	}
}

// TestScheduleSnapshotCoalesces hammers the scheduler concurrently to prove the
// single-flight loop neither panics nor drops the final write.
func TestScheduleSnapshotCoalesces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	a := newAutosaveTestApp(controllerWithContent(t, path))

	for i := 0; i < 64; i++ {
		go a.scheduleSnapshot("main")
	}

	waitForFile(t, path, "acknowledged")
}
