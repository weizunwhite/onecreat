package dsh

// AR-R06 后半:网关 token 刷新之后,一个**已经在跑**的 sidecar 怎么办。
//
// token 在 Boot 时被烤进子进程环境(childEnv 里的 DEEPSEEK_API_KEY)。进程一旦起来,
// 那份环境就冻住了 —— 刷新更新的是 Gateway 对象,原生 provider 下一次请求就能拿到新的,
// 而这个 sidecar 永远拿着旧的。dsh 协议里也没有「换个 token」的方法(见 §22)。
//
// 复核给的是两条路:「必须能到达运行中的 dsh,**或**触发受控滚动重启」。第一条在这个
// 协议上不存在通道,所以走第二条。
//
// 不修的话失败形态是最糟的那种:sidecar 拿着过期 token 去打网关,用户看到的是一串没有
// 解释的鉴权错误,而真正的原因(token 换过了、这个进程没跟上)一个字都没有。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/engine"
)

// 起一个假 sidecar,但 token 由一个可变的源提供。
func bootAdapterWithToken(t *testing.T, tok *staticToken) *Adapter {
	t.Helper()
	opts := Options{
		Cfg:  config.DSHConfig{BinPath: os.Args[0], Args: []string{fakeSidecarFlag}},
		CWD:  t.TempDir(),
		Sink: &recorder{},
	}
	if tok != nil { // nil 时保持"没有凭据源",避免塞进一个类型化的 nil 接口
		opts.Credentials = tok
	}
	a, err := NewAdapter(AdapterOptions{Options: opts})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Boot(ctx); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = a.Shutdown(sctx)
	})
	return a
}

type staticToken struct{ v string }

func (s *staticToken) Token(context.Context) (string, error) { return s.v, nil }

// token 变了 → 下一轮必须被明确拒绝,并说清原因,而不是拿着过期凭据继续跑。
func TestTokenRefreshTerminatesTheRunningSidecar(t *testing.T) {
	tok := &staticToken{v: "token-v1"}
	a := bootAdapterWithToken(t, tok)

	tok.v = "token-v2" // 网关刷新

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := a.Start(ctx, engine.TurnRequest{Input: "你好"})
	if err == nil {
		t.Fatal("token 刷新后,这个 sidecar 还拿着旧凭据,下一轮必须被拒绝")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("拒绝原因必须说清是 token 刷新导致的,拿到:%v", err)
	}
	if dead, _ := a.inner.Dead(); !dead {
		t.Fatal("必须把引擎标成终结,否则下一轮又会写进这个拿着旧 token 的进程")
	}
}

// token 没变时,行为一个字节都不变 —— 不能因为加了这条检查就让正常会话变脆。
func TestUnchangedTokenDoesNotDisturbTheSession(t *testing.T) {
	tok := &staticToken{v: "token-v1"}
	a := bootAdapterWithToken(t, tok)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := a.Start(ctx, engine.TurnRequest{Input: "你好"})
	if err != nil {
		t.Fatalf("token 没变,这一轮不该被拒绝:%v", err)
	}
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// 没配 Credentials 的调用方(今天全部的测试路径)行为不变。
func TestNoCredentialSourceKeepsOldBehaviour(t *testing.T) {
	a, _ := bootFakeAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Start(ctx, engine.TurnRequest{Input: "你好"}); err != nil {
		t.Fatalf("没有凭据源时不该有任何新的失败模式:%v", err)
	}
}

// 凭据源临时取不到 token(网络抖动、刷新正在进行)不该把一个正常会话打死。
//
// 这条是我自己在注释里写下的承诺,但第一版没有用例锁住它 —— 反向验证时去掉 err 判断,
// 测试照样绿。把一次抖动变成「会话没了」,比拿着旧 token 多跑一轮糟得多:后者会在网关
// 那侧以 401 收场,前者直接毁掉用户正在做的事。
type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) {
	return "", errors.New("凭据源暂时不可用")
}

func TestCredentialErrorDoesNotKillTheSession(t *testing.T) {
	// 必须**带着** token 启动:烤进子进程的那份为空时,本来就没有"过期"可言,
	// 走不到这条判断 —— 第一版就是这么写的,于是反向验证时它不会红。
	a := bootAdapterWithToken(t, &staticToken{v: "token-v1"})
	a.inner.opts.Credentials = failingToken{}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Start(ctx, engine.TurnRequest{Input: "你好"}); err != nil {
		t.Fatalf("凭据源报错不该终结会话:%v", err)
	}
	if dead, cause := a.inner.Dead(); dead {
		t.Fatalf("会话被打死了,原因:%v", cause)
	}
}
