package dsh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/engine"
	"reasonix/internal/event"
)

type capture struct{ kinds []event.Kind }

func (c *capture) Emit(e event.Event) { c.kinds = append(c.kinds, e.Kind) }

// dsh 自己会把 turn/end 映射成 TurnDone,而 Controller 的回合状态机收尾时也会发一个。
// 两个 TurnDone 就是两个真源,UI 会看到一轮结束两次 —— 引擎侧那条必须在进 sink 之前
// 就被吞掉。
func TestEngineSideTurnDoneIsSwallowed(t *testing.T) {
	c := &capture{}
	s := dropTurnDone{inner: c}
	s.Emit(event.Event{Kind: event.TurnStarted})
	s.Emit(event.Event{Kind: event.Text, Text: "hi"})
	s.Emit(event.Event{Kind: event.TurnDone})
	s.Emit(event.Event{Kind: event.Notice})
	want := []event.Kind{event.TurnStarted, event.Text, event.Notice}
	if len(c.kinds) != len(want) {
		t.Fatalf("放行的事件数不对:%v", c.kinds)
	}
	for i, k := range want {
		if c.kinds[i] != k {
			t.Fatalf("第 %d 条应是 %v,拿到 %v", i, k, c.kinds[i])
		}
	}
}

// newIdleAdapter 造一个不拉子进程的适配器(Boot 没被调用,rpc 为 nil)。
func newIdleAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewAdapter(AdapterOptions{Options: Options{
		Cfg: config.DSHConfig{BinPath: "not-launched"},
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return a
}

func TestTurnEndUnblocksWait(t *testing.T) {
	a := newIdleAdapter(t)
	tn := &turn{inner: a.inner, done: make(chan struct{})}
	a.cur = tn
	go func() { time.Sleep(10 * time.Millisecond); a.finishTurn() }()
	if err := tn.Wait(context.Background()); err != nil {
		t.Fatalf("正常收工的一轮不该带错误:%v", err)
	}
	if a.cur != nil {
		t.Error("收工后当前一轮应被清掉,否则下一轮的 turn/end 会落到旧句柄上")
	}
}

// 正常跑完之后再 Cancel 必须是空操作 —— Controller 每轮结束都会 defer 一次 Cancel,
// 要是它照样去杀进程,每说一句话就会重启一次 sidecar。
func TestCancelAfterCompletionDoesNotKill(t *testing.T) {
	a := newIdleAdapter(t)
	tn := &turn{inner: a.inner, done: make(chan struct{})}
	tn.finish(nil)
	if err := tn.Cancel(); err != nil {
		t.Fatalf("已结束的一轮 Cancel 应无害:%v", err)
	}
	if err := tn.Wait(context.Background()); err != nil {
		t.Fatalf("Cancel 不该把一个已经成功的一轮改成失败:%v", err)
	}
}

func TestCancelBeforeCompletionEndsTheTurn(t *testing.T) {
	a := newIdleAdapter(t)
	tn := &turn{inner: a.inner, done: make(chan struct{})}
	if err := tn.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := tn.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("被取消的一轮应报 context.Canceled,拿到 %v", err)
	}
	if err := tn.Cancel(); err != nil { // 幂等
		t.Fatalf("重复 Cancel 应无害:%v", err)
	}
}

// ctx 取消时 Wait 必须先真的把 sidecar 停下来再返回:光返回不停进程,它会继续往
// sink 里写上一轮的事件。
func TestWaitHonorsContextCancellation(t *testing.T) {
	a := newIdleAdapter(t)
	tn := &turn{inner: a.inner, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tn.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 ctx 的错误,拿到 %v", err)
	}
	select {
	case <-tn.done:
	default:
		t.Error("Wait 因 ctx 返回前必须已经把这一轮结束掉")
	}
}

// 上一轮还挂着就被新的一轮取代时,旧句柄必须被放掉 —— 否则等它的 goroutine 永远
// 醒不过来。
func TestSupersededTurnIsReleased(t *testing.T) {
	a := newIdleAdapter(t)
	old := &turn{inner: a.inner, done: make(chan struct{})}
	a.cur = old
	// Submit 会失败(没启动),但那发生在取代之后 —— 正是要覆盖的时序。
	_, _ = a.Start(context.Background(), engine.TurnRequest{Input: "hi"})
	select {
	case <-old.done:
	case <-time.After(time.Second):
		t.Fatal("被取代的一轮应立刻结束,不能让等它的人永远挂住")
	}
}

// —— AR-R04:取消之后,这个 sidecar 就是死的,必须说出来 ——

// 之前:Cancel 杀掉子进程,但 Engine.started 已置位、无法重新 Boot,适配器却照旧
// 接受下一轮并写进那个死掉的 LineClient —— 请求石沉大海,UI 还显示就绪。
func TestTurnAfterCancelIsRefusedNotSilentlyHung(t *testing.T) {
	a := newIdleAdapter(t)
	tn := &turn{inner: a.inner, done: make(chan struct{})}
	a.cur = tn
	if err := tn.Cancel(); err != nil {
		t.Fatal(err)
	}
	if dead, _ := a.inner.Dead(); !dead {
		t.Fatal("取消一轮杀掉了进程,引擎必须被标记为终结")
	}
	h, err := a.Start(context.Background(), engine.TurnRequest{Input: "下一句"})
	if err == nil {
		t.Fatal("终结之后不该再接受新一轮 —— 那会写进一个死掉的连接然后静默挂住")
	}
	if h != nil {
		t.Error("拒绝时不该返回句柄")
	}
	if !strings.Contains(err.Error(), "无法继续") {
		t.Errorf("错误应说明该会话无法继续,拿到:%v", err)
	}
}

// sidecar 自己死掉(不是我们杀的)也走同一条路。
func TestSidecarDeathIsTerminalToo(t *testing.T) {
	a := newIdleAdapter(t)
	a.inner.markDead(errors.New("boom"))
	if _, err := a.Start(context.Background(), engine.TurnRequest{}); err == nil {
		t.Fatal("sidecar 已退出时不该接受新一轮")
	}
	if _, err := a.inner.Submit(context.Background(), "s", "hi"); err == nil {
		t.Fatal("Submit 也必须拒绝,而不是写进死连接")
	}
}

// 终结原因只记第一个:先被取消、随后读循环报 EOF,用户该看到的是"你取消了",
// 不是"sidecar 莫名退出"。
func TestTerminalCauseIsStable(t *testing.T) {
	a := newIdleAdapter(t)
	a.inner.markDead(errors.New("第一个原因"))
	a.inner.markDead(errors.New("第二个原因"))
	_, cause := a.inner.Dead()
	if !strings.Contains(cause.Error(), "第一个") {
		t.Fatalf("应保留最早的终结原因,拿到:%v", cause)
	}
}

// —— AR-R06:子进程只拿到白名单里的环境变量 ——

// 之前 childEnv 从 os.Environ() 起步,把本进程的全部秘密交给一个第三方子进程:
// 其它 provider 的 key、项目 .env、CI 注入的凭据。dsh 一个都用不上。
func TestChildEnvDoesNotLeakHostSecrets(t *testing.T) {
	t.Setenv("SOME_OTHER_PROVIDER_API_KEY", "sk-should-not-leak")
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "gw-should-not-leak")
	// 值本身无所谓,断言的是「白名单把它透传了」;用一个中性值,免得在 Windows 上
	// 留下一个我无法实跑验证的平台假设。
	t.Setenv("PATH", "passthrough-marker")

	e, err := New(Options{
		Cfg:          config.DSHConfig{BinPath: "x", GatewayBaseURL: "https://gw.invalid"},
		CWD:          t.TempDir(),
		GatewayToken: "the-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := e.childEnv()
	joined := strings.Join(env, "\n")
	for _, leak := range []string{"sk-should-not-leak", "gw-should-not-leak"} {
		if strings.Contains(joined, leak) {
			t.Errorf("宿主秘密泄漏进 sidecar 环境:%q", leak)
		}
	}
	// 它真正需要的那些必须还在。
	for _, want := range []string{"PATH=passthrough-marker", "DEEPSEEK_API_KEY=the-token", "DEEPSEEK_BASE_URL=https://gw.invalid"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sidecar 缺少必要的环境项 %q", want)
		}
	}
}
