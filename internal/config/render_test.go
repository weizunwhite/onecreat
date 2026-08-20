package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// TestRenderTOMLRoundTrips ensures the annotated TOML we emit parses back into
// an equivalent config — i.e. the wizard never writes a file it can't read.
// H5 回归:system_prompt 含 TOML 多行 basic string 的危险字符(""" 三引号、代码围栏、
// 末尾反斜杠、双引号)时,渲染结果必须仍是合法 TOML 且精确 round-trip —— 否则下次 Load
// 解析失败,把配置写 brick、app/CLI 起不来。
func TestRenderTOMLEscapesSystemPrompt(t *testing.T) {
	tricky := []string{
		"Use \"\"\" triple quotes in docstrings.",
		"Code fence:\n```py\nx = \"\"\"hi\"\"\"\n```\n",
		"Trailing backslash\\",
		"Path C:\\Users\\x and a quote \" here",
		"plain, no specials",
	}
	for _, sp := range tricky {
		c := Default()
		c.Agent.SystemPrompt = sp
		rendered := RenderTOML(c)
		var got Config
		if _, err := toml.Decode(rendered, &got); err != nil {
			t.Fatalf("system_prompt %q 渲染出非法 TOML: %v\n---\n%s", sp, err, rendered)
		}
		if got.Agent.SystemPrompt != sp {
			t.Fatalf("system_prompt 未精确 round-trip:\n got %q\nwant %q", got.Agent.SystemPrompt, sp)
		}
	}
}

