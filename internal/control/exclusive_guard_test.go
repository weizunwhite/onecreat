package control

import (
	"context"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
)

// H1 回归:一个「重写会话日志」的 op 在飞行中(busy,如 /compact 阻塞在多秒摘要网络
// 调用、/new 在 Snapshot)时,并发的一轮 turn 绝不能经 runGuarded 启动 —— 否则它
// Session.Add 的消息会被 op 基于旧快照的 Replace/SetSession 整轮覆盖丢掉(B2 的反向:
// 既有守卫只挡 turn→op,这里钉住补上的 op→turn)。
// 这里用 tryBeginExclusive 模拟 op 飞行中,断言 Send 不启动 turn(runGuarded 见 busy
// 直接返回、不 spawn goroutine,所以同步可断言)。
func TestExclusiveOpBlocksConcurrentTurn(t *testing.T) {
	runner := &fakeTurnRunner{}
	c := New(Options{Runner: runner})

	if !c.tryBeginExclusive() {
		t.Fatal("空闲态应能进入独占临界区")
	}
	c.SendWithRaw("hi", "hi") // busy 飞行中:runGuarded 应直接返回、不启动并发 turn
	if len(runner.inputs) != 0 {
		t.Fatalf("op 飞行中不应启动并发 turn,但 runner 被调用了: %v", runner.inputs)
	}
	c.endExclusive()

	// 释放后应能正常进入下一个独占 op(守卫不会卡死)。
	if !c.tryBeginExclusive() {
		t.Fatal("endExclusive 后应能再次进入独占临界区")
	}
	c.endExclusive()
}

// H1 回归(既有方向):一轮 turn 进行中(running)时,会话重写 op 必须被拒并返回错误,
// 而不是一边 turn Add、一边 op Replace 互相破坏。
func TestRunningTurnBlocksExclusiveOp(t *testing.T) {
	sess := agent.NewSession("sys")
	exec := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	c := New(Options{Executor: exec})

	c.mu.Lock()
	c.running = true // 模拟一轮 turn 进行中
	c.mu.Unlock()

	if c.tryBeginExclusive() {
		t.Fatal("turn 进行中不应允许独占 op 进入")
	}
	if err := c.Compact(context.Background(), ""); err == nil {
		t.Fatal("turn 进行中 /compact 应被拒并返回错误")
	}
	if err := c.NewSession(); err == nil {
		t.Fatal("turn 进行中 /new 应被拒并返回错误")
	}
}
