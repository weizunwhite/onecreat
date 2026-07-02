package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// escapeTOMLMultiline 转义字符串使其能安全嵌入 TOML 多行 basic string("""..."""）。
// system_prompt 是 coding-agent 的提示词,常含代码围栏、docstring 的 """ 三引号、或以
// 反斜杠结尾;若像原来那样当原始多行串直接拼进 """...""",这些字符会写出非法 TOML,下次
// config.Load 解析失败 → boot.Build 报错 → 整个桌面 app / CLI 起不来(H5,自损配置)。
// 多行 basic string 里反斜杠仍是转义引导符,故先把 \ 转成 \\(顺带修掉「内容以 \ 结尾被
// 当成续行符」),再把每个 " 转成 \"(彻底杜绝任何 """ 闭合歧义)。这样既始终合法,又能
// 精确 round-trip 回原文。
func escapeTOMLMultiline(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// RenderTOML renders the config as annotated TOML in the `reasonix setup` house style:
// comments preserved, system_prompt as a multi-line string, helpful hints. The
// output round-trips back through Load (see render_test.go).
func RenderTOML(c *Config) string {
	var b strings.Builder

	b.WriteString("# Reasonix configuration.\n")
	b.WriteString("# Resolution order: flag > ./reasonix.toml > ~/.config/reasonix/config.toml > built-in defaults.\n")
	b.WriteString("# Secrets come from the environment via api_key_env; never put keys here.\n\n")

	fmt.Fprintf(&b, "default_model = %q\n", c.DefaultModel)
	if c.Language != "" {
		fmt.Fprintf(&b, "language      = %q   # ui/model language; empty = auto-detect from $LANG / $REASONIX_LANG\n", c.Language)
	} else {
		b.WriteString("# language      = \"zh\"   # ui/model language; empty = auto-detect from $LANG / $REASONIX_LANG\n")
	}
	b.WriteString("\n")

	b.WriteString("[ui]\n")
	fmt.Fprintf(&b, "theme = %q   # auto|dark|light; CLI colors only; REASONIX_THEME can override per run\n", c.UITheme())
	if style := c.UIThemeStyle(); style != "" {
		fmt.Fprintf(&b, "theme_style = %q   # accent palette; REASONIX_THEME_STYLE can override per run\n", style)
	} else {
		b.WriteString("# theme_style = \"graphite\"   # graphite|ember|aurora|midnight|sandstone|porcelain|linen|glacier\n")
	}
	b.WriteString("\n")

	b.WriteString("[network]\n")
	fmt.Fprintf(&b, "proxy_mode = %q   # auto|env|custom|off; auto currently uses env proxy\n", c.NetworkProxyMode())
	if c.Network.ProxyURL != "" {
		fmt.Fprintf(&b, "proxy_url  = %q   # custom override, e.g. socks5://127.0.0.1:7890\n", c.Network.ProxyURL)
	} else {
		b.WriteString("# proxy_url  = \"socks5://127.0.0.1:7890\"   # optional custom override\n")
	}
	if c.Network.NoProxy != "" {
		fmt.Fprintf(&b, "no_proxy   = %q   # honored for proxy_mode = \"custom\"\n", c.Network.NoProxy)
	} else {
		b.WriteString("# no_proxy   = \"localhost,127.0.0.1,.local\"   # honored for proxy_mode = \"custom\"\n")
	}
	b.WriteString("\n[network.proxy]\n")
	proxyType := c.Network.Proxy.Type
	if proxyType == "" {
		proxyType = "socks5"
	}
	fmt.Fprintf(&b, "type = %q   # http|https|socks5|socks5h\n", proxyType)
	if c.Network.Proxy.Server != "" {
		fmt.Fprintf(&b, "server = %q\n", c.Network.Proxy.Server)
	} else {
		b.WriteString("# server = \"127.0.0.1\"\n")
	}
	if c.Network.Proxy.Port > 0 {
		fmt.Fprintf(&b, "port = %d\n", c.Network.Proxy.Port)
	} else {
		b.WriteString("# port = 7890\n")
	}
	if c.Network.Proxy.Username != "" {
		fmt.Fprintf(&b, "username = %q\n", c.Network.Proxy.Username)
	} else {
		b.WriteString("# username = \"\"\n")
	}
	if c.Network.Proxy.Password != "" {
		fmt.Fprintf(&b, "password = %q   # supports ${VAR} expansion\n", c.Network.Proxy.Password)
	} else {
		b.WriteString("# password = \"${REASONIX_PROXY_PASSWORD}\"   # optional; supports ${VAR} expansion\n")
	}
	b.WriteString("\n")

	b.WriteString("[agent]\n")
	b.WriteString("system_prompt = \"\"\"\n")
	b.WriteString(escapeTOMLMultiline(c.Agent.SystemPrompt))
	b.WriteString("\"\"\"\n")
	if c.Agent.SystemPromptFile != "" {
		fmt.Fprintf(&b, "system_prompt_file = %q\n", c.Agent.SystemPromptFile)
	} else {
		b.WriteString("# system_prompt_file = \"prompts/system.md\"   # overrides system_prompt when set\n")
	}
	fmt.Fprintf(&b, "max_steps   = %d\n", c.Agent.MaxSteps)
	fmt.Fprintf(&b, "temperature = %s\n", formatFloat(c.Agent.Temperature))
	autoPlan := c.Agent.AutoPlan
	if autoPlan == "" {
		autoPlan = "off"
	}
	fmt.Fprintf(&b, "auto_plan   = %q   # off|ask|on; 默认 off（模型自己判断是否先出方案）。ask/on 会按关键词自动切进 plan 模式\n", autoPlan)
	if c.Agent.AutoPlanClassifier != "" {
		fmt.Fprintf(&b, "auto_plan_classifier = %q   # optional provider/model for borderline auto-plan decisions\n", c.Agent.AutoPlanClassifier)
	} else {
		b.WriteString("# auto_plan_classifier = \"deepseek-flash\"   # optional; only used for borderline tasks\n")
	}
	if c.Agent.PlannerModel != "" {
		fmt.Fprintf(&b, "planner_model = %q   # low-frequency planner (two-model collaboration)\n", c.Agent.PlannerModel)
	} else {
		b.WriteString("# planner_model = \"mimo\"   # optional: enable two-model collaboration\n")
	}
	if c.Agent.SubagentModel != "" {
		fmt.Fprintf(&b, "subagent_model = %q   # default model for runAs=subagent skills\n", c.Agent.SubagentModel)
	} else {
		b.WriteString("# subagent_model = \"deepseek-pro\"   # optional default for runAs=subagent skills\n")
	}
	if len(c.Agent.SubagentModels) > 0 {
		fmt.Fprintf(&b, "subagent_models = %s   # per-skill overrides\n", renderStringMap(c.Agent.SubagentModels))
	} else {
		b.WriteString("# subagent_models = { review = \"deepseek-pro\", security_review = \"deepseek-pro\" }   # per-skill overrides\n")
	}
	if c.Agent.OutputStyle != "" {
		fmt.Fprintf(&b, "output_style = %q   # persona/tone folded into the prompt\n", c.Agent.OutputStyle)
	} else {
		b.WriteString("# output_style = \"explanatory\"   # explanatory | learning | concise | custom; empty = default\n")
	}
	b.WriteString("\n")

	for _, p := range c.Providers {
		b.WriteString("[[providers]]\n")
		fmt.Fprintf(&b, "name        = %q\n", p.Name)
		fmt.Fprintf(&b, "kind        = %q\n", p.Kind)
		fmt.Fprintf(&b, "base_url    = %q\n", p.BaseURL)
		if len(p.Models) > 0 {
			fmt.Fprintf(&b, "models      = %s\n", renderStringArray(p.Models))
			if p.Default != "" {
				fmt.Fprintf(&b, "default     = %q\n", p.Default)
			}
		} else if p.Model != "" {
			fmt.Fprintf(&b, "model       = %q\n", p.Model)
		}
		if p.ModelsURL != "" {
			fmt.Fprintf(&b, "models_url  = %q   # auto-fetch models from this URL on startup\n", p.ModelsURL)
		}
		fmt.Fprintf(&b, "api_key_env = %q\n", p.APIKeyEnv)
		if p.BalanceURL != "" {
			fmt.Fprintf(&b, "balance_url = %q   # optional; wallet-balance endpoint shown in the status bar\n", p.BalanceURL)
		}
		if p.ContextWindow > 0 {
			fmt.Fprintf(&b, "context_window = %d   # tokens; compaction triggers near this limit\n", p.ContextWindow)
		}
		if p.Price != nil {
			fmt.Fprintf(&b, "price       = { cache_hit = %v, input = %v, output = %v, currency = %q }   # per 1M tokens\n",
				p.Price.CacheHit, p.Price.Input, p.Price.Output, p.Price.Symbol())
		}
		if p.Thinking != "" {
			fmt.Fprintf(&b, "thinking    = %q\n", p.Thinking)
		}
		if p.Effort != "" {
			fmt.Fprintf(&b, "effort      = %q\n", p.Effort)
		}
		b.WriteString("\n")
	}

	b.WriteString("[tools]\n")
	if len(c.Tools.Enabled) == 0 {
		b.WriteString("enabled = []   # empty = all built-in tools\n")
	} else {
		b.WriteString("enabled = [")
		for i, t := range c.Tools.Enabled {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", t)
		}
		b.WriteString("]\n")
	}
	// [tools.search] 过去不被渲染,导致保存后 engine/rg_path 丢失(D1)。
	b.WriteString("\n[tools.search]\n")
	if c.Tools.Search.Engine != "" {
		fmt.Fprintf(&b, "engine  = %q   # auto|native|rg\n", c.Tools.Search.Engine)
	} else {
		b.WriteString("# engine  = \"auto\"   # auto|native|rg; empty = auto\n")
	}
	if c.Tools.Search.RgPath != "" {
		fmt.Fprintf(&b, "rg_path = %q\n", c.Tools.Search.RgPath)
	} else {
		b.WriteString("# rg_path = \"/usr/local/bin/rg\"   # pin a specific ripgrep binary\n")
	}
	b.WriteString("\n")

	b.WriteString("[skills]\n")
	if len(c.Skills.Paths) > 0 {
		fmt.Fprintf(&b, "paths = %s   # extra custom skill roots\n\n", renderStringArray(c.Skills.Paths))
	} else {
		b.WriteString("# paths = [\"~/my-skills\", \"../shared/skills\"]   # extra custom skill roots\n\n")
	}

	b.WriteString("[permissions]\n")
	b.WriteString("# Per-call gating. mode = writer fallback when no rule matches: ask|allow|deny.\n")
	b.WriteString("# Readers always default to allow. Precedence: deny > ask > allow > fallback.\n")
	b.WriteString("# Rules are \"ToolName\" or \"ToolName(glob)\"; '*' matches any run, '?' one char.\n")
	mode := c.Permissions.Mode
	if mode == "" {
		mode = "ask"
	}
	fmt.Fprintf(&b, "mode  = %q\n", mode)
	b.WriteString(renderRuleList("deny", c.Permissions.Deny, `["bash(rm -rf*)", "bash(git push*)"]   # hard-blocked in every mode`))
	b.WriteString(renderRuleList("allow", c.Permissions.Allow, `["bash(go test*)", "bash(git status*)"]   # never prompted`))
	b.WriteString(renderRuleList("ask", c.Permissions.Ask, `["write_file"]   # force a prompt even if otherwise allowed`))
	b.WriteString("\n")

	b.WriteString("[sandbox]\n")
	b.WriteString("# Confine tool blast radius. File-writers (write_file/edit_file/multi_edit)\n")
	b.WriteString("# may only write under workspace_root (empty = current dir) + allow_write.\n")
	b.WriteString("# bash = \"enforce\" (default) jails each command in an OS sandbox (macOS now;\n")
	b.WriteString("# graceful fallback elsewhere); \"off\" disables it. network allows egress.\n")
	if c.Sandbox.WorkspaceRoot != "" {
		fmt.Fprintf(&b, "workspace_root = %q\n", c.Sandbox.WorkspaceRoot)
	} else {
		b.WriteString("# workspace_root = \"\"            # default: current working directory\n")
	}
	if len(c.Sandbox.AllowWrite) > 0 {
		fmt.Fprintf(&b, "allow_write = %s\n", renderStringArray(c.Sandbox.AllowWrite))
	} else {
		b.WriteString("# allow_write = [\"/tmp\"]          # extra dirs writers may also modify\n")
	}
	fmt.Fprintf(&b, "bash    = %q\n", c.BashMode())
	fmt.Fprintf(&b, "network = %v\n", c.Sandbox.Network)
	b.WriteString("\n")

	b.WriteString("[statusline]\n")
	b.WriteString("# A custom status line: a command whose first stdout line replaces the built-in\n")
	b.WriteString("# data row. It receives {\"model\",\"contextUsed\",\"contextWindow\"} as JSON on stdin.\n")
	if c.Statusline.Command != "" {
		fmt.Fprintf(&b, "command = %q\n", c.Statusline.Command)
	} else {
		b.WriteString("# command = \"my-statusline.sh\"\n")
	}
	b.WriteString("\n")

	renderCodegraph(&b, c)
	renderLSP(&b, c)

	b.WriteString("# External MCP servers. type: \"stdio\" (default, a subprocess) | \"http\" | \"sse\".\n")
	b.WriteString("# ${VAR} / ${VAR:-default} are expanded from the environment in command/args/env/url/headers.\n")
	// 只渲染来自 reasonix.toml 的插件;来自 .mcp.json 的(Source=="mcp.json")跳过——
	// 否则一次 Save 会把它们永久复制进 reasonix.toml,并在名字冲突时反过来遮蔽 .mcp.json(D2)。
	tomlPlugins := make([]PluginEntry, 0, len(c.Plugins))
	for _, pl := range c.Plugins {
		if pl.fromMCPJSON() {
			continue
		}
		tomlPlugins = append(tomlPlugins, pl)
	}
	if len(tomlPlugins) == 0 {
		b.WriteString("# [[plugins]]\n")
		b.WriteString("# name    = \"example\"\n")
		b.WriteString("# command = \"reasonix-plugin-example\"\n")
		b.WriteString("# [[plugins]]                                  # a remote server over Streamable HTTP\n")
		b.WriteString("# name    = \"stripe\"\n")
		b.WriteString("# type    = \"http\"\n")
		b.WriteString("# url     = \"https://mcp.stripe.com\"\n")
		b.WriteString("# headers = { Authorization = \"Bearer ${STRIPE_KEY}\" }\n")
	} else {
		for _, pl := range tomlPlugins {
			b.WriteString("\n[[plugins]]\n")
			fmt.Fprintf(&b, "name    = %q\n", pl.Name)
			if pl.Type != "" {
				fmt.Fprintf(&b, "type    = %q\n", pl.Type)
			}
			if pl.Command != "" {
				fmt.Fprintf(&b, "command = %q\n", pl.Command)
			}
			if len(pl.Args) > 0 {
				fmt.Fprintf(&b, "args    = %s\n", renderStringArray(pl.Args))
			}
			if pl.URL != "" {
				fmt.Fprintf(&b, "url     = %q\n", pl.URL)
			}
			if len(pl.Headers) > 0 {
				fmt.Fprintf(&b, "headers = %s\n", renderStringMap(pl.Headers))
			}
			if len(pl.Env) > 0 {
				fmt.Fprintf(&b, "env     = %s\n", renderStringMap(pl.Env))
			}
			if pl.AutoStart != nil {
				fmt.Fprintf(&b, "auto_start = %v\n", *pl.AutoStart)
			}
			// tier 仅当非默认("lazy")时写出,避免每个插件都多一行噪音;但非默认值必须
			// 持久化,否则 eager 插件保存后降级 lazy(D1)。
			if t := pl.ResolvedTier(); t != "lazy" {
				fmt.Fprintf(&b, "tier    = %q   # eager|lazy|background\n", t)
			}
		}
	}

	return b.String()
}

// renderCodegraph 写出 [codegraph] 段(enabled/auto_install/path)。这些字段过去不被
// RenderTOML 渲染,导致任一次保存都把 codegraph.enabled=false 丢失、下次启动又默认开启
// 并触发自动下载(D1)。
func renderCodegraph(b *strings.Builder, c *Config) {
	b.WriteString("[codegraph]\n")
	b.WriteString("# Built-in CodeGraph code intelligence (codegraph_* tools). enabled=false drops them.\n")
	fmt.Fprintf(b, "enabled      = %v\n", c.Codegraph.Enabled)
	fmt.Fprintf(b, "auto_install = %v   # fetch the runtime into the cache on first use\n", c.Codegraph.AutoInstall)
	if c.Codegraph.Path != "" {
		fmt.Fprintf(b, "path         = %q\n", c.Codegraph.Path)
	} else {
		b.WriteString("# path       = \"/usr/local/bin/codegraph\"   # override binary resolution\n")
	}
	b.WriteString("\n")
}

// renderLSP 写出 [lsp] 段(enabled + [lsp.servers.<lang>] 覆盖)。过去不被渲染,导致
// 保存后 lsp 的 enabled/servers 丢失(D1)。
func renderLSP(b *strings.Builder, c *Config) {
	b.WriteString("[lsp]\n")
	b.WriteString("# Optional Language Server Protocol tools (dormant until a server is on PATH).\n")
	fmt.Fprintf(b, "enabled = %v\n", c.LSP.Enabled)
	langs := make([]string, 0, len(c.LSP.Servers))
	for lang := range c.LSP.Servers {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		s := c.LSP.Servers[lang]
		fmt.Fprintf(b, "\n[lsp.servers.%s]\n", tomlKey(lang))
		if s.Command != "" {
			fmt.Fprintf(b, "command      = %q\n", s.Command)
		}
		if len(s.Args) > 0 {
			fmt.Fprintf(b, "args         = %s\n", renderStringArray(s.Args))
		}
		if len(s.Env) > 0 {
			fmt.Fprintf(b, "env          = %s\n", renderStringMap(s.Env))
		}
		if s.LanguageID != "" {
			fmt.Fprintf(b, "language_id  = %q\n", s.LanguageID)
		}
		if len(s.Extensions) > 0 {
			fmt.Fprintf(b, "extensions   = %s\n", renderStringArray(s.Extensions))
		}
		if s.InstallHint != "" {
			fmt.Fprintf(b, "install_hint = %q\n", s.InstallHint)
		}
	}
	b.WriteString("\n")
}

// renderStringArray renders a []string as a TOML inline array.
func renderStringArray(ss []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range ss {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteByte(']')
	return b.String()
}

// renderStringMap renders a map[string]string as a TOML inline table with keys
// in sorted order so output is deterministic (round-trips cleanly).
func renderStringMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s = %q", tomlKey(k), m[k])
	}
	b.WriteString(" }")
	return b.String()
}

// tomlKey renders a map key as a TOML key: bare when it uses only bare-key-safe
// characters (A-Za-z0-9_-), otherwise double-quoted. Without this, a user-defined
// key like "c++" (a valid quoted TOML key that Load accepts) renders as the bare
// key c++, which is invalid TOML — the SaveTo round-trip probe then rejects every
// subsequent save with an error that doesn't point at the real cause.
func tomlKey(k string) string {
	if k == "" {
		return `""`
	}
	for _, r := range k {
		bareSafe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !bareSafe {
			return fmt.Sprintf("%q", k)
		}
	}
	return k
}

// renderRuleList emits a permission rule list. A populated list renders as an
// active TOML array; an empty one renders as a commented example so `reasonix setup`
// scaffolds discoverable guidance without imposing surprising rules.
func renderRuleList(key string, rules []string, example string) string {
	if len(rules) == 0 {
		return fmt.Sprintf("# %s = %s\n", key, example)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s = [", key)
	for i, r := range rules {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", r)
	}
	b.WriteString("]\n")
	return b.String()
}

// formatFloat ensures a float renders with a decimal point so TOML types it as a
// float, not an integer (e.g. 0 -> "0.0").
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
