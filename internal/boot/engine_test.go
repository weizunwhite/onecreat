package boot

// Plan 12:`engine = "dsh"` 在此之前是个**不生效**的配置项 —— cfg.Engine 除了被渲染
// 进 TOML 之外没有任何消费者,用户写了它,程序静默跑内置内核。这几条用例锁住「它现在
// 真的会选引擎」,以及「选不出来时大声失败,而不是偷偷退回 native」。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/engine"
	"reasonix/internal/engine/dsh"
	"reasonix/internal/engine/native"
)

func specWith(engineName string, dshCfg config.DSHConfig) engineSpec {
	return engineSpec{
		Cfg:    &config.Config{Engine: engineName, DSH: dshCfg},
		Name:   engineName,
		Runner: nil, // native 适配器不解引用它,这些用例只看选出了谁
	}
}

func TestDefaultAndNativeSelectTheBuiltInKernel(t *testing.T) {
	for _, name := range []string{"", "native", "NATIVE", "  native  "} {
		got, err := selectEngine(context.Background(), specWith(name, config.DSHConfig{}))
		if err != nil {
			t.Fatalf("engine=%q: %v", name, err)
		}
		if _, ok := got.(*native.Engine); !ok {
			t.Fatalf("engine=%q 应选内置内核,拿到 %T", name, got)
		}
	}
}

// AR-R01 的核心断言,判据版本 2:**门禁时点**决定放不放行,不是执行位置。
//
// 原判据是"工具必须在 OneCreat 进程里执行(hosted-tools)",写于 rc.7 spike 时期 ——
// 当时 dsh 协议确实没有把工具调用委托回来的通道,它的 tool/call 推过来时文件已经
// 写完了。现在 dsh-tools 在派发之前跑 tools/pre-execute waterfall,控制面插件把它
// await 到 Go 侧,权限门 / plan mode / 审批 / 写前检查点全部在工具真的跑起来之前
// 完成(docs/dsh调研/07 §3)。所以 dsh 声明 gated-tools 并放行。
func TestDSHPassesToolGatingBecauseItsToolsAreGated(t *testing.T) {
	if err := requireToolGating(dshProbe{}); err != nil {
		t.Fatalf("dsh 在派发前阻塞等 OneCreat 裁决,应当通过门禁检查:%v", err)
	}
}

// 判据放宽了,但门还在:一个既不 hosted 也不 gated 的引擎照旧被拒,而且错误必须
// 说清为什么 —— 否则用户只会以为是配置写错了。
func TestEngineWithoutToolGatingIsRefused(t *testing.T) {
	err := requireToolGating(ungatedEngine{})
	if err == nil {
		t.Fatal("既不 hosted 也不 gated 的引擎必须被拒绝")
	}
	for _, want := range []string{"hosted-tools", "gated-tools", "权限门", "native"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q,拿到:%v", want, err)
		}
	}
	var ue *engine.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("应当能取出类型化的 UnsupportedError,拿到 %T", err)
	}
	if ue.Capability != engine.CapGatedTools {
		t.Errorf("缺的能力应是 gated-tools,拿到 %q", ue.Capability)
	}
}

// ungatedEngine 只会流式输出,工具怎么跑没人管 —— 正是这道门要挡住的形状。
type ungatedEngine struct{}

func (ungatedEngine) Start(context.Context, engine.TurnRequest) (engine.TurnHandle, error) {
	return nil, nil
}
func (ungatedEngine) EngineName() string { return "ungated" }
func (ungatedEngine) Supports(c engine.Capability) bool {
	return c == engine.CapStreaming
}

// 这道门有意**没有**开关:一个能被一行配置关掉的安全门等于没有门。
// 这条用例锁住"[dsh] 配置里不存在这样的旁路" —— 无论怎么配,dsh 的门禁声明不变,
// 也就没有任何一行配置能让它变成"不经裁决就跑工具"。
func TestDSHHasNoConfigBypass(t *testing.T) {
	for _, c := range []config.DSHConfig{
		{BinPath: "x"},
		{BinPath: "x", Version: "0.1.0-rc.8"},
		{BinPath: "x", GatewayBaseURL: "https://example.invalid", GatewayTokenEnv: "T"},
	} {
		if spec := specWith("dsh", c); spec.Cfg.DSH.BinPath != c.BinPath {
			t.Fatalf("配置没被带进来:%+v", spec.Cfg.DSH)
		}
		if !dsh.Capabilities().Supports(engine.CapGatedTools) {
			t.Fatalf("任何 [dsh] 配置组合都不该让门禁声明消失:%+v", c)
		}
	}
}

func TestSelectedEnginesCarryTheirCapabilities(t *testing.T) {
	n, err := selectEngine(context.Background(), specWith("native", config.DSHConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []engine.Capability{engine.CapFork, engine.CapHostedTools} {
		if !engine.Supports(n, c) {
			t.Errorf("内置内核应支持 %s", c)
		}
	}
	if engine.NameOf(n) != "native" {
		t.Errorf("内置引擎应自报 native,拿到 %q", engine.NameOf(n))
	}
	// dsh 的能力声明是 fail-closed 判断的唯一依据,这里直接断言那一份。
	if dsh.Capabilities().Supports(engine.CapFork) {
		t.Error("dsh 不该声明 fork")
	}
	if dsh.Capabilities().Supports(engine.CapHostedTools) {
		t.Error("dsh 不该声明 hosted-tools —— 它一旦声明,装配根就会放行")
	}
}
