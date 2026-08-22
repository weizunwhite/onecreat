package dsh

// TurnEngine 把这个包的 sidecar 驱动接到 `engine.TurnEngine` 边界上。
//
// Plan 12 / A14 的验收就是这一层的形状:dsh 是一个 **Engine Adapter**,不是第二套
// OneCreat。它下面没有权限门、没有检查点、没有证据链、没有记忆 —— 那些全部留在
// Controller 那一侧,对两种引擎一视同仁;引擎只经由 Options 上的注入函数
// (Decide / Approver / PreEdit / Tools / Ledger)被动地回调上去。
// 这个文件里出现任何一个应用策略包的 import,都说明 A14 又回来了;
// `engine/boundary_test.go` 会因此变红。
//
// 它取代了此前建在 rc.7 spike 上的 `adapter.go`:那一版把「一轮」表达成
// `Submit + 等 turn/end 通知`,而现在的 `Engine.Run` 本身就是同步的、一轮收敛才
// 返回,于是这里可以和 `native.Engine.Start` 逐字同形 —— 派生 ctx、goroutine、
// `defer cancel()`、`Wait` 绝不在 goroutine 退出前返回。

import (
	"context"
	"sync"

	"reasonix/internal/engine"
)

// Name 是 dsh 引擎在配置与会话记录里的名字。
const Name = "dsh"

// caps 是 dsh sidecar 的能力声明 —— 照实说,不多不少:
//
//   - streaming:文本 / 推理 / 工具进度都经 mapper 流向 sink。
//   - approval:成立。dsh-tools 的调度器在**派发之前**跑 `tools/pre-execute`
//     waterfall,控制面插件把它 await 到 Go 侧,`handlePreExecute` 在这里跑权限
//     策略并在需要时阻塞等审批;拒绝时工具**不执行**。通道断开 / turn 取消一律
//     判 deny(fail-closed)。
//   - resume:成立。dsh 会话 id 由 Go 会话文件路径派生,`onecreat/session.load`
//     从 dsh 自己的 store 恢复,跨进程 resume 实测过(docs/dsh调研/06 §2.4d)。
//   - gated-tools:见 engine.CapGatedTools —— 工具在 sidecar 进程里执行,但每一次
//     调用在派发前阻塞等 OneCreat 裁决。
//
// **不**声明的两条,都不是保守起见,是真的没有:
//
//   - fork:会话日志的真源在 dsh 侧,照着 OneCreat 本地的消息镜像 rewind / branch
//     只会让两边悄悄对不上(手动 compact 同理,见 Controller 的 CapFork 口径)。
//   - hosted-tools:工具**不**在 OneCreat 进程里执行。门禁时点相同,执行位置不同 ——
//     这条差别对 checkpoint 之外的东西无所谓,但它必须照实说,否则将来有人拿
//     CapHostedTools 当"工具在本地"的依据时会踩空。
var caps = engine.Set{
	engine.CapStreaming:  true,
	engine.CapApproval:   true,
	engine.CapResume:     true,
	engine.CapGatedTools: true,
}

// Capabilities 返回 dsh 引擎的能力集合,不需要先构造引擎。装配根用它做
// fail-closed 判断 —— 复用同一份声明,不另写一套。
func Capabilities() engine.Set { return caps }

// TurnEngine 实现 engine.TurnEngine + engine.Capable。
type TurnEngine struct{ inner *Engine }

// NewTurnEngine 把一个已构造的 sidecar 引擎包成 TurnEngine。
func NewTurnEngine(inner *Engine) *TurnEngine { return &TurnEngine{inner: inner} }

// Inner 交出底层引擎。装配根需要它来接那些**不属于**引擎边界的东西
// (会话绑定、计划模式下发、关闭),按 A14 这些是应用策略,不上 TurnEngine 接口。
func (t *TurnEngine) Inner() *Engine { return t.inner }

// EngineName 实现 engine.Named。
func (t *TurnEngine) EngineName() string { return Name }

// Supports 声明能力。
func (t *TurnEngine) Supports(c engine.Capability) bool { return caps.Supports(c) }

// Capabilities 返回能力集合(诊断用)。
func (t *TurnEngine) Capabilities() engine.Set { return caps }

// Start 在自己的 goroutine 上跑这一轮,立刻返回句柄。与 native 同形。
func (t *TurnEngine) Start(ctx context.Context, req engine.TurnRequest) (engine.TurnHandle, error) {
	cctx, cancel := context.WithCancel(ctx)
	h := &turnHandle{inner: t.inner, cancel: cancel, done: make(chan struct{})}
	go func() {
		// cancel 必须在 goroutine 退出时无条件释放:正常跑完时没人会再调 Cancel(),
		// 不主动放掉的话这个派生 ctx 会一直挂在父 ctx 上,直到整个会话结束。
		defer cancel()
		defer close(h.done)
		h.err = t.inner.Run(cctx, req.Input)
	}()
	return h, nil
}

// turnHandle 是 dsh 一轮的句柄。
type turnHandle struct {
	inner  *Engine
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
	// err 只由运行 goroutine 写、且只在 close(done) 之前写;读方必须先 <-done。
	err error
}

// Cancel 中止这一轮(幂等)。
//
// 两件事都要做,少一件都是半个取消:派生 ctx 负责让 Run 自己的等待醒过来,
// inner.Cancel() 经 wire 的 `onecreat/session.cancel` 告诉 sidecar 真的停下 ——
// 后者是 feat 这套驱动有、而先前建在 spike 上的适配器没有的**真**取消
// (那一版只能杀进程)。
func (h *turnHandle) Cancel() error {
	var err error
	h.once.Do(func() {
		err = h.inner.Cancel()
		h.cancel()
	})
	return err
}

// Wait 阻塞到运行 goroutine 真正退出。
//
// 它**不**在 ctx 取消时提前返回:提前返回会让 Controller 在引擎还在写会话镜像的
// 时候去读它,正好撞上「单写者 + 同 goroutine 免锁读」这条内核红线。ctx 取消的
// 正确效果是让那个 goroutine 自己结束,然后这里自然返回。
func (h *turnHandle) Wait(context.Context) error {
	<-h.done
	return h.err
}
