package control

// AR-R03 的后半:**转录文本的真源属于引擎**。
//
// 前半(#15)已经让 Registry 记下真实引擎名,不再把每条会话都登记成 native。但
// sessionStore.save() 仍然无条件把 OneCreat 这侧的消息日志写成一份 native JSONL ——
// 对一个自己在别的进程里维护会话的引擎来说,那份文件不是它的转录文本,而是一个
// **看起来像转录文本的赝品**:磁盘上有文件、历史面板列得出来、resume 的入口看着可用,
// 打开却是空的或残缺的。复核的原话是「不得双写或用空 native JSONL 冒充 dsh」。
//
// 判据用现成的 CapResume,不新造一个能力 —— 它的定义就是「引擎的会话状态可以从
// OneCreat 侧的消息日志恢复」。能恢复,那份日志才是真源,才该落盘。

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/engine"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/session"
)

// storingEngine 声明 CapResume:OneCreat 的日志就是真源。
type storingEngine struct{ fakeEngine }

func (*storingEngine) Supports(c engine.Capability) bool {
	return c == engine.CapStreaming || c == engine.CapResume
}
func (*storingEngine) EngineName() string { return "fake-storing" }

func sessionWithContent() *agent.Session {
	s := agent.NewSession("sys")
	s.Add(provider.Message{Role: provider.RoleUser, Content: "hello"})
	return s
}

// 不声明 CapResume 的引擎:OneCreat 不得替它写一份 native 转录文本。
func TestEngineWithoutResumeGetsNoNativeTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	sess := sessionWithContent()
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)

	c := New(Options{Engine: &limitedEngine{}, Executor: exec, SessionDir: dir, SessionPath: path})
	if err := c.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("引擎没有 CapResume,却在磁盘上留下了一份 native 转录文本(%d 字节):"+
			"这正是「用 native JSONL 冒充别的引擎」\n%s", len(b), b)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat: %v", err)
	}
}

// 声明 CapResume 的引擎(native 今天的形状)行为一个字节都不变。
func TestEngineWithResumeStillWritesItsTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	sess := sessionWithContent()
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)

	c := New(Options{Engine: &storingEngine{}, Executor: exec, SessionDir: dir, SessionPath: path})
	if err := c.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("声明了 CapResume 的引擎必须照常落盘:%v", err)
	}
}

// 记录仍然要有 —— 复核要的是「Registry 记录真实 engine、stable session ID」——
// 但必须标成 ephemeral:身份留着,别谎称本地有可重开的转录文本。
func TestEngineWithoutResumeIsRegisteredAsEphemeral(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	sess := sessionWithContent()
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)

	New(Options{Engine: &limitedEngine{}, Executor: exec, SessionDir: dir, SessionPath: path})

	reg := session.Open(dir)
	rec, ok := reg.ByStore(path)
	if !ok {
		t.Fatal("记录不该消失 —— 身份、项目、真实引擎名都要留着")
	}
	if rec.Engine != "fake-limited" {
		t.Fatalf("引擎名 = %q", rec.Engine)
	}
	if !rec.Ephemeral {
		t.Fatal("没有 CapResume 的引擎,记录必须标成 ephemeral —— " +
			"否则前端会提供 resume/history,打开却是一个从未被写过的文件")
	}
}

// 而 native 那条必须照常登记,且**不是** ephemeral。
func TestEngineWithResumeIsRegistered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	sess := sessionWithContent()
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)

	New(Options{Engine: &storingEngine{}, Executor: exec, SessionDir: dir, SessionPath: path})

	reg := session.Open(dir)
	rec, ok := reg.ByStore(path)
	if !ok {
		t.Fatal("声明了 CapResume 的引擎必须照常登记会话记录")
	}
	if rec.Ephemeral {
		t.Fatal("会落盘的引擎不该被标成 ephemeral")
	}
}
