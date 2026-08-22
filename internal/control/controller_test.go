package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type typedNilControllerSink struct{}

func (*typedNilControllerSink) Emit(event.Event) {}

type appendingRunner struct {
	session *agent.Session
}

func (r appendingRunner) Run(_ context.Context, input string) error {
	r.session.Add(provider.Message{Role: provider.RoleUser, Content: input})
	return nil
}

type fakeControlTool struct{ name string }

func (t fakeControlTool) Name() string { return t.name }
func (fakeControlTool) Description() string {
	return "fake"
}
func (fakeControlTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (fakeControlTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
func (fakeControlTool) ReadOnly() bool { return true }

func TestNewTreatsTypedNilSinkAsDiscard(t *testing.T) {
	var sink *typedNilControllerSink
	c := New(Options{Sink: sink})

	c.notice("typed nil sink should not panic")
}

func TestRunTurnSnapshotsActivityWhenTranscriptChanges(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	path := filepath.Join(dir, "session.jsonl")
	c := New(Options{Runner: appendingRunner{session: sess}, Executor: exec, SessionDir: dir, SessionPath: path, Label: "test"})

	if err := c.runTurn(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	loaded, err := agent.LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("saved messages = %d, want system + user", len(loaded.Messages))
	}
	meta, ok, err := agent.LoadBranchMeta(path)
	if err != nil || !ok {
		t.Fatalf("load activity meta ok=%v err=%v", ok, err)
	}
	if meta.UpdatedAt.IsZero() {
		t.Fatal("activity meta should be marked")
	}
}

func TestSnapshotDoesNotRefreshSessionActivity(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "first"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test"})
	c.SetSessionPath(filepath.Join(dir, "session.jsonl"))

	if err := c.SnapshotActivity(); err != nil {
		t.Fatal(err)
	}
	first, ok, err := agent.LoadBranchMeta(c.SessionPath())
	if err != nil || !ok {
		t.Fatalf("load initial meta ok=%v err=%v", ok, err)
	}

	time.Sleep(10 * time.Millisecond)
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "saved without activity"})
	if err := c.Snapshot(); err != nil {
		t.Fatal(err)
	}
	second, ok, err := agent.LoadBranchMeta(c.SessionPath())
	if err != nil || !ok {
		t.Fatalf("load second meta ok=%v err=%v", ok, err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("Snapshot refreshed activity: first=%s second=%s", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestSnapshotActivityRefreshesSessionActivity(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "first"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test"})
	c.SetSessionPath(filepath.Join(dir, "session.jsonl"))

	if err := c.SnapshotActivity(); err != nil {
		t.Fatal(err)
	}
	first, _, err := agent.LoadBranchMeta(c.SessionPath())
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "activity"})
	if err := c.SnapshotActivity(); err != nil {
		t.Fatal(err)
	}
	second, _, err := agent.LoadBranchMeta(c.SessionPath())
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("SnapshotActivity did not refresh activity: first=%s second=%s", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestDisconnectMCPServerRemovesLazyPlaceholder(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(fakeControlTool{name: "mcp__mock__connect"})
	c := New(Options{Host: plugin.NewHost(), Registry: reg})

	if ok := c.DisconnectMCPServer("mock"); !ok {
		t.Fatal("DisconnectMCPServer returned false for a registered lazy placeholder")
	}
	if _, found := reg.Get("mcp__mock__connect"); found {
		t.Fatalf("lazy placeholder still registered after disconnect; names=%v", reg.Names())
	}
}

func TestRemoveMCPServerRemovesUnconnectedLazyPlaceholder(t *testing.T) {
	// 1b:MCP 增删改走用户级配置(UserConfigPath = <REASONIX_CONFIG_DIR>/config.toml),
	// 不再写项目 reasonix.toml。把 lazy 占位服务器写进隔离的用户级配置,验证 RemoveMCPServer
	// 从那里删掉。t.Chdir 到空目录排除项目级 toml 干扰 config.Load()。
	cfgDir := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", cfgDir)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(`
[[plugins]]
name = "mock"
command = "mock-mcp"
tier = "lazy"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Add(fakeControlTool{name: "mcp__mock__connect"})
	c := New(Options{Host: plugin.NewHost(), Registry: reg})

	disconnected, err := c.RemoveMCPServer("mock")
	if err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	if disconnected {
		t.Fatal("RemoveMCPServer reported a live disconnect for an unconnected lazy placeholder")
	}
	if _, found := reg.Get("mcp__mock__connect"); found {
		t.Fatalf("lazy placeholder still registered after remove; names=%v", reg.Names())
	}
	if names := c.ConfiguredMCPNames(); len(names) != 0 {
		t.Fatalf("ConfiguredMCPNames() = %v, want empty after remove", names)
	}
	// 1b:绝不在项目目录落 onecreat.toml/reasonix.toml——写项目级会把全量快照落进项目
	// 文件、遮蔽用户级配置。
	for _, name := range []string{"onecreat.toml", "reasonix.toml"} {
		if _, serr := os.Stat(name); !os.IsNotExist(serr) {
			t.Fatalf("MCP 操作不应创建项目级 %s(会遮蔽用户级配置)", name)
		}
	}
}

// A3:服务器只声明在项目 onecreat.toml → RemoveMCPServer 外科式从项目文件删块,不写全量
// 快照(项目文件不多出 [[providers]]/default_model 无关键),也不创建用户级文件。
func TestRemoveMCPServerProjectLevelSurgical(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", cfgDir)
	t.Chdir(t.TempDir())
	proj := `default_model = "deepseek-pro"

[[plugins]]
name = "mock"
command = "mock-mcp"
tier = "lazy"
`
	if err := os.WriteFile("onecreat.toml", []byte(proj), 0o644); err != nil {
		t.Fatalf("write project toml: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Add(fakeControlTool{name: "mcp__mock__connect"})
	c := New(Options{Host: plugin.NewHost(), Registry: reg})

	disconnected, err := c.RemoveMCPServer("mock")
	if err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	if disconnected {
		t.Fatal("reported a live disconnect for an unconnected lazy placeholder")
	}
	if _, found := reg.Get("mcp__mock__connect"); found {
		t.Fatalf("lazy placeholder still registered after remove; names=%v", reg.Names())
	}
	out, _ := os.ReadFile("onecreat.toml")
	text := string(out)
	if strings.Contains(text, "mock") {
		t.Fatalf("项目 toml 里 mock 块未删:\n%s", text)
	}
	if !strings.Contains(text, `default_model = "deepseek-pro"`) {
		t.Fatalf("项目 toml 的 default_model 被改动:\n%s", text)
	}
	if strings.Contains(text, "[[providers]]") {
		t.Fatalf("项目 toml 被撑成全量快照(多出 [[providers]]):\n%s", text)
	}
	// 项目级删除不应落用户级文件。
	if _, serr := os.Stat(filepath.Join(cfgDir, "config.toml")); !os.IsNotExist(serr) {
		t.Fatal("项目级删除不应创建用户级 config.toml")
	}
	if names := c.ConfiguredMCPNames(); len(names) != 0 {
		t.Fatalf("ConfiguredMCPNames() = %v, want empty after remove", names)
	}
}

// A3:服务器声明在项目根 .mcp.json → 不由我们写回;RemoveMCPServer 本会话断开 + 发 notice
// 告知需手动编辑该文件,不再静默复活,且不改动 .mcp.json。
func TestRemoveMCPServerFromMCPJSONEmitsNotice(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", cfgDir)
	t.Chdir(t.TempDir())
	mcpJSON := `{"mcpServers":{"mock":{"command":"mock-mcp"}}}`
	if err := os.WriteFile(".mcp.json", []byte(mcpJSON), 0o644); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}

	var notices []string
	reg := tool.NewRegistry()
	reg.Add(fakeControlTool{name: "mcp__mock__connect"})
	c := New(Options{
		Host:     plugin.NewHost(),
		Registry: reg,
		Sink: event.FuncSink(func(e event.Event) {
			if e.Kind == event.Notice {
				notices = append(notices, e.Text)
			}
		}),
	})

	disconnected, err := c.RemoveMCPServer("mock")
	if err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	if disconnected {
		t.Fatal("reported a live disconnect for an unconnected lazy placeholder")
	}
	found := false
	for _, n := range notices {
		if strings.Contains(n, ".mcp.json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a notice mentioning .mcp.json, got %v", notices)
	}
	out, _ := os.ReadFile(".mcp.json")
	if string(out) != mcpJSON {
		t.Fatalf(".mcp.json 被改动,应保持原样:\n%s", string(out))
	}
	if _, ok := reg.Get("mcp__mock__connect"); ok {
		t.Fatalf("lazy placeholder still registered after remove; names=%v", reg.Names())
	}
}

// approvalIDs returns a Controller whose Sink forwards each ApprovalRequest's ID
// onto the channel, plus a counter of how many requests it emitted.
func approvalIDs() (*Controller, chan string, *int) {
	ids := make(chan string, 8)
	prompts := 0
	c := New(Options{Sink: event.FuncSink(func(e event.Event) {
		if e.Kind == event.ApprovalRequest {
			prompts++
			ids <- e.Approval.ID
		}
	})})
	return c, ids, &prompts
}

// TestApprovalAllowOnce drives the happy path: the gate emits an ApprovalRequest,
// the (fake) frontend answers allow, and the gate returns allow with no grant.
func TestApprovalAllowOnce(t *testing.T) {
	c, ids, _ := approvalIDs()
	go func() { c.Approve(<-ids, true, false) }()

	allow, remember, err := c.approvals.Approve(context.Background(), "bash", "go test", nil)
	if err != nil || !allow || remember {
		t.Fatalf("Approve = (%v,%v,%v), want allow once", allow, remember, err)
	}
}

// TestApprovalDeny confirms a declined call returns allow=false.
func TestApprovalDeny(t *testing.T) {
	c, ids, _ := approvalIDs()
	go func() { c.Approve(<-ids, false, false) }()

	allow, _, err := c.approvals.Approve(context.Background(), "bash", "rm -rf /", nil)
	if err != nil || allow {
		t.Fatalf("Approve = (%v,%v), want deny", allow, err)
	}
}

// TestApprovalSessionGrant proves an "allow this session" answer short-circuits
// later prompts for the same tool+subject: only the first reaches the frontend.
func TestApprovalSessionGrant(t *testing.T) {
	c, ids, prompts := approvalIDs()
	// Only the first call reaches the frontend (the session grant short-circuits
	// the rest), so a single approval is all this needs — ranging would block on
	// a second ID that never arrives.
	go func() { c.Approve(<-ids, true, true) }()

	for i := 0; i < 3; i++ {
		allow, _, err := c.approvals.Approve(context.Background(), "bash", "go build", nil)
		if err != nil || !allow {
			t.Fatalf("call %d = (%v,%v), want allow", i, allow, err)
		}
	}
	if *prompts != 1 {
		t.Errorf("prompted %d times, want 1 (session grant should short-circuit)", *prompts)
	}
}

// TestApprovalCtxCancel ensures a cancelled turn unblocks the gate with an error
// (rather than hanging) when no one answers.
func TestApprovalCtxCancel(t *testing.T) {
	c := New(Options{Sink: event.Discard})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	allow, _, err := c.approvals.Approve(ctx, "bash", "x", nil)
	if err == nil || allow {
		t.Fatalf("Approve on cancelled ctx = (%v,%v), want (false, error)", allow, err)
	}
}
