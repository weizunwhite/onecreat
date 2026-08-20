package control

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// B1/B4 回归:压缩(用 InvalidateCheckpoints 模拟)失效了 checkpoint 边界后,对压缩前
// 的 turn 做「仅对话」rewind 必须明确报失败(发 Warn notice + 返回错误),绝不静默用
// 陈旧下标切到错误位置。
func TestConversationRewindAfterCompactionFails(t *testing.T) {
	var notices []string
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "first prompt"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "first answer"})
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "second prompt"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{
		Executor:   exec,
		SessionDir: t.TempDir(),
		Label:      "test",
		Sink: event.FuncSink(func(e event.Event) {
			if e.Kind == event.Notice {
				notices = append(notices, e.Text)
			}
		}),
	})
	c.SetSessionPath(agent.NewSessionPath(c.sessionDir, "test"))

	// 记录 turn 1 的对话边界(消息下标 1,即 "first answer" 之前)。
	c.ckpt.seedBound(1, 1)

	// 压缩前:rewind 能成功截断到下标 1。
	if err := c.Rewind(1, RewindConversation); err != nil {
		t.Fatalf("pre-compaction conversation rewind should succeed: %v", err)
	}
	if got := len(exec.Session().Snapshot()); got != 1 {
		t.Fatalf("after rewind, message count = %d, want 1", got)
	}

	// 重新建立一个边界,然后模拟一次压缩(自动/手动压缩都经 onCompact → 此方法)。
	c.ckpt.seedBound(2, 1)
	c.InvalidateCheckpoints()

	notices = notices[:0]
	err := c.Rewind(2, RewindConversation)
	if err == nil {
		t.Fatal("conversation rewind after compaction must fail, not silently succeed")
	}
	if len(notices) == 0 || !strings.Contains(notices[len(notices)-1], "turn") {
		t.Fatalf("expected a failure notice mentioning the turn, got %v", notices)
	}
}

// B4 回归:rewind 边界越界(如压缩后陈旧下标 > 当前消息数)时,必须报失败而不是被静默
// 跳过却仍 emit「rewound …」成功 notice。
func TestConversationRewindStaleBoundaryReportsFailure(t *testing.T) {
	var notices []string
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "only prompt"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{
		Executor:   exec,
		SessionDir: t.TempDir(),
		Label:      "test",
		Sink: event.FuncSink(func(e event.Event) {
			if e.Kind == event.Notice {
				notices = append(notices, e.Text)
			}
		}),
	})
	c.SetSessionPath(agent.NewSessionPath(c.sessionDir, "test"))

	c.ckpt.seedBound(1, 99) // 越界:远大于当前消息数

	if err := c.Rewind(1, RewindConversation); err == nil {
		t.Fatal("rewind with out-of-range boundary must fail")
	}
	for _, n := range notices {
		if strings.HasPrefix(n, "rewound conversation") {
			t.Fatalf("must not emit a success notice on a skipped rewind, got %q", n)
		}
	}
}

// F1 回归:对话 rewind 必须 Prune 掉被回退掉的 turn(>=T)的 checkpoint,否则复用 turn 号后
// 废弃时间线的同号快照会污染 RestoreCode(按旧内容覆盖 / 静默删文件)。
func TestConversationRewindPrunesAbandonedCheckpoints(t *testing.T) {
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "p0"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "a0"})
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "p1"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: t.TempDir(), Label: "test", Sink: event.Discard})
	c.SetSessionPath(agent.NewSessionPath(c.sessionDir, "test"))

	// 造 turn 0 / turn 1 的 checkpoint + 对话边界。
	c.ckpt.storeForTest().Begin(0, "t0", 0)
	c.ckpt.storeForTest().Begin(1, "t1", 2)
	c.ckpt.setTurn(2)
	c.ckpt.seedBound(0, 0)
	c.ckpt.seedBound(1, 2)

	if err := c.Rewind(1, RewindConversation); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	// turn>=1 的 checkpoint 应已被 Prune,不再残留污染。
	for _, m := range c.ckpt.storeForTest().List() {
		if m.Turn >= 1 {
			t.Fatalf("rewind 到 turn 1 后 turn>=1 的 checkpoint 应被清除,仍有 turn %d", m.Turn)
		}
	}
}

