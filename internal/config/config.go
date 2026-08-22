// Package config loads Reasonix's runtime configuration from TOML. Resolution order:
// flag > project ./reasonix.toml > user ~/.config/reasonix/config.toml > built-in defaults.
// Secrets come from the environment via api_key_env and are never stored in
// config files.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"reasonix/internal/netclient"
	"reasonix/internal/provider"
	"reasonix/internal/workspace"
)

// Config is Reasonix's runtime configuration.
type Config struct {
	DefaultModel string            `toml:"default_model"`
	Language     string            `toml:"language"` // ui/model language tag (e.g. "zh"); empty = auto-detect from $LANG / $REASONIX_LANG
	UI           UIConfig          `toml:"ui"`
	Agent        AgentConfig       `toml:"agent"`
	Providers    []ProviderEntry   `toml:"providers"`
	Tools        ToolsConfig       `toml:"tools"`
	Permissions  PermissionsConfig `toml:"permissions"`
	Sandbox      SandboxConfig     `toml:"sandbox"`
	Network      NetworkConfig     `toml:"network"`
	Plugins      []PluginEntry     `toml:"plugins"`
	Skills       SkillsConfig      `toml:"skills"`
	Codegraph    CodegraphConfig   `toml:"codegraph"`
	Statusline   StatuslineConfig  `toml:"statusline"`
	LSP          LSPConfig         `toml:"lsp"`
	// Engine 选择底层 agent 引擎:"native"(现有 Go 内核,默认)或 "dsh"
	// (DeepSeek Harness sidecar,通过 stdio JSON-RPC 驱动)。空 = native。
	// dsh 引擎目前处于 spike 阶段,详见 docs/dsh调研/。
	Engine string    `toml:"engine"`
	DSH    DSHConfig `toml:"dsh"`

	// env is the workspace-scoped .env overlay this config resolves credentials
	// through. Like workspace it is loader-supplied identity, not a configurable
	// field, so it stays unexported and invisible to the TOML codec.
	env Env

	// workspace is the project directory this config was loaded from. It is
	// unexported (and therefore invisible to the TOML codec, which decodes onto
	// an existing *Config) because it is loader-supplied identity, not a
	// configurable field. WriteRoots resolves its default root against it, so a
	// config loaded for workspace B never confines writes to workspace A.
	workspace workspace.Context
}

// Workspace reports the project directory this config was loaded from. The zero
// Context means "process working directory" (see LoadIn).
func (c *Config) Workspace() workspace.Context { return c.workspace }

// Env returns the workspace-scoped environment overlay (process env first, then
// this workspace's .env, then ~/.env). Use it instead of os.Getenv for anything
// a project's .env may supply — os.Getenv cannot tell two workspaces apart.
func (c *Config) Env() Env { return c.env }

// bindEnv attaches the overlay to the config and to every provider entry, so
// ProviderEntry.APIKey resolves through the workspace that loaded it rather than
// through whichever workspace happened to be loaded first (AR-R10).
//
// 每个 ProviderEntry 各带一份:`ResolveModel` 返回的是结构体**副本**,副本必须自己
// 带着叠加层,否则一离开 Config 就退化成读进程环境。
func (c *Config) bindEnv(env Env) {
	c.env = env
	for i := range c.Providers {
		c.Providers[i].env = env
	}
}

// DSHConfig 配置 dsh(DeepSeek Harness)sidecar 引擎。仅当 engine="dsh" 时生效。
// 秘密(网关 token / API key)仍从环境变量取,绝不写这里——只放"环境变量名"。
type DSHConfig struct {
	// BinPath 是驱动 dsh sidecar 的可执行文件(通常是 node 或打包后的 dsh 运行时)。
	BinPath string `toml:"bin_path"`
	// Args 是启动参数(一般包含 cordis.yml 路径)。
	Args []string `toml:"args"`
	// Version 锁死的 dsh 精确版本(developer preview,防破坏性变更漂移)。
	Version string `toml:"version"`
	// StartupTimeoutSec 是等 initialize 握手完成的超时(秒);0 = 内置默认。
	StartupTimeoutSec int `toml:"startup_timeout_sec"`
	// GatewayBaseURL 传给 dsh provider 的 base URL(应指平台网关)。
	GatewayBaseURL string `toml:"gateway_base_url"`
	// GatewayTokenEnv 是持有网关 token 的环境变量名(默认 ONECREAT_GATEWAY_TOKEN)。
	GatewayTokenEnv string `toml:"gateway_token_env"`
	// ModelPlaceholder 是下发给 dsh 的 wire model(档位占位符,绝不填真实模型名)。
	// 只在网关模式下使用;直连模式用 DirectModel。
	ModelPlaceholder string `toml:"model_placeholder"`
	// RuntimeDir 是 OneCreat 自带 dsh 组合包的目录(含 node_modules 与 profiles/)。
	// 空 = 自动解析:先找主程序同目录的 runtime/dsh(打包形态),再找开发仓库的 dsh/。
	RuntimeDir string `toml:"runtime_dir"`
	// Profile 是组合包内 cordis profile 的相对路径。空 = profiles/onecreat.cordis.yml。
	Profile string `toml:"profile"`
	// DirectModel 是**直连模式**(未走平台网关,用自己的 DEEPSEEK_API_KEY)下发给 dsh
	// 的真实模型 id。网关模式下不使用它(那时下发 ModelPlaceholder)。
	DirectModel string `toml:"direct_model"`
}

