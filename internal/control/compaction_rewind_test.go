package control

import (
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
	c.mu.Lock()
	c.cpBound[1] = 1
	c.mu.Unlock()

	// 压缩前:rewind 能成功截断到下标 1。
	if err := c.Rewind(1, RewindConversation); err != nil {
		t.Fatalf("pre-compaction conversation rewind should succeed: %v", err)
	}
	if got := len(exec.Session().Snapshot()); got != 1 {
		t.Fatalf("after rewind, message count = %d, want 1", got)
	}

	// 重新建立一个边界,然后模拟一次压缩(自动/手动压缩都经 onCompact → 此方法)。
	c.mu.Lock()
	c.cpBound[2] = 1
	c.mu.Unlock()
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

	c.mu.Lock()
	c.cpBound[1] = 99 // 越界:远大于当前消息数
	c.mu.Unlock()

	if err := c.Rewind(1, RewindConversation); err == nil {
		t.Fatal("rewind with out-of-range boundary must fail")
	}
	for _, n := range notices {
		if strings.HasPrefix(n, "rewound conversation") {
			t.Fatalf("must not emit a success notice on a skipped rewind, got %q", n)
		}
	}
}
