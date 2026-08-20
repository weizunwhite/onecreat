package boot

// Plan 12:`engine = "dsh"` 在此之前是个**不生效**的配置项 —— cfg.Engine 除了被渲染
// 进 TOML 之外没有任何消费者,用户写了它,程序静默跑内置内核。这几条用例锁住「它现在
// 真的会选引擎」,以及「选不出来时大声失败,而不是偷偷退回 native」。

import (
	"context"
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

// engine="dsh" 不再是空配置:它会真的去装配 dsh 适配器,因此缺 bin_path 时必须
// 在装配期报错 —— 静默跑成 native 才是最坏的结果(用户以为自己在用 dsh)。
func TestDSHWithoutBinPathFailsLoudly(t *testing.T) {
	got, err := selectEngine(context.Background(), specWith("dsh", config.DSHConfig{}))
	if err == nil {
		t.Fatalf("缺 bin_path 应报错,却拿到了 %T", got)
	}
	if !strings.Contains(err.Error(), "bin_path") {
		t.Errorf("错误信息应点出 bin_path,拿到:%v", err)
	}
	if _, ok := got.(*native.Engine); ok {
		t.Error("绝不能在 engine=\"dsh\" 配错时静默回退到 native")
	}
}

func TestUnknownEngineIsRejected(t *testing.T) {
	_, err := selectEngine(context.Background(), specWith("gpt-harness", config.DSHConfig{}))
	if err == nil {
		t.Fatal("未知引擎名应报错")
	}
	for _, want := range []string{"gpt-harness", "native", "dsh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q,拿到:%v", want, err)
		}
	}
}

// dsh 拉不起子进程时同样必须失败退出,而且不能把一个半死的适配器交出去。
func TestDSHFailsWhenSidecarCannotStart(t *testing.T) {
	cfg := config.DSHConfig{BinPath: filepath.Join(t.TempDir(), "does-not-exist")}
	got, err := selectEngine(context.Background(), specWith("dsh", cfg))
	if err == nil {
		t.Fatalf("子进程拉不起来应报错,却拿到了 %T", got)
	}
	if got != nil {
		t.Errorf("失败时不该返回引擎,拿到 %T", got)
	}
}

// 装配根选出来的引擎必须带着如实的能力矩阵 —— 上层就是靠它决定能不能 rewind/fork。
func TestSelectedEnginesCarryTheirCapabilities(t *testing.T) {
	n, err := selectEngine(context.Background(), specWith("native", config.DSHConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if !engine.Supports(n, engine.CapFork) {
		t.Error("内置内核应支持 fork")
	}
	a, err := dsh.NewAdapter(dsh.AdapterOptions{Options: dsh.Options{
		Cfg: config.DSHConfig{BinPath: filepath.Join(t.TempDir(), "dsh")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if engine.Supports(a, engine.CapFork) {
		t.Error("dsh 不该声明 fork")
	}
}