// UIConfig controls presentation-only settings. Theme affects CLI rendering; the
// desktop frontend keeps its own browser-local theme setting.
type UIConfig struct {
	Theme      string `toml:"theme"`       // auto|dark|light; empty resolves to auto
	ThemeStyle string `toml:"theme_style"` // graphite|ember|aurora|midnight|sandstone|porcelain|linen|glacier
}

// UITheme normalizes ui.theme to a supported value.
func (c *Config) UITheme() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.Theme)) {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// UIThemeStyle normalizes ui.theme_style. Empty means "pick the default style
// for the resolved light/dark shell".
func (c *Config) UIThemeStyle() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.ThemeStyle)) {
	case "graphite", "ember", "aurora", "midnight", "sandstone", "porcelain", "linen", "glacier":
		return strings.ToLower(strings.TrimSpace(c.UI.ThemeStyle))
	default:
		return ""
	}
}

// LSPConfig governs the optional Language Server Protocol tools (lsp_definition,
// lsp_references, lsp_hover, lsp_diagnostics). Enabled defaults to true; the
// servers themselves are never bundled — each resolves on PATH and the tool
// returns an install hint when it is missing, so the capability is dormant until
// the user installs a server. Servers overrides or extends the built-in language
// → server map, keyed by language id (e.g. "go", "rust", "python").
type LSPConfig struct {
	Enabled bool                 `toml:"enabled"`
	Servers map[string]LSPServer `toml:"servers"`
}

// LSPServer overrides a built-in language's server or, when keyed by a new
// language, adds one. An empty field falls back to the built-in default for that
// language; Extensions is required when adding a language the built-ins don't
// cover (e.g. ".ex" for Elixir) so files route to it.
type LSPServer struct {
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Env         map[string]string `toml:"env"`
	LanguageID  string            `toml:"language_id"`
	Extensions  []string          `toml:"extensions"`
	InstallHint string            `toml:"install_hint"`
}

// StatuslineConfig configures a custom status line. Command, when set, is run at
// startup and after each turn; its first line of stdout replaces the built-in
// status data row. A JSON payload (model, context tokens, cwd) is fed on stdin.
type StatuslineConfig struct {
	Command string `toml:"command"`
}

// CodegraphConfig governs the built-in CodeGraph MCP server — symbol/call-graph
// code intelligence (tree-sitter + SQLite) that gives the agent codegraph_*
// search / context / explore / trace / node tools. Enabled defaults to true; set
// enabled = false to drop those tools and fall back to grep/glob. AutoInstall
// (default true) lets reasonix fetch the CodeGraph runtime into its cache on first
// use; set false to require an explicit `reasonix codegraph install` (e.g. for
// air-gapped or headless runs). Path overrides binary resolution; empty resolves
// the cache, then a `codegraph` on PATH, then a bundle beside the executable.
type CodegraphConfig struct {
	Enabled     bool   `toml:"enabled"`
	AutoInstall bool   `toml:"auto_install"`
	Path        string `toml:"path"`
}

// NetworkConfig controls ordinary outbound HTTP traffic such as model providers,
// wallet-balance lookups, updater checks, and CodeGraph downloads. It intentionally
// does not apply to web_fetch, which keeps its own SSRF-guarded dialer.
type NetworkConfig struct {
	// ProxyMode is "auto" (default; environment proxy for now), "env", "custom",
	// or "off". auto leaves room for OS proxy detection later without changing the
	// config shape.
	ProxyMode string `toml:"proxy_mode"`
	// ProxyURL is an advanced custom override such as "socks5://127.0.0.1:7890".
	// When set and proxy_mode = "custom", it wins over the structured proxy table.
	ProxyURL string `toml:"proxy_url"`
	// NoProxy is honored for custom proxies. Env/auto modes use NO_PROXY from the
	// process environment instead.
	NoProxy string             `toml:"no_proxy"`
	Proxy   NetworkProxyConfig `toml:"proxy"`
}

