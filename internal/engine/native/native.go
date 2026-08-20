// Package native adapts OneCreat 内置的 Go 内核(`agent.Runner` —— 单模型 Agent 或
// 双模型 Coordinator)到 `engine.TurnEngine` 边界。
//
// 这个适配器**故意什么策略都不做**:记忆、证据、权限、计费、检查点全部留在
// Controller 那一侧。它唯一的工作是把「同步跑一轮」翻译成「开始 + 等待 + 取消」。
package native

import (
	"context"
	"sync"

	"reasonix/internal/agent"
	"reasonix/internal/engine"
)

// caps 是内置内核的能力:四项全有。它拥有会话日志本身,所以 resume / fork 都是
// 它能兑现的承诺(rewind、branch 都建立在这上面)。
var caps = engine.Set{
	engine.CapStreaming: true,
	engine.CapApproval:  true,
	engine.CapResume:    true,
	engine.CapFork:      true,
}

// Engine 是内置内核的 TurnEngine 适配器。
type Engine struct {
	runner agent.Runner
}

// New 包一个 runner(executor 或 coordinator)成 TurnEngine。
func New(r agent.Runner) *Engine { return &Engine{runner: r} }

// Supports 声明内置内核的能力。
func (e *Engine) Supports(c engine.Capability) bool { return caps.Supports(c) }

// Capabilities 返回能力集合(诊断用)。
func (e *Engine) Capabilities() engine.Set { return caps }

// Start 在自己的 goroutine 上跑这一轮,立刻返回句柄。
//
// 取消沿用既有的**唯一**触发源:传进来的 ctx。句柄从它派生一个子 ctx,于是
// Cancel() 与「上层取消这一轮」是同一件事的两个说法,不是第二条取消链路。
func (e *Engine) Start(ctx context.Context, req engine.TurnRequest) (engine.TurnHandle, error) {
	cctx, cancel := context.WithCancel(ctx)
	h := &handle{cancel: cancel, done: make(chan struct{})}
	go func() {
		// cancel 必须在 goroutine 退出时无条件释放:正常跑完时没人会再去调
		// Cancel(),不主动放掉的话这个派生 ctx 会一直挂在父 ctx 上,直到整个
		// 会话结束才被回收。
		defer cancel()
		defer close(h.done)
		h.err = e.runner.Run(cctx, req.Input)
	}()
	return h, nil
}

// handle 是一轮内置内核的句柄。
type handle struct {
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
	// err 只由运行 goroutine 写、且只在 close(done) 之前写;读方必须先 <-done,
	// 由 channel close 的 happens-before 保证可见性与无竞态。
	err error
}

// Cancel 取消这一轮(幂等)。
func (h *handle) Cancel() error {
	h.once.Do(func() { h.cancel() })
	return nil
}

// Wait 阻塞到运行 goroutine 真正退出。
//
// 它**不**在 ctx 取消时提前返回 —— 提前返回会让 Controller 在 agent 还在写
// `Session` 的时候去读它,正好撞上「单写者 + 同 goroutine 免锁读」这条内核红线。
// ctx 取消的正确效果是让那个 goroutine 自己结束,然后这里自然返回。
func (h *handle) Wait(context.Context) error {
	<-h.done
	return h.err
}