// F2 回归:压缩失效边界后,即使 resume(rebindCheckpoints 从磁盘重载 checkpoint),压缩前
// turn 的陈旧 MsgIndex 也不能被复活——对它做对话 rewind 必须报"不可用",而非静默切错位置。
func TestCompactionBoundsInvalidationSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "p0"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "a0"})
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test", Sink: event.Discard})
	path := agent.NewSessionPath(dir, "test")
	c.SetSessionPath(path)

	// turn 0 的对话边界(经 checkpoint store 持久化到磁盘)。
	c.ckpt.storeForTest().Begin(0, "t0", 1)
	c.ckpt.setTurn(1)
	c.ckpt.seedBound(0, 1)

	// 压缩失效边界(threshold=cpTurn=1;turn 0 < 1 → 陈旧),并把 marker 持久化。
	c.InvalidateCheckpoints()

	// 模拟 resume:同一 session 路径重新 rebind(新建 store 从磁盘加载 checkpoint + marker)。
	c.ckpt.Rebind(path)

	// turn 0 的陈旧边界不应被复活;对它做对话 rewind 必须失败,而不是静默成功切到错误位置。
	if err := c.Rewind(0, RewindConversation); err == nil {
		t.Fatal("resume 后对压缩前 turn 的对话 rewind 必须报不可用,却静默成功(陈旧边界被复活)")
	}
}

// A1 回归:Summarize(SummarizeFrom/UpTo)和压缩一样原地重写日志,必须把"边界失效"持久化;
// 否则 resume 后 rebindCheckpoints 复活陈旧 MsgIndex,对 summarize 前 turn 做对话 rewind 会
// 静默切到错误偏移(悬空 tool_calls → 请求 400)。去掉 summarizeAt 里的 InvalidateCheckpoints
// 持久化,本测试应挂。
func TestSummarizeBoundsInvalidationSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "p0"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "a0"})
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "p1"})
	// 假 provider 让 summarize() 拿到非空摘要而不真连模型。
	exec := agent.New(&classifierProvider{text: "compacted summary"}, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test", Sink: event.Discard})
	path := agent.NewSessionPath(dir, "test")
	c.SetSessionPath(path)

	// turn 0 的对话边界(经 checkpoint store 持久化到磁盘)。boundary=2 → "从这轮之后总结"。
	c.ckpt.storeForTest().Begin(0, "t0", 2)
	c.ckpt.setTurn(1)
	c.ckpt.seedBound(0, 2)

	// SummarizeFrom 原地重写日志,应像压缩一样把边界失效持久化。
	if err := c.SummarizeFrom(context.Background(), 0); err != nil {
		t.Fatalf("SummarizeFrom: %v", err)
	}

	// 模拟 resume:同一 session 路径重新 rebind(新建 store 从磁盘加载 checkpoint + marker)。
	c.ckpt.Rebind(path)

	// turn 0 的陈旧边界不应被复活;对它做对话 rewind 必须失败,而不是静默成功切到错误位置。
	if err := c.Rewind(0, RewindConversation); err == nil {
		t.Fatal("resume 后对 summarize 前 turn 的对话 rewind 必须报不可用,却静默成功(陈旧边界被复活)")
	}
}

// A2 回归:Checkpoints() 与替换 c.cp 的 rebindCheckpoints(SetSessionPath/Resume)并发时
// 不得数据竞争。裸读 c.cp 指针会被 `go test -race` 判为 race;加锁捕获后本测试应干净通过。
func TestCheckpointsConcurrentWithRebindNoRace(t *testing.T) {
	dir := t.TempDir()
	sess := agent.NewSession("sys")
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, SessionDir: dir, Label: "test", Sink: event.Discard})
	path := agent.NewSessionPath(dir, "test")
	c.SetSessionPath(path)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			c.SetSessionPath(path) // 每次都在 c.mu 下替换 c.cp 指针
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = c.Checkpoints() // 与上面的替换并发读 c.cp
	}
	<-done
}