// NetworkProxyConfig is the structured custom-proxy editor shape. Password is
// optional and supports ${VAR} expansion, so users can avoid storing it literally.
type NetworkProxyConfig struct {
	Type     string `toml:"type"` // http|https|socks5|socks5h
	Server   string `toml:"server"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// NetworkProxySpec returns the expanded proxy settings used by netclient.
func (c *Config) NetworkProxySpec() netclient.ProxySpec {
	return netclient.ProxySpec{
		Mode:     c.Network.ProxyMode,
		URL:      ExpandVars(c.Network.ProxyURL),
		NoProxy:  ExpandVars(c.Network.NoProxy),
		Type:     c.Network.Proxy.Type,
		Server:   ExpandVars(c.Network.Proxy.Server),
		Port:     c.Network.Proxy.Port,
		Username: ExpandVars(c.Network.Proxy.Username),
		Password: ExpandVars(c.Network.Proxy.Password),
	}
}

// NetworkProxyMode normalizes network.proxy_mode to a known value.
func (c *Config) NetworkProxyMode() string {
	return netclient.NormalizeMode(c.Network.ProxyMode)
}

// SkillsConfig configures skill discovery. Paths adds extra "custom"-scope skill
// roots — each a directory of SKILL.md / <name>.md playbooks — scanned between
// the project roots (.reasonix/.agents/.claude under the workspace) and the
// global roots (the same three under the home dir). ~ and relative paths and
// ${VAR} expansion are supported.
type SkillsConfig struct {
	Paths []string `toml:"paths"`
}

// SkillCustomPaths returns the configured custom skill roots with ${VAR}
// expanded; empty entries are dropped.
func (c *Config) SkillCustomPaths() []string {
	var out []string
	for _, p := range c.Skills.Paths {
		if p = ExpandVars(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// SandboxConfig bounds the blast radius of tool calls (Phase 0: file-writer
// confinement). WorkspaceRoot is the directory the built-in file writers
// (write_file / edit_file / multi_edit) may modify; empty means the current
// working directory, so writes stay inside the project by default. AllowWrite
// lists extra directories writers may also touch (e.g. a sibling repo or a temp
// dir). Both support ${VAR} / ${VAR:-default} expansion. Reads are unrestricted;
// confining `bash` is Phase 1 (OS-level sandbox).
type SandboxConfig struct {
	WorkspaceRoot string   `toml:"workspace_root"`
	AllowWrite    []string `toml:"allow_write"`
	// Bash is the OS-sandbox mode for the bash tool: "enforce" (default) jails
	// each command, "off" runs it unconfined. Phase 1; macOS only for now, with
	// a graceful fallback elsewhere (see internal/sandbox).
	Bash string `toml:"bash"`
	// Network allows network egress from inside the bash sandbox. Defaults true
	// so module/package downloads keep working; the boundary is then writes.
	Network bool `toml:"network"`
}

// WriteRoots returns the directories file-writer tools may modify: the
// workspace root (defaulting to the current working directory when unset) plus
// any AllowWrite extras, with ${VAR} expanded. The roots are returned as given
// (relative or absolute); the confiner resolves them to absolute, symlink-free
// paths. The result is always non-empty, so confinement is on by default.
func (c *Config) WriteRoots() []string {
	root := ExpandVars(c.Sandbox.WorkspaceRoot)
	if root == "" {
		// Default to this config's workspace root. Only when there is no
		// explicit workspace (the CLI, or a Config built by Default()) does it
		// fall back to the process working directory, which is what this did
		// unconditionally before workspaces became explicit.
		root = c.workspace.Root()
	}
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = "."
		}
	}
	roots := []string{root}
	for _, d := range c.Sandbox.AllowWrite {
		if d = ExpandVars(d); d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// BashMode normalises the bash-sandbox mode: only an explicit "off" disables
// it; empty or any other value resolves to "enforce", so the sandbox is on by
// default and fails safe.
func (c *Config) BashMode() string {
	if c.Sandbox.Bash == "off" {
		return "off"
	}
	return "enforce"
}

// AgentConfig configures the harness loop. PlannerModel is optional: when set
// to another provider's name it enables two-model collaboration, where the
// planner handles low-frequency planning in its own session (kept separate so
// each model's prompt prefix stays cache-stable). SubagentModel is the optional
// default for runAs=subagent skills; SubagentModels overrides it per skill name.
type AgentConfig struct {
	SystemPrompt     string            `toml:"system_prompt"`
	SystemPromptFile string            `toml:"system_prompt_file"`
	MaxSteps         int               `toml:"max_steps"` // tool-call rounds per turn; 0 = unlimited
	Temperature      float64           `toml:"temperature"`
	PlannerModel     string            `toml:"planner_model"`
	SubagentModel    string            `toml:"subagent_model"`
	SubagentModels   map[string]string `toml:"subagent_models"`
	// OutputStyle selects a persona/tone block folded into the system prompt at
	// startup (a built-in like "explanatory"/"learning"/"concise", or a custom
	// .reasonix/output-styles/<name>.md). Empty = the unmodified prompt.
	OutputStyle string `toml:"output_style"`
	// AutoPlan controls whether interactive turns that look multi-step start in
	// plan mode automatically: "off" disables it, "ask"/"on" enable the gate.
	AutoPlan string `toml:"auto_plan"`
	// AutoPlanClassifier optionally names a provider/model used to classify
	// borderline auto-plan decisions. Empty keeps the zero-cost heuristic path.
	AutoPlanClassifier string `toml:"auto_plan_classifier"`
}

// ProviderEntry declares a model provider instance. ContextWindow is the model's
// token budget; the harness compacts older history as a turn's prompt approaches
// it (see agent compaction). 0 disables compaction for the instance.
type ProviderEntry struct {
	Name          string            `toml:"name"`
	Kind          string            `toml:"kind"`
	BaseURL       string            `toml:"base_url"`
	Model         string            `toml:"model"`      // a single model (back-compat)
	Models        []string          `toml:"models"`     // a vendor's model list (one base_url/key, many models)
	ModelsURL     string            `toml:"models_url"` // auto-fetch models from this URL on startup
	Default       string            `toml:"default"`    // default model when Models is set (else Models[0])
	APIKeyEnv     string            `toml:"api_key_env"`
	BalanceURL    string            `toml:"balance_url"` // optional; a provider-specific wallet-balance endpoint (DeepSeek: https://api.deepseek.com/user/balance). Empty = no balance readout.
	ContextWindow int               `toml:"context_window"`
	Price         *provider.Pricing `toml:"price"`
	// Thinking / Effort are provider-kind-specific knobs forwarded to the provider
	// via Config.Extra. The anthropic provider reads Thinking="adaptive" to enable
	// extended thinking and Effort ("low".."max") to tune depth. The
	// openai-compatible provider forwards Effort as reasoning_effort for
	// thinking-capable models; DeepSeek accepts high|max.
	// Empty = provider default.
	Thinking string `toml:"thinking"`
	Effort   string `toml:"effort"`
	// env is the workspace-scoped .env overlay APIKey resolves through. It is
	// loader-supplied (Config.bindEnv), unexported so the TOML codec never sees
	// it, and carried by value so a ResolveModel copy keeps resolving through the
	// workspace it came from (AR-R10). The zero value behaves exactly like
	// os.Getenv, so hand-built entries are unaffected.
	env Env
}

// ModelList returns the models this provider exposes: the explicit `models` list,
// or the single `model` as a one-element list (back-compat). Empty if neither set.
func (e *ProviderEntry) ModelList() []string {
	if len(e.Models) > 0 {
		return e.Models
	}
	if e.Model != "" {
		return []string{e.Model}
	}
	return nil
}

// DefaultModel returns the provider's default model: the explicit `default`, else
// the first of ModelList.
func (e *ProviderEntry) DefaultModel() string {
	if e.Default != "" {
		return e.Default
	}
	if l := e.ModelList(); len(l) > 0 {
		return l[0]
	}
	return ""
}

// HasModel reports whether m is one of the provider's models.
func (e *ProviderEntry) HasModel(m string) bool {
	for _, x := range e.ModelList() {
		if x == m {
			return true
		}
	}
	return false
}

// ToolsConfig selects which built-in tools are enabled. Empty means all of them.
type ToolsConfig struct {
	Enabled []string     `toml:"enabled"`
	Search  SearchConfig `toml:"search"`
}

// SearchConfig tunes the grep tool's engine. Engine is "auto" (default — use
// ripgrep when it's on PATH, else the native Go scanner), "native" (always Go),
// or "rg" (require ripgrep; warn at startup and fall back to native if absent).
// RgPath optionally points at a specific ripgrep binary instead of a PATH lookup.
type SearchConfig struct {
	Engine string `toml:"engine"`
	RgPath string `toml:"rg_path"`
}

// PermissionsConfig declares the per-call permission policy (see
// internal/permission). Mode is the fallback decision for writer tools when no
// rule matches ("ask" | "allow" | "deny"; default "ask"); read-only tools always
// fall back to allow. Allow/Ask/Deny are rule lists of the form "ToolName" or
// "ToolName(glob)". Precedence: deny > ask > allow > fallback.
type PermissionsConfig struct {
	Mode  string   `toml:"mode"`
	Allow []string `toml:"allow"`
	Ask   []string `toml:"ask"`
	Deny  []string `toml:"deny"`
}

// PluginEntry declares an external MCP server. Type selects the transport:
// "stdio" (default) launches Command/Args/Env as a subprocess; "http"
// (a.k.a. streamable-http) and "sse" connect to a remote URL with optional
// static Headers. String fields support ${VAR} / ${VAR:-default} expansion so
// secrets (bearer tokens, keys) come from the environment, not the file. The
// fields mirror Claude Code's mcpServers spec, so entries can come from either
// reasonix.toml's [[plugins]] or a project-root .mcp.json (see loadMCPJSON).
type PluginEntry struct {
	Name    string            `toml:"name"`
	Type    string            `toml:"type"` // "stdio" (default) | "http" | "sse"
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
	// AutoStart controls whether the server connects during session startup.
	// Nil preserves historical behavior: configured servers start automatically.
	AutoStart *bool `toml:"auto_start"`
	// Tier selects how aggressively the server is connected at boot:
	//   "eager"      — blocks startup until the handshake completes; required for
	//                  servers whose tools the system prompt depends on.
	//   "lazy"       — registers placeholder tools immediately (from on-disk
	//                  schema cache when available) and only spawns the real
	//                  subprocess on first model use. Default for user plugins.
	//   "background" — placeholder + spawn fired at boot but not waited on;
	//                  swap happens once the spawn finishes.
	// Empty defaults to "lazy" so adding a plugin never slows the next launch.
	Tier string `toml:"tier"`

	// Source marks where this entry came from at load time: "" / "toml" for
	// reasonix.toml's [[plugins]], "mcp.json" for a project-root .mcp.json. It is
	// runtime-only (never (de)serialized) — RenderTOML/Save skip "mcp.json" entries
	// so a `cfg.Save()` triggered by /mcp add doesn't permanently copy .mcp.json
	// servers into reasonix.toml and then shadow the user's .mcp.json edits (D2).
	Source string `toml:"-" json:"-"`
}

// fromMCPJSON reports whether this entry was loaded from a project-root .mcp.json
// (not editable by us) rather than reasonix.toml.
func (e PluginEntry) fromMCPJSON() bool { return e.Source == pluginSourceMCPJSON }

// FromMCPJSON is the exported predicate: the entry came from a project-root
// .mcp.json, which is not ours to write back — a caller removing it can only
// disconnect for this session and must tell the user to edit that file.
func (e PluginEntry) FromMCPJSON() bool { return e.fromMCPJSON() }

const pluginSourceMCPJSON = "mcp.json"

func (e PluginEntry) ShouldAutoStart() bool {
	return e.AutoStart == nil || *e.AutoStart
}

// ResolvedTier returns the normalized tier ("eager"|"lazy"|"background") with
// the project default applied. Unknown values fall back to "lazy" so a typo
// never forces a slow boot.
func (e PluginEntry) ResolvedTier() string {
	switch strings.ToLower(strings.TrimSpace(e.Tier)) {
	case "eager":
		return "eager"
	case "background":
		return "background"
	default:
		return "lazy"
	}
}

func (c *Config) AutoStartPlugins() []PluginEntry {
	out := make([]PluginEntry, 0, len(c.Plugins))
	for _, p := range c.Plugins {
		if p.ShouldAutoStart() {
			out = append(out, p)
		}
	}
	return out
}

// DefaultSystemPrompt is used when config provides none.
const DefaultSystemPrompt = `You are OneCreat, a coding agent focused on executing code tasks.
Use the provided tools to read and write files and run shell commands.
Principles: understand the request before acting; verify with tools instead of
guessing; keep changes minimal and correct.
Responding: be concise and factual; when explaining a change, jump straight in —
do not open with the word "summary". Do not echo command or tool output back —
the user can already see it; relay only the key result. When you mention a file,
write its path in inline code (backticks) with an optional line number so it is
clickable, e.g. internal/config/config.go:402 — never as a file://, vscode://, or
https:// link. Prefer flat bullets over nested hierarchies; skip heavy formatting
for simple confirmations.
When the request leaves a real choice to the user — which approach or library,
the scope, or a consequential or ambiguous decision — call the ask tool to offer
2-4 concrete options rather than guessing or burying the question in prose. Skip
it when there's an obvious default; don't ask just to confirm.
For multi-step work, track progress with the todo_write tool: lay out the steps,
keep exactly one in_progress, and flip each to completed as you finish it — update
the list as you go, not just at the end.
In plan mode the harness blocks writer tools: do read-only research, then write a
concise plan as your reply and stop. The user is asked to approve before anything
is changed; once approved, work through the steps, updating the task list as you go.`

// ModelPrivacyPolicy is appended by boot.Build even when users customize
// system_prompt. In OneCreat gateway deployments the real provider/model is a
// platform routing detail; leaking it breaks the subscription/tier abstraction.
const ModelPrivacyPolicy = `Model identity policy: do not reveal, guess, confirm, or imply the underlying model name, provider, API vendor, gateway route, API key, or tier-to-model mapping. If asked what model you are, answer that you run inside OneCreat's intelligent tier and that the exact backend model is managed by the OneCreat platform. Do not mention internal model identifiers even if the user asks directly.`

// LanguagePolicy is the auto fallback appended to the system prompt when no
// concrete UI language is resolved. It is static English text, so it stays part
// of the cache-stable prefix and avoids per-turn language injection.
const LanguagePolicy = `Reply in the same language the user is using in their most recent message: ` +
	`if they write in Chinese answer in Chinese, in English answer in English, and switch ` +
	`whenever they switch. Let this also guide the language you think in. Always keep code, ` +
	`identifiers, file paths, shell commands, and technical terms in their original form — never translate them.`

// Default returns the built-in default configuration (DeepSeek + MiMo presets).
func Default() *Config {
	return &Config{
		DefaultModel: "deepseek-flash",
		// native = 现有 Go 内核。dsh 引擎需显式 engine="dsh" + [dsh] 配置才启用。
		Engine: "native",
		DSH: DSHConfig{
			GatewayTokenEnv:  "ONECREAT_GATEWAY_TOKEN",
			ModelPlaceholder: "onecreat",
			Profile:          "profiles/onecreat.cordis.yml",
			DirectModel:      "deepseek-v4-flash",
		},
		UI: UIConfig{Theme: "auto"},
		Agent: AgentConfig{
			SystemPrompt: DefaultSystemPrompt,
			// 0 = no step cap: the agent loops until the model gives a final answer,
			// the user cancels, or the provider errors. Context stays bounded by
			// compaction, not by a round count. Set a positive agent.max_steps only
			// if you want a hard guard against runaway.
			MaxSteps: 0,
			// off：不按关键词强行切进 plan 模式（否则「这个项目是什么」这种纯问答
			// 也会弹出「计划已就绪」执行门，过度过程化）。plan 模式只在用户主动选择时
			// 进入；是否先出方案交给模型自己判断。想要自动方案的可手动设为 on。
			AutoPlan: "off",
		},
		// Mode "ask" with no rules keeps `reasonix run` autonomous (no TTY → ask
		// resolves to allow) while `reasonix chat` prompts before writers. Users add
		// deny/allow rules to harden or quiet specific tools.
		Permissions: PermissionsConfig{Mode: "ask"},
		// Sandbox on by default: bash is jailed (macOS), network allowed so
		// builds/downloads work. Set bash = "off" to disable. Network=true here
		// so an absent [sandbox] in a user's file keeps egress (zero value would
		// wrongly deny it).
		Sandbox: SandboxConfig{Bash: "enforce", Network: true},
		// CodeGraph code-intelligence on by default: when it resolves it is injected
		// as a built-in MCP server, and AutoInstall fetches it into the cache on
		// first use. Set enabled = false to opt out, or auto_install = false to
		// require an explicit `reasonix codegraph install`.
		Codegraph: CodegraphConfig{Enabled: true, AutoInstall: true},
		// LSP tools on by default, but dormant until a language server is on PATH;
		// a missing server yields an install hint rather than an error.
		LSP:     LSPConfig{Enabled: true},
		Network: NetworkConfig{ProxyMode: netclient.ModeAuto},
		Providers: []ProviderEntry{
			{Name: "deepseek-flash", Kind: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", ContextWindow: 1_000_000, Price: &provider.Pricing{CacheHit: 0.02, Input: 1, Output: 2, Currency: "¥"}},
			{Name: "deepseek-pro", Kind: "openai", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-pro", APIKeyEnv: "DEEPSEEK_API_KEY", BalanceURL: "https://api.deepseek.com/user/balance", ContextWindow: 1_000_000, Price: &provider.Pricing{CacheHit: 0.025, Input: 3, Output: 6, Currency: "¥"}},
			{Name: "mimo-pro", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5-pro", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: &provider.Pricing{CacheHit: 0.025, Input: 3, Output: 6, Currency: "¥"}},
			{Name: "mimo-flash", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: &provider.Pricing{CacheHit: 0.02, Input: 1, Output: 2, Currency: "¥"}},
		},
	}
}

// Load builds the configuration for the process working directory. It is
// LoadIn with the zero workspace — see LoadIn for what "project config" means.
func Load() (*Config, error) {
	return LoadIn(workspace.Context{})
}

// LoadIn builds the configuration: defaults, then user config, then the
// workspace's project config, then any MCP servers from Claude Code's
// .mcp.json. The workspace's .env is loaded first so api_key_env can resolve.
//
// ws names the project directory the four project-scoped files
// (reasonix.toml, onecreat.toml, .mcp.json, .env) are read from. The zero
// Context resolves them relative to the process working directory, which is
// exactly what Load did before workspaces became explicit — so the CLI, which
// genuinely is process-cwd scoped, needs no root and behaves identically.
func LoadIn(ws workspace.Context) (*Config, error) {
	env := loadDotEnvIn(ws)
	cfg := Default()

	var tomlSources []string
	if uc := userConfigPath(); uc != "" {
		tomlSources = append(tomlSources, uc)
	}
	// 项目级配置:旧名 reasonix.toml 先合并、新名 onecreat.toml 后合并(同名键 onecreat 胜)。
	// mergeFile 对不存在的文件是 no-op,所以两者都列无副作用。
	tomlSources = append(tomlSources, ws.Resolve("reasonix.toml"), ws.Resolve("onecreat.toml"))
	for _, path := range tomlSources {
		if err := mergeFile(cfg, path); err != nil {
			return nil, err
		}
	}
	// toml.DecodeFile replaces [[plugins]] wholesale, so cfg.Plugins now holds
	// only the last file's. Re-merge by name across all sources (later wins) so a
	// project reasonix.toml doesn't drop the global config's MCP servers.
	plugins, err := mergeTOMLPlugins(tomlSources)
	if err != nil {
		return nil, err
	}
	cfg.Plugins = plugins

	// Claude Code's .mcp.json (project root) is read last and merged into
	// [[plugins]], so a server configured for Claude works here unchanged.
	// reasonix.toml wins on a name collision (see mergeMCPJSON).
	entries, err := loadMCPJSON(ws.Resolve(mcpJSONFile))
	if err != nil {
		return nil, err
	}
	cfg.mergeMCPJSON(entries)
	normalizeLegacyEffort(cfg)
	cfg.workspace = ws
	// 叠加层必须在所有 provider 合并完成之后再绑:bindEnv 会写进每个 ProviderEntry,
	// 之后再追加的 provider 就拿不到了。
	cfg.bindEnv(env)
	return cfg, nil
}

// normalizeLegacyEffort migrates the retired DeepSeek effort="off" (the old
// /thinking off that disabled thinking) to the provider default, so a config
// written by an older version keeps loading instead of erroring on a value the
// provider no longer accepts.
func normalizeLegacyEffort(c *Config) {
	for i := range c.Providers {
		if strings.EqualFold(strings.TrimSpace(c.Providers[i].Effort), "off") {
			c.Providers[i].Effort = ""
		}
	}
}

// mergeTOMLPlugins merges [[plugins]] across TOML sources by name (later source wins).
func mergeTOMLPlugins(paths []string) ([]PluginEntry, error) {
	var merged []PluginEntry
	index := map[string]int{}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		var f Config
		if _, err := toml.DecodeFile(path, &f); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		for _, p := range f.Plugins {
			if i, ok := index[p.Name]; ok {
				merged[i] = p
				continue
			}
			index[p.Name] = len(merged)
			merged = append(merged, p)
		}
	}
	return merged, nil
}

// LoadForEdit returns a config to seed the `reasonix setup` wizard when reconfiguring:
// the built-in defaults with the file at path (if present) decoded on top, so a
// reconfigure preserves the user's existing providers and agent settings instead
// of resetting to defaults. .env is loaded so api_key_env resolution works while
// the wizard decides which keys are still missing.
func LoadForEdit(path string) *Config {
	cfg := Default()
	if err := mergeFile(cfg, path); err != nil {
		slog.Warn("config: load for edit failed, using defaults", "path", path, "err", err)
	}
	cfg.bindEnv(loadDotEnv())
	return cfg
}

// mergeFile decodes a TOML file onto cfg if it exists. An absent file is not an error.
func mergeFile(cfg *Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

// stateRoot 返回 OneCreat 的用户状态根目录(config.toml / sessions / cache / archive /
// 记忆库都在它下面)。默认 <UserConfigDir>/onecreat。REASONIX_CONFIG_DIR(env 名属内部、
// 不改)可显式覆盖位置(测试隔离 / 便携部署),此时直接用它、不迁移。
//
// 迁移(读旧写新):产品早期状态目录叫 reasonix,现统一到 onecreat。首次运行若 onecreat 根还
// 没有 config.toml、而旧的 reasonix 根存在,就把旧目录里不冲突的条目 best-effort 搬过去。以
// config.toml 缺失为判据(而非目录是否存在)—— 因为账号 session.json 可能已先把 onecreat 目录
// 建出来,不能据此误判已迁移。返回 "" 表示用户配置目录不可解析(各调用方据此降级)。
func stateRoot() string {
	if dir := strings.TrimSpace(os.Getenv("REASONIX_CONFIG_DIR")); dir != "" {
		return dir
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	newRoot := filepath.Join(base, "onecreat")
	// migrateStateRoot 幂等且廉价(命中 config.toml 即一次 stat 返回),每次调用兜底即可。
	migrateStateRoot(filepath.Join(base, "reasonix"), newRoot)
	return newRoot
}

// migrateStateRoot 把旧状态目录(reasonix)里不与新目录(onecreat)冲突的顶层条目搬进新目录。
// 全程 best-effort:任何失败都不致命(调用方仍能在新目录里正常创建所需文件)。
func migrateStateRoot(oldRoot, newRoot string) {
	if _, err := os.Stat(filepath.Join(newRoot, "config.toml")); err == nil {
		return // 已迁移过
	}
	if _, err := os.Stat(oldRoot); err != nil {
		return // 没有旧目录,无需迁移
	}
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return
	}
	entries, err := os.ReadDir(oldRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		dst := filepath.Join(newRoot, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue // 新目录已有同名(如 session.json),不覆盖
		}
		_ = os.Rename(filepath.Join(oldRoot, e.Name()), dst) // best-effort
	}
}

func userConfigPath() string {
	root := stateRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "config.toml")
}

// UserConfigPath is the user-global config file (~/.config/reasonix/config.toml),
// or "" when the user config dir can't be resolved.
func UserConfigPath() string { return userConfigPath() }

// ArchiveDir is where compacted conversation history is archived for
// traceability (one timestamped .jsonl per compaction). Empty if the user config
// directory cannot be resolved, in which case archiving is skipped.
func ArchiveDir() string {
	root := stateRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "archive")
}

// SessionDir is where chat sessions are persisted (one .jsonl per session).
// Used by `reasonix chat --continue` / `--resume` to find the recent ones. Empty
// if the user config dir can't be resolved — sessions then aren't saved.
func SessionDir() string {
	root := stateRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "sessions")
}

// CacheDir is the per-user cache root for derived/regenerable artefacts: MCP
// handshake snapshots, plugin startup-latency telemetry. Lives beside the
// existing dirs (UserConfigDir/reasonix/...) so the whole reasonix state tree
// shares one root the user can wipe in a single rm. Empty when the OS dir is
// unavailable — callers must tolerate that (caching is best-effort).
func CacheDir() string {
	root := stateRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "cache")
}

