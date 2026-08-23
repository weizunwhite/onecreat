package dsh

import (
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/provider"
)

// 回归:Controller.NewSession/Resume 会把 executor 里的会话换成新对象。引擎的
// Go 会话镜像必须每次经 SessionFunc 现取当前会话 —— 若持有装配时的指针快照,
// 换会话后 user/assistant 文本会继续写进已弃用的旧会话,History 只剩系统提示。
func TestSessionMirrorFollowsSwap(t *testing.T) {
	oldSess := agent.NewSession("system")
	newSess := agent.NewSession("system")
	current := oldSess

	e, err := New(Options{
		SessionFunc: func() *agent.Session { return current },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 第一轮:写进旧会话。
	e.mirrorUser("第一轮提问")
	e.mu.Lock()
	e.pending = []provider.Message{{Role: provider.RoleAssistant, Content: "第一轮回答"}}
	e.mu.Unlock()
	e.flushPending()

	// 模拟 Controller.NewSession:换成新会话对象。
	current = newSess

	// 第二轮:必须写进新会话,而不是旧指针。
	e.mirrorUser("第二轮提问")
	e.mu.Lock()
	e.pending = []provider.Message{{Role: provider.RoleAssistant, Content: "第二轮回答"}}
	e.mu.Unlock()
	e.flushPending()

	if got := len(oldSess.Snapshot()); got != 3 { // system + user + assistant
		t.Fatalf("旧会话应停在第一轮的 3 条消息,实际 %d 条", got)
	}
	msgs := newSess.Snapshot()
	if len(msgs) != 3 {
		t.Fatalf("新会话应有 system+user+assistant 共 3 条消息,实际 %d 条", len(msgs))
	}
	if msgs[1].Role != provider.RoleUser || msgs[1].Content != "第二轮提问" {
		t.Fatalf("新会话第 2 条应是第二轮的用户消息,实际 %+v", msgs[1])
	}
	if msgs[2].Role != provider.RoleAssistant || msgs[2].Content != "第二轮回答" {
		t.Fatalf("新会话第 3 条应是第二轮的 assistant 回复,实际 %+v", msgs[2])
	}
}

// SessionFunc 未接(纯 headless 用法)时镜像写入必须安静跳过,不 panic。
func TestSessionMirrorNilFunc(t *testing.T) {
	e, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.mirrorUser("无镜像")
	e.mu.Lock()
	e.pending = []provider.Message{{Role: provider.RoleAssistant, Content: "无镜像"}}
	e.mu.Unlock()
	e.flushPending()
}
