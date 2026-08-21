package boot

// Plan 12:`engine = "dsh"` 在此之前是个**不生效**的配置项 —— cfg.Engine 除了被渲染
// 进 TOML 之外没有任何消费者,用户写了它,程序静默跑内置内核。这几条用例锁住「它现在
// 真的会选引擎」,以及「选不出来时大声失败,而不是偷偷退回 native」。

import (
	"context"
	"errors"
	"path/filepath"
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

// AR-R01 的核心断言:engine="dsh" 必须 fail-closed。
//
// 它的工具跑在自己的进程里,等 tool/call 推过来时文件已经写完、shell 已经跑过 ——
// 权限门 / plan mode / PreToolUse hook / 写前检查点 / 证据链全部落空。用户以为
// OneCreat 在替他把关,实际上没有。所以装配直接失败,而不是"能力少一条"。
func TestDSHIsRefusedBecauseItsToolsBypassPolicy(t *testing.T) {
	got, err := selectEngine(context.Background(), specWith("dsh", config.DSHConfig{
		BinPath: filepath.Join(t.TempDir(), "dsh"), // 配置齐全也照样拒绝
	}))
	if err == nil {
		t.Fatalf("engine=\"dsh\" 必须被拒绝,却拿到了 %T", got)
	}
	if got != nil {
		t.Errorf("拒绝时不该返回引擎,拿到 %T", got)
	}
	// 错误必须点出**为什么**,否则用户只会以为是配置写错了。
	for _, want := range []string{"hosted-tools", "权限门", "native"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q,拿到:%v", want, err)
		}
	}
	var ue *engine.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("应当能取出类型化的 UnsupportedError,拿到 %T", err)
	}
	if ue.Capability != engine.CapHostedTools {
		t.Errorf("缺的能力应是 hosted-tools,拿到 %q", ue.Capability)
	}
}

// 这道门有意**没有**开关:一个能被一行配置关掉的安全门等于没有门。
// 这条用例锁住"配置里不存在这样的旁路"。
func TestDSHHasNoConfigBypass(t *testing.T) {
	for _, c := range []config.DSHConfig{
		{BinPath: "x"},
		{BinPath: "x", Version: "0.1.0-rc.7"},
		{BinPath: "x", GatewayBaseURL: "https://example.invalid", GatewayTokenEnv: "T"},
	} {
		if _, err := selectEngine(context.Background(), specWith("dsh", c)); err == nil {
			t.Fatalf("任何 [dsh] 配置组合都不该放行:%+v", c)
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
