package control

// AR-R02:能力必须被**执行**,不能只是声明。
//
// Plan 12 让引擎自报能力,但会话操作照旧直接重写 native executor 的 Session。对一个
// transcript 不在 OneCreat 这边的引擎,那意味着:本地影子被改了、前端收到"成功"、
// 而引擎那边纹丝不动 —— 用户看到的成功是假的。这几条用例锁住 fail-closed,并且
// 特别检查**状态没有被改到一半**。

import (
	"context"
	"errors"
	"testing"

	"reasonix/internal/engine"
)

// limitedEngine 只支持 streaming —— 也就是 dsh 今天的形状。
type limitedEngine struct{ fakeEngine }

func (*limitedEngine) Supports(c engine.Capability) bool { return c == engine.CapStreaming }
func (*limitedEngine) EngineName() string                { return "fake-limited" }

func newLimitedController(t *testing.T) *Controller {
	t.Helper()
	dir := t.TempDir()
	return New(Options{Engine: &limitedEngine{}, SessionDir: dir, SessionPath: dir + "/s.jsonl"})
}

// unsupportedOps 是所有"会改写会话状态"的入口。少了任何一个,那个入口就是缺口。
func unsupportedOps(c *Controller) map[string]func() error {
	return map[string]func() error{
		"Fork":         func() error { _, err := c.Fork(1); return err },
		"ForkNamed":    func() error { _, err := c.ForkNamed(1, "x"); return err },
		"Branch":       func() error { _, err := c.Branch("x"); return err },
		"SwitchBranch": func() error { _, err := c.SwitchBranch("x"); return err },
		"Rewind":       func() error { return c.Rewind(1, RewindCode) },
		"NewSession":   func() error { return c.NewSession() },
		"Compact":      func() error { return c.Compact(context.Background(), "") },
	}
}

func TestUnsupportedSessionOpsAreRefused(t *testing.T) {
	c := newLimitedController(t)
	for name, op := range unsupportedOps(c) {
		err := op()
		if err == nil {
			t.Errorf("%s:引擎不支持时必须报错,却成功了 —— 前端会把假成功显示给用户", name)
			continue
		}
		var ue *engine.UnsupportedError
		if !errors.As(err, &ue) {
			t.Errorf("%s:应返回类型化的 UnsupportedError,拿到 %T(%v)", name, err, err)
			continue
		}
		if ue.Engine != "fake-limited" {
			t.Errorf("%s:错误里应带引擎名,拿到 %q", name, ue.Engine)
		}
		if ue.Operation == "" {
			t.Errorf("%s:错误里应说明用户想做的是哪件事", name)
		}
	}
}

// 关键的一半:拒绝必须发生在**碰任何状态之前**。半路失败会留下"影子改了一半、
// 引擎一无所知"的局面,是最难查的一类 bug。
func TestUnsupportedOpsLeaveStateUntouched(t *testing.T) {
	c := newLimitedController(t)
	before := c.SessionPath()
	for name, op := range unsupportedOps(c) {
		_ = op()
		if got := c.SessionPath(); got != before {
			t.Fatalf("%s:被拒绝的操作却动了会话路径 %q → %q", name, before, got)
		}
		// 独占锁必须已经释放:拒绝路径若忘了 EndExclusive,后面每个操作都会被
		// "另一个操作正在进行"挡住,症状与能力无关,极难定位。
		if !c.turn.TryBeginExclusive() {
			t.Fatalf("%s:拒绝后独占状态没有复原", name)
		}
		c.turn.EndExclusive()
	}
}

// 支持的引擎不受影响 —— 这道门只拦不支持的那些。
func TestSupportedEngineIsNotBlocked(t *testing.T) {
	c := New(Options{Runner: &recordingRunner{}})
	if !c.Supports(engine.CapFork) {
		t.Fatal("内置引擎应支持 fork")
	}
	if err := c.requireCap("分叉会话", engine.CapFork); err != nil {
		t.Fatalf("支持的能力不该被拦:%v", err)
	}
	if c.EngineName() != "native" {
		t.Errorf("引擎名应为 native,拿到 %q", c.EngineName())
	}
}

// 会话记录里的 engine 字段必须是**真实**引擎,不能一律记成 native(AR-R03)。
func TestSessionRecordCarriesTheRealEngine(t *testing.T) {
	c := newLimitedController(t)
	rec, ok := c.SessionRecord()
	if !ok {
		t.Fatal("应当登记了会话记录")
	}
	if rec.Engine != "fake-limited" {
		t.Fatalf("会话应记在真实引擎名下,拿到 %q —— 记成 native 会让人以为本地文件里有它的 transcript", rec.Engine)
	}
}
