package dsh

// Adapter 把这个包的 sidecar 驱动接到 `engine.TurnEngine` 边界上。
//
// Plan 12 的验收就是这一行代码的形状:dsh 是一个 **Engine Adapter**,不是第二套
// OneCreat。它下面没有权限门、没有检查点、没有证据链、没有记忆 —— 那些全部留在
// Controller 那一侧,对两种引擎一视同仁。这个文件里出现任何一个应用策略包的
// import,都说明 A14 又回来了;`engine_boundary_test.go` 会因此变红。

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"reasonix/internal/engine"
	"reasonix/internal/event"
)

// caps 是 dsh sidecar 的能力 —— 只有流式。其余三项**故意**不声明,而且都不是保守
// 起见,是真的没有:
//
//   - approval:dsh 在自己的进程里跑自己的工具,OneCreat 的权限门够不着它;
//   - resume / fork:会话日志在 dsh 那边,OneCreat 侧的消息日志不是它的真源,
//     照着本地日志 rewind / branch 只会让两边悄悄对不上;
//   - hosted-tools:**这一条是安全边界,不是功能多寡。** dsh 在自己的进程里跑自己的
//     工具,等它把 tool/call 推过来时,文件已经写完、shell 已经跑过了 —— 权限门、
//     plan mode 只读门、PreToolUse hook、写前检查点、证据链全都够不着。事后看一眼
//     不叫门禁。装配根据此 fail-closed(见 boot.selectEngine),在 dsh 协议支持把工具
//     调用委托回 OneCreat 执行之前,这个引擎不允许被启用。
//
// 与其假装支持然后静默走样,不如在这里说实话,让上层能拒绝。
var caps = engine.Set{engine.CapStreaming: true}

// Name 是 dsh 引擎在配置与会话记录里的名字。
const Name = "dsh"

// Capabilities 返回 dsh 引擎的能力集合,不需要先构造适配器。装配根用它做
// fail-closed 判断 —— 复用同一份声明,不另写一套。
func Capabilities() engine.Set { return caps }

// AdapterOptions 配置一个 dsh 引擎适配器。
type AdapterOptions struct {
	// Options 是底层 sidecar 驱动的配置(bin_path、CWD、网关 token、脱敏串)。
	// 其中 Sink 是这个会话的事件出口;OnTurnEnd 由适配器自己接管,传了也会被覆盖。
	Options
	// SessionID 是下发给 dsh 的会话标识。为空时用 DefaultSessionID。
	SessionID string
}

// DefaultSessionID 是没有配置时下发给 dsh 的会话标识。
//
// 已知限制:dsh 的 JSON-RPC 目前没有「新建会话」方法,所以一个 sidecar 进程就是
// 一个会话。这正是它不声明 CapResume / CapFork 的原因,不是随手挑的默认值。
const DefaultSessionID = "onecreat"

// Adapter 实现 engine.TurnEngine + engine.Capable。
type Adapter struct {
	inner     *Engine
	sessionID string

	mu  sync.Mutex
	cur *turn
}

// NewAdapter 构造适配器(尚未拉起子进程,调 Boot 才拉起)。
//
// 它替换 Sink 与 OnTurnEnd 这两项:dsh 自己会把 turn/end 映射成 event.TurnDone,
// 而 Controller 的回合状态机在一轮收尾时也会发一个 —— 两个 TurnDone 就是两个真源,
// UI 会看到一轮结束两次。这里的取舍与 native 一致:**TurnStarted 归引擎,TurnDone
// 归 Controller**,所以引擎侧那一条在进 sink 之前就被吞掉,同一条通知转而喂给 Wait。
func NewAdapter(opts AdapterOptions) (*Adapter, error) {
	a := &Adapter{sessionID: opts.SessionID}
	if a.sessionID == "" {
		a.sessionID = DefaultSessionID
	}
	inner := opts.Options
	if inner.Sink == nil {
		inner.Sink = event.Discard
	}
	inner.Sink = dropTurnDone{inner: inner.Sink}
	inner.OnTurnEnd = a.finishTurn
	e, err := New(inner)
	if err != nil {
		return nil, err
	}
	a.inner = e
	return a, nil
}

// Boot 拉起 sidecar 并完成握手。装配期就调用它,好让配置错误在装配时就炸出来,
// 而不是等用户敲下第一句话才发现。
func (a *Adapter) Boot(ctx context.Context) error { return a.inner.Start(ctx) }

// Shutdown 关闭 sidecar(装配方把它挂在会话作用域上)。
func (a *Adapter) Shutdown(ctx context.Context) error { return a.inner.Shutdown(ctx) }

// EngineName 实现 engine.Named。
func (a *Adapter) EngineName() string { return Name }