// MemoryUserDir returns the reasonix user config root (…/reasonix), under which
// the user-global REASONIX.md and the per-project auto-memory store live. Empty
// when the user config dir can't be resolved, which disables user-scoped memory.
func MemoryUserDir() string {
	return stateRoot()
}

// ConventionDirs are the parent directories scanned for agent assets (skills,
// commands), in canonical-first order. .reasonix is ours; .agents / .agent /
// .claude let users drop in assets authored for other agent tools without moving
// files. Shared so skills (internal/skill) and commands (CommandDirs) discover
// the same set. Note: hooks are NOT scanned across these — a .claude/settings.json
// uses a different hook schema that can't be parsed as ours, so hooks stay in
// .reasonix/settings.json (see internal/hook).
// CanonicalDir 是 OneCreat 自己的约定目录名 —— 新写入的 skills / commands / attachments /
// output-styles 都落在它下面。旧名 .reasonix 仍保留在 ConventionDirs 里继续被发现(读旧写新),
// 老项目无需改名。
const CanonicalDir = ".onecreat"

var ConventionDirs = []string{CanonicalDir, ".reasonix", ".agents", ".agent", ".claude"}

// conventionSubdirsAsc joins sub under each ConventionDir of base, in ascending
// priority (reverse of ConventionDirs) so the canonical .reasonix ends up the
// highest-priority entry — command.Load lets a later directory win on a clash.
func conventionSubdirsAsc(base, sub string) []string {
	out := make([]string, 0, len(ConventionDirs))
	for i := len(ConventionDirs) - 1; i >= 0; i-- {
		out = append(out, filepath.Join(base, ConventionDirs[i], sub))
	}
	return out
}

