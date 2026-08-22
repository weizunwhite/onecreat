package native

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/engine"
)

type fnRunner func(context.Context, string) error

func (f fnRunner) Run(ctx context.Context, s string) error { return f(ctx, s) }

func TestRunnerGetsTheTurnInput(t *testing.T) {
	var got string
	e := New(fnRunner(func(_ context.Context, s string) error { got = s; return nil }))
	h, err := e.Start(context.Background(), engine.TurnRequest{Input: "写个 hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "写个 hello" {
		t.Fatalf("runner 应收到这一轮的输入,拿到 %q", got)
	}
}

func TestRunnerErrorSurfacesFromWait(t *testing.T) {
	boom := errors.New("模型挂了")
	e := New(fnRunner(func(context.Context, string) error { return boom }))
	h, _ := e.Start(context.Background(), engine.TurnRequest{})
	if err := h.Wait(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Wait 应原样返回这一轮的错误,拿到 %v", err)
	}
}

func TestCancelStopsTheRunner(t *testing.T) {
	started := make(chan struct{})
	e := New(fnRunner(func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	h, _ := e.Start(context.Background(), engine.TurnRequest{})
	<-started
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("被取消的一轮应报 context.Canceled,拿到 %v", err)
	}
	if err := h.Cancel(); err != nil { // 幂等
		t.Fatalf("重复 Cancel 应无害:%v", err)
	}
}

// 这一条是内核红线的守卫:Controller 在 Wait 返回之后立刻去读 `agent.Session`
// (Stop hook 要最后一条 assistant 文本、收尾要落盘)。Wait 只要早于运行 goroutine
// 退出一步返回,那就是「单写者 + 同 goroutine 免锁读」被破坏 —— 一个 -race 未必
// 每次都抓得到、但线上会真的踩到的竞态。
func TestWaitNeverReturnsWhileTheRunnerIsStillRunning(t *testing.T) {
	var inside atomic.Bool
	release := make(chan struct{})
	e := New(fnRunner(func(ctx context.Context, _ string) error {
		inside.Store(true)
		select {
		case <-release:
		case <-ctx.Done():
		}
		inside.Store(false)
		return nil
	}))
	// ctx 立刻取消:即便如此,Wait 也必须等到 runner 真的退出。
	ctx, cancel := context.WithCancel(context.Background())
	h, _ := e.Start(ctx, engine.TurnRequest{})
	for !inside.Load() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if inside.Load() {
		t.Fatal("Wait 返回时 runner 还在跑 —— 这会让 Controller 与 agent 同时碰 Session")
	}
	close(release)
}