// Supports 声明能力。
func (a *Adapter) Supports(c engine.Capability) bool { return caps.Supports(c) }

// Capabilities 返回能力集合(诊断用)。
func (a *Adapter) Capabilities() engine.Set { return caps }

// Start 把这一轮的文本入队给 dsh,立刻返回句柄。
func (a *Adapter) Start(ctx context.Context, req engine.TurnRequest) (engine.TurnHandle, error) {
	// 终结过的 sidecar 不能再开新一轮 —— dsh 没有 mid-turn 取消,取消就是杀进程,
	// 而这条 stdio 连接承载的会话随之消失。这里明确拒绝,而不是让下一轮写进一个
	// 死掉的 LineClient 然后静默挂住(AR-R04)。调用方应当重建这个会话。
	if dead, cause := a.inner.Dead(); dead {
		return nil, cause
	}
	// 网关 token 刷新之后,这个进程还拿着启动时烤进环境的那一份 —— 协议里没有换
	// token 的方法,所以走复核给的第二条路:标成终结,由调用方重建(AR-R06 后半)。
	// 放在 Dead 检查之后:已经终结的会话该报它自己的原因,不该被这条盖掉。
	if a.inner.tokenChanged(ctx) {
		a.inner.markDead(errTokenRefreshed)
		return nil, errTokenRefreshed
	}
	t := &turn{inner: a.inner, done: make(chan struct{})}
	a.mu.Lock()
	prev := a.cur
	a.cur = t
	a.mu.Unlock()
	if prev != nil {
		// 上一轮还挂着(sidecar 没推 turn/end 就被换掉了):放它走,别让等它的人
		// 永远卡住。回合互斥在 Controller 那侧,这里只是不制造泄漏。
		prev.finish(errors.New("dsh 引擎:上一轮被新的一轮取代"))
	}
	if _, err := a.inner.Submit(ctx, a.sessionID, req.Input); err != nil {
		a.clear(t)
		return nil, fmt.Errorf("dsh 引擎:入队失败: %w", err)
	}
	return t, nil
}

// finishTurn 是 sidecar 推来 turn/end 时的回调。
func (a *Adapter) finishTurn() {
	a.mu.Lock()
	t := a.cur
	a.cur = nil
	a.mu.Unlock()
	if t != nil {
		t.finish(nil)
	}
}

// clear 只在 t 仍是当前一轮时清掉它,避免误伤后来的一轮。
func (a *Adapter) clear(t *turn) {
	a.mu.Lock()
	if a.cur == t {
		a.cur = nil
	}
	a.mu.Unlock()
}

// turn 是 dsh 一轮的句柄。
type turn struct {
	inner *Engine
	once  sync.Once
	done  chan struct{}
	// err 只在 once 里写,读方必须先 <-done。
	err error
}

// finish 结束这一轮(幂等)。
func (t *turn) finish(err error) {
	t.once.Do(func() {
		t.err = err
		close(t.done)
	})
}

// Cancel 中止这一轮(幂等;已结束则是空操作)。
//
// dsh 的 JSON-RPC 没有 mid-turn 取消方法,所以取消降级为关掉 sidecar 进程 ——
// 这是 spike 报告里就写明的已知取舍,不是这里新引入的。
func (t *turn) Cancel() error {
	select {
	case <-t.done:
		return nil // 已经结束了:别去杀一个正常收工的 sidecar
	default:
	}
	err := t.inner.Cancel()
	t.finish(context.Canceled)
	return err
}

// Wait 等这一轮结束。
//
// 三个出口都必须有,少一个就是一种挂死:turn/end 是正常收工;子进程半路死掉后
// dsh 永远不会再推 turn/end;ctx 取消则要先真的把 sidecar 停下来再返回 —— 光返回
// 不停进程,它会继续往 sink 里写上一轮的事件。
func (t *turn) Wait(ctx context.Context) error {
	select {
	case <-t.done:
		return t.err
	case err := <-t.inner.Died():
		// 读循环终止 = 子进程没了。标记终结,后续的 Start 会明确拒绝而不是挂住。
		died := fmt.Errorf("dsh 引擎:sidecar 中途退出: %w", err)
		t.inner.markDead(died)
		t.finish(died)
		<-t.done
		return t.err
	case <-ctx.Done():
		_ = t.Cancel()
		return ctx.Err()
	}
}

// dropTurnDone 吞掉引擎侧的 TurnDone,其余原样放行。见 NewAdapter 的说明。
type dropTurnDone struct{ inner event.Sink }

func (d dropTurnDone) Emit(e event.Event) {
	if e.Kind == event.TurnDone {
		return
	}
	d.inner.Emit(e)
}
