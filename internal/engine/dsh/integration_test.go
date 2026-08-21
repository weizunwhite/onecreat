package dsh

// 假 sidecar 集成测试(复核对 dsh 的验收:「不允许只编译 adapter」)。
//
// 前面那些用例用手搓的结构体驱动 turn / Adapter —— 那只能测到我**以为**协议长什么样。
// 这里真的拉起一个进程,走完 stdio JSON-RPC 的 initialize → session/prompt → 通知流 →
// turn/end,再优雅关闭。接线本身、事件映射、终结语义,都在这条路径上。

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/engine"
	"reasonix/internal/event"
)

// recorder 收下适配器发出的事件。
type recorder struct {
	mu   sync.Mutex
	seen []event.Event
}

func (r *recorder) Emit(e event.Event) {
	r.mu.Lock()
	r.seen = append(r.seen, e)
	r.mu.Unlock()
}

func (r *recorder) kinds() []event.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Kind, len(r.seen))
	for i, e := range r.seen {
		out[i] = e.Kind
	}
	return out
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.seen {
		if e.Kind == event.Text || e.Kind == event.Message {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

// bootFakeAdapter 拉起一个真实的假 sidecar 子进程。
func bootFakeAdapter(t *testing.T) (*Adapter, *recorder) {
	t.Helper()
	rec := &recorder{}
	a, err := NewAdapter(AdapterOptions{Options: Options{
		Cfg:  config.DSHConfig{BinPath: os.Args[0], Args: []string{fakeSidecarFlag}},
		CWD:  t.TempDir(),
		Sink: rec,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Boot(ctx); err != nil {
		t.Fatalf("Boot(拉起假 sidecar): %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = a.Shutdown(sctx)
	})
	return a, rec
}

// 一整轮走通:握手、入队、事件流到 sink、turn/end 让 Wait 返回。
func TestFakeSidecarDrivesAFullTurn(t *testing.T) {
	a, rec := bootFakeAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := a.Start(ctx, engine.TurnRequest{Input: "在吗"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait:%v —— turn/end 应该让这一轮正常收工", err)
	}

	if got := rec.text(); !strings.Contains(got, "你好") {
		t.Errorf("assistant 文本没到 sink,拿到 %q", got)
	}
	kinds := rec.kinds()
	var sawStart, sawDone bool
	for _, k := range kinds {
		switch k {
		case event.TurnStarted:
			sawStart = true
		case event.TurnDone:
			sawDone = true
		}
	}
	if !sawStart {
		t.Errorf("turn/start 应映射成 TurnStarted,拿到 %v", kinds)
	}
	// 引擎侧的 TurnDone 必须被吞掉 —— Controller 的回合状态机会自己发一个,
	// 两个就是两个真源,UI 会看到一轮结束两次。
	if sawDone {
		t.Errorf("引擎侧的 TurnDone 应被吞掉,却出现在 sink 里:%v", kinds)
	}
}

// 连着跑两轮:同一个 sidecar 复用,句柄不串。
func TestFakeSidecarHandlesConsecutiveTurns(t *testing.T) {
	a, rec := bootFakeAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for i := 0; i < 2; i++ {
		h, err := a.Start(ctx, engine.TurnRequest{Input: "第几轮"})
		if err != nil {
			t.Fatalf("第 %d 轮 Start: %v", i, err)
		}
		if err := h.Wait(ctx); err != nil {
			t.Fatalf("第 %d 轮 Wait: %v", i, err)
		}
	}
	turns := 0
	for _, k := range rec.kinds() {
		if k == event.TurnStarted {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("应当跑了 2 轮,数到 %d —— 同一个 sidecar 复用时句柄不该串", turns)
	}
}

// 取消一轮会杀掉 sidecar(dsh 没有 mid-turn cancel)。关键断言是**之后**的行为:
// 必须明确拒绝下一轮,而不是写进一个死掉的连接然后静默挂住(AR-R04)。
func TestFakeSidecarCancelIsTerminal(t *testing.T) {
	a, _ := bootFakeAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 用一轮**不会自己结束**的:假 sidecar 回得太快,不这样的话 Cancel 到达时
	// 那一轮早就正常收工了,而"已收工就不该杀进程"恰恰是正确行为。
	h, err := a.Start(ctx, engine.TurnRequest{Input: "慢慢想 " + hangSentinel})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if dead, _ := a.inner.Dead(); !dead {
		t.Fatal("取消一轮杀掉了进程,引擎必须被标记为终结")
	}
	_, err = a.Start(ctx, engine.TurnRequest{Input: "还在吗"})
	if err == nil {
		t.Fatal("终结之后不该再接受新一轮 —— 那会写进死掉的连接然后静默挂住")
	}
	// 断言必须够强:光有"报了个错"不算。去掉终结检查后,写进死管道也会因为
	// broken pipe 报错,测试照样绿 —— 那就分不出「我们主动拒绝了」和「碰巧失败了」。
	// 而 AR-R04 要防的正是**不报错的那一种**:连接还在、请求石沉大海、UI 显示就绪。
	// 所以这里要求错误就是那条终结原因。
	if !strings.Contains(err.Error(), "无法继续") {
		t.Fatalf("应当是明确的终结拒绝,而不是碰巧写失败;拿到:%v", err)
	}
}

// 装配根仍然拒绝启用它:这条集成测试证明适配器能用,**不**代表 dsh 被放行(AR-R01)。
func TestWorkingAdapterStillDoesNotMakeDSHUsable(t *testing.T) {
	if engine.Supports(&Adapter{}, engine.CapHostedTools) {
		t.Fatal("dsh 不该声明 hosted-tools —— 它一旦声明,boot.selectEngine 就会放行")
	}
}
