package control

// AR-R12:回合真正接进 runtime scope。
//
// 「四级 Runtime Scope」在 Plan 03 建好、Plan 05 把 workspace/session 接上,但 Turn 一直
// 只是文档骨架:`Guarded` 从 `context.Background()` 派生回合 context,于是关闭会话并不会
// 取消在途的那一轮 —— 得靠 Controller 记得手工 Cancel。少记一次,就是一个还在跑的
// goroutine 抓着一个已经关掉的会话。

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/runtime"
	"reasonix/internal/workspace"
)

// newScopedTurnState 造一个接了 runtime.Session 的 turnState。
func newScopedTurnState(t *testing.T) (*turnState, *runtime.Session, *runtime.Process) {
	t.Helper()
	proc := runtime.NewProcess(context.Background())
	t.Cleanup(proc.Close)
	sess := proc.OpenWorkspace(newWS(t)).NewSession("")
	return newTurnState(event.Discard, nil, sess), sess, proc
}

// 关掉会话必须把在途的那一轮一起取消 —— 这正是接线的理由。
func TestClosingTheSessionCancelsTheTurnInFlight(t *testing.T) {
	ts, sess, _ := newScopedTurnState(t)
	started := make(chan context.Context, 1)
	ts.Guarded(func(ctx context.Context) error {
		started <- ctx
		<-ctx.Done()
		return ctx.Err()
	})
	ctx := <-started
	select {
	case <-ctx.Done():
		t.Fatal("回合刚开始,不该已经取消")
	default:
	}
	sess.Close()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("关闭会话没有取消在途的回合 —— 那一轮会抓着一个已经关掉的会话继续跑")
	}
}

// 取消一轮不得连累会话:下一轮还得能开起来。
func TestCancellingATurnLeavesTheSessionUsable(t *testing.T) {
	ts, sess, _ := newScopedTurnState(t)
	first := make(chan context.Context, 1)
	ts.Guarded(func(ctx context.Context) error { first <- ctx; <-ctx.Done(); return nil })
	c1 := <-first
	ts.Cancel()
	select {
	case <-c1.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel 没有取消这一轮")
	}
	if sess.Closed() {
		t.Fatal("取消一轮把会话也关掉了 —— Turn 的取消从不该碰它的 session")
	}
	// 等第一轮真正退出(Guarded 在 body 返回后才清 running)。
	waitIdle(t, ts)

	second := make(chan struct{})
	ts.Guarded(func(context.Context) error { close(second); return nil })
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("取消一轮之后,下一轮开不起来了")
	}
}

// 兄弟会话互不影响。
func TestCancellingOneSessionsTurnLeavesSiblingsAlone(t *testing.T) {
	proc := runtime.NewProcess(context.Background())
	defer proc.Close()
	ws := proc.OpenWorkspace(newWS(t))
	a, b := ws.NewSession("a"), ws.NewSession("b")
	tsA, tsB := newTurnState(event.Discard, nil, a), newTurnState(event.Discard, nil, b)

	ca, cb := make(chan context.Context, 1), make(chan context.Context, 1)
	tsA.Guarded(func(ctx context.Context) error { ca <- ctx; <-ctx.Done(); return nil })
	tsB.Guarded(func(ctx context.Context) error { cb <- ctx; <-ctx.Done(); return nil })
	ctxA, ctxB := <-ca, <-cb

	a.Close()
	select {
	case <-ctxA.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("关闭 a 没有取消 a 的回合")
	}
	select {
	case <-ctxB.Done():
		t.Fatal("关闭 a 把兄弟会话 b 的回合也取消了")
	case <-time.After(100 * time.Millisecond):
	}
	b.Close()
}

// 没接 scope 的 Controller(CLI / 大量既有测试)行为不变。
func TestUnscopedTurnStillRuns(t *testing.T) {
	ts := newTurnState(event.Discard, nil, nil)
	done := make(chan struct{})
	ts.Guarded(func(ctx context.Context) error {
		if ctx == nil {
			t.Error("回合 context 不该为 nil")
		}
		close(done)
		return nil
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("没接 scope 时回合也必须能跑")
	}
}

// 守卫:接了 scope 就不许再拿 context.Background() 当回合的根。
// 这条容易在后续改动里被悄悄改回去 —— 改回去之后所有行为看起来都正常,只有
// 「关会话不取消回合」这一个症状,而它极难被注意到。
func TestTurnRootIsTheScopeNotBackground(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "turn_state.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var inGuarded bool
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok {
			inGuarded = fd.Name.Name == "Guarded"
			return true
		}
		if !inGuarded {
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "context" && sel.Sel.Name == "Background" {
				bad = append(bad, "Guarded 里直接用了 context.Background()")
			}
		}
		return true
	})
	for _, b := range bad {
		t.Errorf("%s —— 回合的根必须来自 beginTurn(接了 scope 时是 runtime.Turn)", b)
	}
}

// newWS 造一个指向临时目录的 workspace.Context。
func newWS(t *testing.T) workspace.Context {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// waitIdle 等 turnState 回到空闲。
func waitIdle(t *testing.T, ts *turnState) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !ts.Running() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("回合迟迟没有结束")
}
