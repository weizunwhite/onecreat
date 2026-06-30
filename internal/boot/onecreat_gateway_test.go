package boot

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/config"
)

// 网关模式下应禁用客户端 planner(双模型):否则 planner 不走网关,会泄露真实模型名(阶段
// 标记 / 顶栏 label)并绕过网关计量。验证 label 不再含 "planner"。
func TestBuildDisablesPlannerInGatewayMode(t *testing.T) {
	const cfg = `
default_model = "exec-model"

[codegraph]
enabled = false

[agent]
system_prompt = "BASE"
planner_model = "plan-model"

[[providers]]
name = "exec-model"
kind = "openai"
base_url = "https://example.invalid"
model = "exec-x"
api_key_env = "REASONIX_TEST_KEY_UNSET"

[[providers]]
name = "plan-model"
kind = "openai"
base_url = "https://example.invalid"
model = "plan-y"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`
	// 非网关模式:双模型生效,label 含 "planner"。
	d1 := t.TempDir()
	t.Chdir(d1)
	writeFile(t, d1, "onecreat.toml", cfg)
	t.Setenv("ONECREAT_GATEWAY_URL", "")
	c1, err := Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Build(direct): %v", err)
	}
	if !strings.Contains(c1.Label(), "planner") {
		c1.Close()
		t.Fatalf("非网关模式 label 应含 planner,得到 %q", c1.Label())
	}
	c1.Close()

	// 网关模式:planner 被禁用,label 不含 "planner"。
	d2 := t.TempDir()
	t.Chdir(d2)
	writeFile(t, d2, "onecreat.toml", cfg)
	t.Setenv("ONECREAT_GATEWAY_URL", "https://t.example.com/api/onecreat/v1")
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "tok")
	c2, err := Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Build(gateway): %v", err)
	}
	defer c2.Close()
	if strings.Contains(c2.Label(), "planner") {
		t.Fatalf("网关模式应禁用 planner,label 不应含 planner,得到 %q", c2.Label())
	}
}

// M4:applyOnecreatGateway 是「是否计量 / 送哪档模型 / 用什么 token 计费」的核心改写,
// 此前零测试覆盖(H2/H3 等钱 bug 正是从这片代码溜过去的)。锁死它的行为。
func TestApplyOnecreatGateway(t *testing.T) {
	// 非网关模式(env 未设):零副作用 —— 命令行 / 未登录客户端必须完全不受影响。
	t.Setenv("ONECREAT_GATEWAY_URL", "")
	t.Setenv("ONECREAT_TIER", "")
	e := &config.ProviderEntry{Kind: "openai", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", Model: "deepseek-flash"}
	applyOnecreatGateway(e)
	if e.BaseURL != "https://api.deepseek.com" || e.APIKeyEnv != "DEEPSEEK_API_KEY" || e.BalanceURL == "" || e.Model != "deepseek-flash" {
		t.Fatalf("env 未设时不应改写 entry: %+v", e)
	}

	// 网关模式 + 档位:改 Name→平台别名、BaseURL→网关、key→网关 token、清直连余额、model→档位。
	t.Setenv("ONECREAT_GATEWAY_URL", "https://t.example.com/api/onecreat/v1")
	t.Setenv("ONECREAT_TIER", "tier-2")
	e2 := &config.ProviderEntry{Kind: "openai", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", Model: "deepseek-flash"}
	applyOnecreatGateway(e2)
	if e2.Name != "onecreat" {
		t.Fatalf("Name 未改成平台别名: %q", e2.Name)
	}
	if e2.BaseURL != "https://t.example.com/api/onecreat/v1" {
		t.Fatalf("BaseURL 未改走网关: %q", e2.BaseURL)
	}
	if e2.APIKeyEnv != "ONECREAT_GATEWAY_TOKEN" {
		t.Fatalf("APIKeyEnv 未改成网关 token: %q", e2.APIKeyEnv)
	}
	if e2.BalanceURL != "" {
		t.Fatalf("网关模式应清空直连 BalanceURL: %q", e2.BalanceURL)
	}
	if e2.Model != "tier-2" {
		t.Fatalf("Model 未改成选中档位: %q", e2.Model)
	}

	// 网关模式但未设档位(过渡期旧客户端):保持原 model,网关侧兼容。
	t.Setenv("ONECREAT_TIER", "")
	e3 := &config.ProviderEntry{Kind: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash"}
	applyOnecreatGateway(e3)
	if e3.Name != "onecreat" {
		t.Fatalf("未设档位时也应隐藏 provider 名: %q", e3.Name)
	}
	if e3.Model != "deepseek-flash" {
		t.Fatalf("未设档位时应保留原 model: %q", e3.Model)
	}
	if e3.BaseURL != "https://t.example.com/api/onecreat/v1" {
		t.Fatalf("未设档位仍应改走网关 BaseURL: %q", e3.BaseURL)
	}

	// 非 openai kind:即便网关 env 已设也不改写。注意这是本函数的边界,也正是 H4 的缺口
	// (登录用户加一个 anthropic provider + 自带 key 即绕过网关/计量);H4 的强制必须落在
	// 平台网关侧 + settings 后端拒改,本函数只负责 openai 直连→网关的改写。
	t.Setenv("ONECREAT_TIER", "tier-3")
	e4 := &config.ProviderEntry{Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY", Model: "claude"}
	applyOnecreatGateway(e4)
	if e4.BaseURL != "https://api.anthropic.com" || e4.APIKeyEnv != "ANTHROPIC_API_KEY" || e4.Model != "claude" {
		t.Fatalf("非 openai kind 不应被网关改写: %+v", e4)
	}
}