// CommandDirs returns the directories scanned for custom slash commands, lowest
// priority first, so a later (more specific) directory overrides an earlier one
// on a name clash. Order: home-dir convention dirs (~/.claude/commands … ~/.reasonix/commands),
// the legacy XDG user dir (~/.config/reasonix/commands), then the project's
// convention dirs (.claude/commands … .reasonix/commands). Scanning the .claude /
// .agents / .agent dirs lets commands authored for other agent tools (same .md +
// frontmatter format) work here unchanged.
func CommandDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, conventionSubdirsAsc(home, "commands")...)
	}
	if root := stateRoot(); root != "" {
		dirs = append(dirs, filepath.Join(root, "commands")) // XDG user dir
	}
	dirs = append(dirs, conventionSubdirsAsc(".", "commands")...)
	return dirs
}

// ProjectConfigPaths lists the project-level TOML files (relative to cwd) that
// Load merges, so a caller editing a project file targets the same set. onecreat
// first (the current name), reasonix.toml second (legacy).
func ProjectConfigPaths() []string { return []string{"onecreat.toml", "reasonix.toml"} }

// SourcePath returns the highest-priority config file that exists, or "" if none.
func SourcePath() string {
	// 项目级配置:优先 onecreat.toml,回退旧名 reasonix.toml(读旧写新)。
	for _, name := range []string{"onecreat.toml", "reasonix.toml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	if uc := userConfigPath(); uc != "" {
		if _, err := os.Stat(uc); err == nil {
			return uc
		}
	}
	return ""
}