func TestRenderTOMLRoundTrips(t *testing.T) {
	orig := Default()
	orig.DefaultModel = "mimo-pro"
	orig.Language = "zh"
	orig.UI.Theme = "light"
	orig.UI.ThemeStyle = "glacier"
	orig.Agent.AutoPlanClassifier = "deepseek-flash"
	orig.Agent.SubagentModel = "mimo-pro"
	orig.Agent.SubagentModels = map[string]string{"review": "deepseek-pro"}
	orig.Permissions = PermissionsConfig{
		Mode:  "deny",
		Deny:  []string{"bash(rm -rf*)"},
		Allow: []string{"bash(go test*)", "read_file"},
	}
	orig.Network = NetworkConfig{
		ProxyMode: "custom",
		NoProxy:   "localhost,127.0.0.1",
		Proxy: NetworkProxyConfig{
			Type:     "socks5",
			Server:   "127.0.0.1",
			Port:     7890,
			Username: "user",
			Password: "${REASONIX_PROXY_PASSWORD}",
		},
	}
	orig.Skills.Paths = []string{"~/my-skills", "../shared/skills"}
	// D1:这些 section/字段过去不被 RenderTOML 渲染,任一次保存都会丢失。设为非默认值。
	orig.Codegraph = CodegraphConfig{Enabled: false, AutoInstall: false, Path: "/opt/codegraph"}
	orig.LSP = LSPConfig{Enabled: false, Servers: map[string]LSPServer{
		"elixir": {Command: "elixir-ls", LanguageID: "elixir", Extensions: []string{".ex", ".exs"}, InstallHint: "mix do local.hex"},
		// 非裸键安全的语言键(含 '+'):必须渲染为引号键 [lsp.servers."c++"],否则渲染出的
		// TOML 非法,SaveTo round-trip 探针拒绝写入,此后所有保存都失败。
		"c++": {Command: "clangd", LanguageID: "cpp", Extensions: []string{".cpp", ".hpp"}},
	}}
	orig.Tools.Search = SearchConfig{Engine: "rg", RgPath: "/usr/local/bin/rg"}
	// dsh 引擎选择 + [dsh] 段必须 round-trip(否则保存后引擎配置丢失)。
	orig.Engine = "dsh"
	orig.DSH = DSHConfig{
		BinPath:           "node",
		Args:              []string{"lib/bin.js", "cordis.yml"},
		Version:           "0.1.0-rc.8",
		StartupTimeoutSec: 30,
		GatewayBaseURL:    "https://t.weizunxy.com/api/onecreat/v1",
		GatewayTokenEnv:   "ONECREAT_GATEWAY_TOKEN",
		ModelPlaceholder:  "onecreat",
		RuntimeDir:        "/opt/onecreat/runtime/dsh",
		Profile:           "profiles/onecreat.cordis.yml",
		DirectModel:       "deepseek-v4-flash",
	}
	orig.Plugins = []PluginEntry{
		{Name: "example", Command: "reasonix-plugin-example", Tier: "eager"}, // D1:tier 必须持久化
		{Name: "stripe", Type: "http", URL: "https://mcp.stripe.com", Headers: map[string]string{"Authorization": "Bearer x"}, AutoStart: boolPtr(false)},
	}
	mm, _ := orig.Provider("mimo-pro")
	mm.BaseURL = "http://localhost:8000/v1"
	ds, _ := orig.Provider("deepseek-flash")
	ds.Effort = "max"

	rendered := RenderTOML(orig)

	var got Config
	if _, err := toml.Decode(rendered, &got); err != nil {
		t.Fatalf("rendered TOML does not parse: %v\n---\n%s", err, rendered)
	}

	if got.DefaultModel != "mimo-pro" {
		t.Errorf("default_model = %q, want mimo-pro", got.DefaultModel)
	}
	if got.Language != "zh" {
		t.Errorf("language = %q, want zh", got.Language)
	}
	if got.UI.Theme != "light" {
		t.Errorf("ui.theme = %q, want light", got.UI.Theme)
	}
	if got.UI.ThemeStyle != "glacier" {
		t.Errorf("ui.theme_style = %q, want glacier", got.UI.ThemeStyle)
	}
	if got.Agent.MaxSteps != orig.Agent.MaxSteps {
		t.Errorf("max_steps = %d, want %d", got.Agent.MaxSteps, orig.Agent.MaxSteps)
	}
	if got.Agent.Temperature != orig.Agent.Temperature {
		t.Errorf("temperature = %v, want %v", got.Agent.Temperature, orig.Agent.Temperature)
	}
	if got.Agent.AutoPlan != "off" {
		t.Errorf("auto_plan = %q, want off", got.Agent.AutoPlan)
	}
	if got.Agent.AutoPlanClassifier != "deepseek-flash" {
		t.Errorf("auto_plan_classifier = %q, want deepseek-flash", got.Agent.AutoPlanClassifier)
	}
	if got.Agent.SystemPrompt != orig.Agent.SystemPrompt {
		t.Errorf("system_prompt mismatch:\n got %q\nwant %q", got.Agent.SystemPrompt, orig.Agent.SystemPrompt)
	}
	if got.Agent.SubagentModel != "mimo-pro" {
		t.Errorf("subagent_model = %q, want mimo-pro", got.Agent.SubagentModel)
	}
	if got.Agent.SubagentModels["review"] != "deepseek-pro" {
		t.Errorf("subagent_models.review = %q, want deepseek-pro", got.Agent.SubagentModels["review"])
	}
	if g, _ := got.Provider("mimo-pro"); g == nil || g.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("mimo-pro base_url not preserved: %+v", g)
	}
	if g, _ := got.Provider("deepseek-flash"); g == nil || g.Effort != "max" {
		t.Errorf("deepseek-flash effort not preserved: %+v", g)
	}
	if len(got.Providers) != len(orig.Providers) {
		t.Errorf("providers count = %d, want %d", len(got.Providers), len(orig.Providers))
	}
	if got.Permissions.Mode != "deny" {
		t.Errorf("permissions.mode = %q, want deny", got.Permissions.Mode)
	}
	if len(got.Permissions.Deny) != 1 || got.Permissions.Deny[0] != "bash(rm -rf*)" {
		t.Errorf("permissions.deny = %v, want [bash(rm -rf*)]", got.Permissions.Deny)
	}
	if len(got.Permissions.Allow) != 2 {
		t.Errorf("permissions.allow = %v, want 2 entries", got.Permissions.Allow)
	}
	if got.Network.ProxyMode != "custom" || got.Network.Proxy.Type != "socks5" || got.Network.Proxy.Port != 7890 {
		t.Errorf("network proxy not preserved: %+v", got.Network)
	}
	if got.Engine != "dsh" {
		t.Errorf("engine = %q, want dsh", got.Engine)
	}
	if got.DSH.BinPath != "node" || got.DSH.Version != "0.1.0-rc.8" || got.DSH.StartupTimeoutSec != 30 {
		t.Errorf("dsh scalar fields not preserved: %+v", got.DSH)
	}
	if len(got.DSH.Args) != 2 || got.DSH.Args[0] != "lib/bin.js" || got.DSH.Args[1] != "cordis.yml" {
		t.Errorf("dsh.args not preserved: %v", got.DSH.Args)
	}
	if got.DSH.GatewayBaseURL != "https://t.weizunxy.com/api/onecreat/v1" || got.DSH.ModelPlaceholder != "onecreat" {
		t.Errorf("dsh gateway/model fields not preserved: %+v", got.DSH)
	}
	if got.DSH.RuntimeDir != "/opt/onecreat/runtime/dsh" || got.DSH.Profile != "profiles/onecreat.cordis.yml" || got.DSH.DirectModel != "deepseek-v4-flash" {
		t.Errorf("dsh runtime/profile/direct_model not preserved: %+v", got.DSH)
	}
	if len(got.Skills.Paths) != 2 || got.Skills.Paths[0] != "~/my-skills" {
		t.Errorf("skills.paths = %v", got.Skills.Paths)
	}
	if len(got.Plugins) != 2 {
		t.Fatalf("plugins count = %d, want 2", len(got.Plugins))
	}
	stripe := got.Plugins[1]
	if stripe.Name != "stripe" || stripe.Type != "http" || stripe.URL != "https://mcp.stripe.com" {
		t.Errorf("http plugin not preserved: %+v", stripe)
	}
	if stripe.Headers["Authorization"] != "Bearer x" {
		t.Errorf("plugin headers not preserved: %v", stripe.Headers)
	}
	if stripe.AutoStart == nil || *stripe.AutoStart {
		t.Errorf("auto_start should render and parse as false, got %+v", stripe.AutoStart)
	}
	// D1:tier 必须 round-trip(eager 插件保存后不能降级 lazy)。
	if got.Plugins[0].ResolvedTier() != "eager" {
		t.Errorf("plugin tier = %q, want eager", got.Plugins[0].ResolvedTier())
	}
	// D1:[codegraph] / [lsp] / [tools.search] 必须 round-trip。
	if got.Codegraph.Enabled || got.Codegraph.AutoInstall || got.Codegraph.Path != "/opt/codegraph" {
		t.Errorf("codegraph not preserved: %+v", got.Codegraph)
	}
	if got.LSP.Enabled {
		t.Errorf("lsp.enabled should be false")
	}
	el, ok := got.LSP.Servers["elixir"]
	if !ok || el.Command != "elixir-ls" || el.LanguageID != "elixir" || len(el.Extensions) != 2 || el.InstallHint != "mix do local.hex" {
		t.Errorf("lsp.servers.elixir not preserved: %+v (ok=%v)", el, ok)
	}
	cpp, ok := got.LSP.Servers["c++"]
	if !ok || cpp.Command != "clangd" || cpp.LanguageID != "cpp" {
		t.Errorf(`lsp.servers."c++" (quoted key) not preserved: %+v (ok=%v)`, cpp, ok)
	}
	if got.Tools.Search.Engine != "rg" || got.Tools.Search.RgPath != "/usr/local/bin/rg" {
		t.Errorf("tools.search not preserved: %+v", got.Tools.Search)
	}
}

// TestRenderTOMLSkipsMCPJSONPlugins 钉死 D2:来自 .mcp.json 的插件(Source=="mcp.json")
// 不能被写进 reasonix.toml,否则一次 Save 就把它永久复制进来并遮蔽用户的 .mcp.json。
func TestRenderTOMLSkipsMCPJSONPlugins(t *testing.T) {
	c := Default()
	c.Plugins = []PluginEntry{
		{Name: "from_toml", Command: "x"},
		{Name: "from_mcpjson", Command: "y", Source: pluginSourceMCPJSON},
	}
	rendered := RenderTOML(c)

	var got Config
	if _, err := toml.Decode(rendered, &got); err != nil {
		t.Fatalf("rendered TOML does not parse: %v", err)
	}
	for _, p := range got.Plugins {
		if p.Name == "from_mcpjson" {
			t.Fatalf(".mcp.json plugin was copied into reasonix.toml: %+v", p)
		}
	}
	found := false
	for _, p := range got.Plugins {
		if p.Name == "from_toml" {
			found = true
		}
	}
	if !found {
		t.Fatal("reasonix.toml plugin should still be rendered")
	}
}

func boolPtr(v bool) *bool { return &v }
