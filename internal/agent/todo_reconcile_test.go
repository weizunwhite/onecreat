package agent

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// countReconcileReminders 统计本会话里注入了几条 turn-end 对账提醒。
func countReconcileReminders(s *Session) int {
	n := 0
	for _, m := range s.Messages {
		if m.Role == provider.RoleUser && strings.Contains(m.Content, "[系统对账提醒]") {
			n++
		}
	}
	return n
}

func registryWithTodoWrite(t *testing.T) *tool.Registry {
	t.Helper()
	todoWrite, ok := tool.LookupBuiltin("todo_write")
	if !ok {
		t.Fatal("todo_write builtin not registered")
	}
	reg := tool.NewRegistry()
	reg.Add(todoWrite)
	return reg
}

// TestTodoReconcileRemindsOnUnfinishedList 复现 3/6 冻结场景:本轮更新过 todo
// 且还有 pending/in_progress 项时,模型直接收尾应被拦下一次,注入对账提醒后
// 再跑一轮。
func TestTodoReconcileRemindsOnUnfinishedList(t *testing.T) {
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{
			toolCallChunk("c1", "todo_write", `{"todos":[
				{"content":"Create project","status":"completed"},
				{"content":"Write code","status":"in_progress"},
				{"content":"Deploy","status":"pending"}
			]}`),
			{Type: provider.ChunkDone},
		},
		// 模型不再调工具,直接给最终回复 —— 应触发一次对账提醒。
		{{Type: provider.ChunkText, Text: "全部搞定了"}, {Type: provider.ChunkDone}},
	}}

	a := New(prov, registryWithTodoWrite(t), NewSession(""), Options{}, event.Discard)
	if err := a.Run(context.Background(), "do the work"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countReconcileReminders(a.session); got != 1 {
		t.Fatalf("reminders = %d, want exactly 1", got)
	}
	// 提醒注入后必须真的多跑了一轮(turn 2 的脚本被重放为 turn 3)。
	if prov.call != 3 {
		t.Fatalf("provider calls = %d, want 3 (work, ended-early, post-reminder)", prov.call)
	}
}

// TestTodoReconcileRemindsAtMostOnce 提醒一次后模型仍坚持收尾,接受其答案,
// 不允许无限拉锯。
func TestTodoReconcileRemindsAtMostOnce(t *testing.T) {
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{
			toolCallChunk("c1", "todo_write", `{"todos":[{"content":"Write code","status":"in_progress"}]}`),
			{Type: provider.ChunkDone},
		},
		// 之后每轮都是纯文本 —— 提醒一次后必须放行。
		{{Type: provider.ChunkText, Text: "就这样吧"}, {Type: provider.ChunkDone}},
	}}

	a := New(prov, registryWithTodoWrite(t), NewSession(""), Options{}, event.Discard)
	if err := a.Run(context.Background(), "do the work"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countReconcileReminders(a.session); got != 1 {
		t.Fatalf("reminders = %d, want exactly 1 (latched)", got)
	}
}

// TestTodoReconcileSilentWhenListComplete 清单全 completed 时不打扰。
func TestTodoReconcileSilentWhenListComplete(t *testing.T) {
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{
			toolCallChunk("c1", "todo_write", `{"todos":[{"content":"Write code","status":"completed"}]}`),
			{Type: provider.ChunkDone},
		},
		{{Type: provider.ChunkText, Text: "done"}, {Type: provider.ChunkDone}},
	}}

	a := New(prov, registryWithTodoWrite(t), NewSession(""), Options{}, event.Discard)
	if err := a.Run(context.Background(), "do the work"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countReconcileReminders(a.session); got != 0 {
		t.Fatalf("reminders = %d, want 0 for a fully completed list", got)
	}
}

// TestTodoReconcileSilentWithoutTodoWrite 本轮根本没碰 todo 清单(没有进度
// 声明)时,直接放行 —— 不替上一轮的旧清单催账。
func TestTodoReconcileSilentWithoutTodoWrite(t *testing.T) {
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{{Type: provider.ChunkText, Text: "直接回答"}, {Type: provider.ChunkDone}},
	}}

	a := New(prov, registryWithTodoWrite(t), NewSession(""), Options{}, event.Discard)
	if err := a.Run(context.Background(), "just a question"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countReconcileReminders(a.session); got != 0 {
		t.Fatalf("reminders = %d, want 0 when the turn never touched todos", got)
	}
}
