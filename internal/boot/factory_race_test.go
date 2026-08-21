package boot

// AR-R11:Factory 是一个工作区的**唯一**账本。
//
// 之前 refcount 在 runtime.Process 那边、services 在 Factory 这边,而 Release 先关 scope、
// 后删 services。中间那一瞬,并发的 OpenWorkspace 会拿到「新的 scope + 已经关掉的
// services」;随后前一个的 delete 又把它正在用的那条记录抹掉。两个注册表就是两个真源。

import (
	"context"
	"sync"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/workspace"
)

func testWS(t *testing.T) workspace.Context {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func lspSpec(root string) WorkspaceSpec {
	cfg := config.Default()
	cfg.LSP.Enabled = true
	return WorkspaceSpec{Config: cfg, Root: root}
}

// 持有期间,scope 绝不能是关闭的 —— 这是"持有"的全部含义。最后释放与重开并发时,
// 原来的实现会把一个刚建好的 scope 配上一套已经拆掉的服务。
func TestConcurrentLastReleaseAndReopenNeverHandsOutAClosedScope(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)
	spec := lspSpec(ws.Root())

	var wg sync.WaitGroup
	for round := 0; round < 200; round++ {
		h := f.OpenWorkspace(ws, spec)
		wg.Add(2)
		go func() { defer wg.Done(); h.Release() }() // 可能是最后一个 → 触发拆除
		go func() {
			defer wg.Done()
			h2 := f.OpenWorkspace(ws, spec) // 与拆除赛跑
			if h2.Scope().Closed() {
				t.Error("拿到了一个已经关闭的 workspace scope —— 持有期间它不该是关的")
			}
			if h2.Services() == nil {
				t.Error("OpenWorkspace 必须给出共享服务")
			}
			h2.Release()
		}()
		wg.Wait()
	}
}

// 全部释放之后再打开,必须拿到**新**的一套服务,而不是复用那份已经拆掉的。
func TestServicesAreRebuiltAfterTheLastHolderLeaves(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)
	spec := lspSpec(ws.Root())

	h1 := f.OpenWorkspace(ws, spec)
	svc1, scope1 := h1.Services(), h1.Scope()
	h1.Release()
	if !scope1.Closed() {
		t.Fatal("最后一个持有者走了,workspace 应当关闭")
	}

	h2 := f.OpenWorkspace(ws, spec)
	defer h2.Release()
	if h2.Services() == svc1 {
		t.Fatal("复用了已经拆掉的那套服务 —— 它的 LSP manager 已经 Close 过了")
	}
	if h2.Scope() == scope1 {
		t.Fatal("复用了已经关闭的 scope")
	}
}

// 两个会话开同一个项目,必须共享同一套服务;这是 Plan 05 的目的,不能被这次改动破坏。
func TestTwoSessionsOnOneProjectStillShare(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)
	spec := lspSpec(ws.Root())

	a := f.OpenWorkspace(ws, spec)
	defer a.Release()
	b := f.OpenWorkspace(ws, spec)
	defer b.Release()

	if a.Services() != b.Services() {
		t.Fatal("同一个项目的两个会话应共享同一套服务")
	}
	if a.Scope() != b.Scope() {
		t.Fatal("同一个项目的两个会话应共享同一个 workspace scope")
	}
	a.Release() // 不是最后一个
	if b.Scope().Closed() {
		t.Fatal("关掉其中一个会话,把另一个还在用的工作区也关了")
	}
}

// Hold 走同一套账本:它必须能挡住拆除,否则它就是绕过账本偷偷复活 scope 的第二条路径。
func TestHoldKeepsTheWorkspaceAliveAcrossARebuild(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)
	spec := lspSpec(ws.Root())

	h := f.OpenWorkspace(ws, spec)
	svc, scope := h.Services(), h.Scope()

	hold := f.Hold(ws) // 桌面重建标签时的做法:先握住,再换会话
	h.Release()        // 旧会话走了,但 hold 还在

	if scope.Closed() {
		t.Fatal("Hold 期间工作区被关掉了 —— 重建标签会把项目的语言服务器停掉再立刻重启")
	}
	again := f.OpenWorkspace(ws, spec)
	if again.Services() != svc {
		t.Fatal("Hold 跨越重建之后,新会话拿到的应当还是原来那套服务")
	}
	again.Release()
	hold.Release()
	if !scope.Closed() {
		t.Fatal("最后一个持有者(hold)释放后,工作区应当关闭")
	}
}

// Hold 先于任何 OpenWorkspace 时,服务要能被后来的会话补上 —— 而不是永远是空的。
func TestServicesStartLazilyWhenHoldCameFirst(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)

	hold := f.Hold(ws)
	defer hold.Release()
	if hold.Services() != nil {
		t.Fatal("Hold 只占生命周期,不该带服务")
	}
	h := f.OpenWorkspace(ws, lspSpec(ws.Root()))
	defer h.Release()
	if h.Services() == nil {
		t.Fatal("Hold 在先时,后来的会话仍然必须拿到共享服务")
	}
}

// 并发开关同一个项目,-race 下不得有数据竞争,且账本最终必须归零。
func TestFactoryLedgerIsConsistentUnderLoad(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)
	spec := lspSpec(ws.Root())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 60; j++ {
				h := f.OpenWorkspace(ws, spec)
				if h.Scope().Closed() {
					t.Error("持有期间 scope 是关闭的")
				}
				h.Release()
			}
		}()
	}
	wg.Wait()

	f.mu.Lock()
	n := len(f.entries)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("全部释放后账本应为空,还剩 %d 条 —— 记录泄漏意味着服务也没被拆", n)
	}
}

// 配置代际(AR-R11 的另一半):共享服务正被别人用着时,新会话带来的不同配置不会
// 就地生效。原来这是**静默**的 —— 用户改了 LSP / CodeGraph 设置却不知道为什么没反应。
// 现在指纹不同会记一条 warn,而服务保持不变(不能在别人用着的时候换掉它)。
func TestLiveConfigChangeDoesNotSwapSharedServicesUnderneath(t *testing.T) {
	f := NewFactory(context.Background())
	defer f.Close()
	ws := testWS(t)

	first := lspSpec(ws.Root())
	h1 := f.OpenWorkspace(ws, first)
	defer h1.Release()
	svc := h1.Services()

	// 第二个会话带着不同的配置来开同一个项目。
	second := lspSpec(ws.Root())
	second.Config.Codegraph.Enabled = !second.Config.Codegraph.Enabled
	if specFingerprint(first) == specFingerprint(second) {
		t.Fatal("指纹必须能区分出这次配置变化,否则警告永远不会触发")
	}
	h2 := f.OpenWorkspace(ws, second)
	defer h2.Release()

	if h2.Services() != svc {
		t.Fatal("不能在别的会话正用着的时候把共享服务换掉")
	}
}

// 指纹只概括**会影响共享服务**的那几项:别的字段变了不该制造假警报。
func TestFingerprintIgnoresSettingsThatDoNotAffectSharedServices(t *testing.T) {
	ws := testWS(t)
	a := lspSpec(ws.Root())
	b := lspSpec(ws.Root())
	b.Config.Agent.Temperature = a.Config.Agent.Temperature + 0.3
	b.Config.UI.Theme = "dark"
	if specFingerprint(a) != specFingerprint(b) {
		t.Fatal("与共享服务无关的设置不该改变指纹 —— 那会让用户看到莫名其妙的警告")
	}
}
