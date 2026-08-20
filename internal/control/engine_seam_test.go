package control

// Plan 12 / A14:Controller 与「谁来跑这一轮」之间只剩一条缝 —— TurnEngine。
// 这几条用例锁住那条缝的行为,而不是它的实现细节。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"reasonix/internal/engine"
)

// fakeEngine 记录它收到了什么,并让测试决定这一轮怎么结束。
type fakeEngine struct {
	mu       sync.Mutex
	inputs   []string
	handles  []*fakeHandle
	startErr error
}

func (f *fakeEngine) Start(ctx context.Context, req engine.TurnRequest) (engine.TurnHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.inputs = append(f.inputs, req.Input)
	h := &fakeHandle{ctx: ctx, done: make(chan struct{})}
	close(h.done) // 默认:一开始就已经跑完
	f.handles = append(f.handles, h)
	return h, nil
}

func (f *fakeEngine) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inputs...)
}

func (f *fakeEngine) lastHandle() *fakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.handles) == 0 {
		return nil
	}
	return f.handles[len(f.handles)-1]
}

type fakeHandle struct {
	ctx      context.Context
	done     chan struct{}
	err      error
	mu       sync.Mutex
	canceled int
}

func (h *fakeHandle) Cancel() error {
	h.mu.Lock()
	h.canceled++
	h.mu.Unlock()
	return nil
}

func (h *fakeHandle) cancels() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.canceled
}

func (h *fakeHandle) Wait(context.Context) error {
	<-h.done
	return h.err
}

func TestTurnGoesThroughTheEngine(t *testing.T) {
	fe := &fakeEngine{}
	c := New(Options{Engine: fe})
	if err := c.Run(context.Background(), "写个 hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fe.got()
	if len(got) != 1 || got[0] != "写个 hello" {
		t.Fatalf("引擎应收到这一轮的输入,拿到 %v", got)
	}
}

// 每一轮结束都必须 Cancel 句柄。native 下这是空操作,但 dsh 的 sidecar 是独立进程,
// 漏掉这一下就是一个跑到天荒地老的子进程。
func TestFinishedTurnReleasesItsHandle(t *testing.T) {
	fe := &fakeEngine{}
	c := New(Options{Engine: fe})
	if err := c.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if n := fe.lastHandle().cancels(); n != 1 {
		t.Fatalf("这一轮结束后应恰好 Cancel 一次句柄,拿到 %d", n)
	}
}

// 取消的触发源仍然只有 ctx 一个:它必须原样传进引擎,否则 dsh 那种不随 ctx 死掉的
// 引擎就永远收不到「停」。
func TestEngineSeesTheTurnContext(t *testing.T) {
	fe := &fakeEngine{}
	c := New(Options{Engine: fe})
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Run(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	h := fe.lastHandle()
	select {
	case <-h.ctx.Done():
		t.Fatal("回合还没被取消,引擎手里的 ctx 不该已经 Done")
	default:
	}
	cancel()
	select {
	case <-h.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("取消这一轮之后,引擎手里的 ctx 也该随之取消")
	}
}

func TestEngineStartFailureSurfaces(t *testing.T) {
	boom := errors.New("引擎起不来")
	fe := &fakeEngine{startErr: boom}
	c := New(Options{Engine: fe})
	if err := c.Run(context.Background(), "hi"); !errors.Is(err, boom) {
		t.Fatalf("引擎启动失败应原样冒出来,拿到 %v", err)
	}
}

// Options 里两个字段不是两个真源:New 立刻把它们收敛成唯一的 c.engine,Engine 优先。
func TestExplicitEngineWinsOverRunnerShorthand(t *testing.T) {
	fe := &fakeEngine{}
	r := &recordingRunner{}
	c := New(Options{Engine: fe, Runner: r})
	if err := c.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(fe.got()) != 1 {
		t.Fatal("显式传入的 Engine 应该接管这一轮")
	}
	if r.calls != 0 {
		t.Fatalf("Runner 简写不该同时生效,却被调了 %d 次", r.calls)
	}
}

type recordingRunner struct{ calls int }

func (r *recordingRunner) Run(context.Context, string) error { r.calls++; return nil }

// 不传 Engine 时,Runner 简写照旧走内置引擎 —— 既有的全部调用方都靠这条。
func TestRunnerShorthandStillDrivesTurns(t *testing.T) {
	r := &recordingRunner{}
	c := New(Options{Runner: r})
	if err := c.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("Runner 应被调用一次,拿到 %d", r.calls)
	}
}