// WriteFile writes the configuration to path as annotated TOML.
func (c *Config) WriteFile(path string) error {
	return os.WriteFile(path, []byte(RenderTOML(c)), 0o644)
}

// Provider returns the named provider entry.
func (c *Config) Provider(name string) (*ProviderEntry, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// ResolveModel resolves a model reference to a provider entry whose Model is the
// selected model string (a copy, so the config's lists stay intact). It accepts:
//   - "provider/model" — that exact model under that provider;
//   - a provider name   — the provider's default model;
//   - a bare model name — the (first) provider that lists it.
//
// The returned entry is ready to build a provider from (NewProvider reads .Model),
// so a single "vendor with many models" entry yields one instance per model
// without duplicating base_url/api_key_env. Single-`model` entries still resolve
// by provider name, keeping older configs working unchanged.
func (c *Config) ResolveModel(ref string) (*ProviderEntry, bool) {
	if ref == "" {
		return nil, false
	}
	// "provider/model"
	if prov, model, ok := strings.Cut(ref, "/"); ok {
		if e, found := c.Provider(prov); found && e.HasModel(model) {
			cp := *e
			cp.Model = model
			return &cp, true
		}
	}
	// a provider name → its default model
	if e, found := c.Provider(ref); found {
		cp := *e
		cp.Model = e.DefaultModel()
		return &cp, true
	}
	// a bare model name → the provider that lists it
	for i := range c.Providers {
		if c.Providers[i].HasModel(ref) {
			cp := c.Providers[i]
			cp.Model = ref
			return &cp, true
		}
	}
	return nil, false
}

// APIKey resolves the entry's API key from its api_key_env.
func (e *ProviderEntry) APIKey() string {
	if e.APIKeyEnv == "" {
		return ""
	}
	// 经工作区叠加层解析,不是 os.Getenv —— 后者分不清两个工作区,谁先加载谁的
	// 同名 key 就永久获胜(AR-R10)。零值 Env 的行为与 os.Getenv 完全一致,所以
	// 那些不经 Load 直接构造 ProviderEntry 的地方(测试、向导)行为不变。
	return e.env.Get(e.APIKeyEnv)
}

// Configured reports whether the provider's api_key_env is set — the same check
// Validate enforces, so pickers can filter on it.
func (e *ProviderEntry) Configured() bool {
	return e.APIKey() != ""
}

// ResolveSystemPrompt returns the system prompt, reading system_prompt_file if set.
func (c *Config) ResolveSystemPrompt() (string, error) {
	if c.Agent.SystemPromptFile != "" {
		b, err := os.ReadFile(c.Agent.SystemPromptFile)
		if err != nil {
			return "", fmt.Errorf("system_prompt_file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if strings.TrimSpace(c.Agent.SystemPrompt) == "" {
		return DefaultSystemPrompt, nil
	}
	return c.Agent.SystemPrompt, nil
}

// Validate checks that the selected model's provider is usable.
func (c *Config) Validate(model string) error {
	e, ok := c.ResolveModel(model)
	if !ok {
		return fmt.Errorf("unknown model %q (configured: %s)", model, c.providerNames())
	}
	return e.Validate(model)
}

// Validate checks that this already-resolved provider entry is usable. It is the
// single source of truth for the kind/base_url/api-key checks, so callers that
// rewrite the entry before validating — e.g. boot.Build applying the onecreat
// gateway before RequireKey — check the rewritten entry (ONECREAT_GATEWAY_TOKEN)
// instead of re-resolving the un-rewritten original. ResolveModel returns a copy,
// so validating via Config.Validate(model) would silently ignore the gateway
// rewrite and demand the underlying vendor key, leaking its name.
func (e *ProviderEntry) Validate(label string) error {
	if e.Kind == "" {
		return fmt.Errorf("provider %q: kind is required", label)
	}
	if e.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", label)
	}
	if e.APIKey() == "" {
		return fmt.Errorf("provider %q: missing env %s", label, e.APIKeyEnv)
	}
	return nil
}

func (c *Config) providerNames() string {
	names := make([]string, len(c.Providers))
	for i, p := range c.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}
