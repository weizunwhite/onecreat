package dsh

import (
	"testing"

	"reasonix/internal/config"
)

// 网关模式:wire model 必须是用户当前选中的档位 —— 平台网关就是按 tier-N 映射
// 真实模型与计费的。下发占位符会让"切档"完全不生效(甚至被网关拒)。
func TestWireModelGatewayUsesTier(t *testing.T) {
	t.Setenv(gatewayTierEnv, "tier-3")
	eng, err := New(Options{Gateway: true, Cfg: config.DSHConfig{ModelPlaceholder: "onecreat"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "tier-3" {
		t.Fatalf("网关模式应下发档位 tier-3,得到 %q", got)
	}
}

// 没设档位(过渡期 / 旧会话)时回退占位符,绝不回退成真实模型名。
func TestWireModelGatewayFallsBackToPlaceholder(t *testing.T) {
	t.Setenv(gatewayTierEnv, "")
	eng, err := New(Options{Gateway: true, Cfg: config.DSHConfig{
		ModelPlaceholder: "onecreat", DirectModel: "deepseek-v4-flash",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "onecreat" {
		t.Fatalf("未设档位应回退占位符,得到 %q", got)
	}
	// 占位符也没配时的兜底同样不能是真实模型名。
	eng2, _ := New(Options{Gateway: true})
	if got := eng2.wireModel(); got != "onecreat" {
		t.Fatalf("占位符缺省兜底错误: %q", got)
	}
}

// 直连模式(用户自己的 key)不受 ONECREAT_TIER 影响:那时下发的就是真实模型 id。
func TestWireModelDirectIgnoresTier(t *testing.T) {
	t.Setenv(gatewayTierEnv, "tier-2")
	eng, err := New(Options{Gateway: false, Cfg: config.DSHConfig{DirectModel: "deepseek-v4-flash"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := eng.wireModel(); got != "deepseek-v4-flash" {
		t.Fatalf("直连模式不该读档位,得到 %q", got)
	}
}

// 档位占位符不能被脱敏器误擦(擦了就等于把 model 字段打成 "OneCreat",网关认不出)。
func TestScrubberLeavesTierAlone(t *testing.T) {
	s := NewScrubber("OneCreat", "deepseek-official", "llm-deepseek", "DeepSeek", "deepseek",
		"api.deepseek.com", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL", "https://t.example.com/api/onecreat/v1")
	for _, tier := range []string{"tier-1", "tier-2", "tier-3"} {
		if got := s.Text(tier); got != tier {
			t.Fatalf("档位占位符被误擦: %q → %q", tier, got)
		}
	}
}
