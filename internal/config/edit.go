package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"reasonix/internal/fileutil"
	"reasonix/internal/netclient"
	"reasonix/internal/permission"
)

// edit.go is the programmatic mutation surface a settings UI drives: change the
// default model, add/remove a provider, set the planner, edit permission rules,
// add/remove an MCP server — each validated, then persisted with SaveTo. It is
// separate from the `reasonix setup` wizard (cli) so a GUI can apply one setting at a
// time without replaying the whole interactive flow. Every mutator works on the
// in-memory *Config; nothing writes to disk until SaveTo/Save is called, so a UI
// can stage several changes and commit once. Mutations round-trip through
// RenderTOML → Load (the wizard relies on the same guarantee).

// permission rule list names accepted by the rule mutators.
const (
	listAllow = "allow"
	listAsk   = "ask"
	listDeny  = "deny"
)

// SetDefaultModel points default_model at an existing provider. It errors if no
// provider by that name is configured, so a UI can't strand the config on a
// model that doesn't exist.
func (c *Config) SetDefaultModel(name string) error {
	if _, ok := c.Provider(name); !ok {
		return fmt.Errorf("set default: no provider %q (configured: %s)", name, c.providerNames())
	}
	c.DefaultModel = name
	return nil
}

// SetPlannerModel sets (or, with "", clears) agent.planner_model for two-model
// collaboration. A non-empty name must be a configured provider.
func (c *Config) SetPlannerModel(name string) error {
	if name == "" {
		c.Agent.PlannerModel = ""
		return nil
	}
	if _, ok := c.Provider(name); !ok {
		return fmt.Errorf("set planner: no provider %q (configured: %s)", name, c.providerNames())
	}
	c.Agent.PlannerModel = name
	return nil
}

// UpsertProvider adds e, or replaces an existing provider with the same name
// (preserving its position). Required fields (name, kind, base_url, model) are
// validated; whether the kind is actually registered and the key resolves is
// checked later by provider.New / Validate, which give actionable errors.
func (c *Config) UpsertProvider(e ProviderEntry) error {
	if err := validateProvider(e); err != nil {
		return err
	}
	for i := range c.Providers {
		if c.Providers[i].Name == e.Name {
			c.Providers[i] = e
			return nil
		}
	}
	c.Providers = append(c.Providers, e)
	return nil
}

// SetProviderEffort updates a provider's provider-specific thinking effort knob.
func (c *Config) SetProviderEffort(name, effort string) error {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			c.Providers[i].Effort = strings.ToLower(strings.TrimSpace(effort))
			return nil
		}
	}
	return fmt.Errorf("set provider effort: no provider %q", name)
}

// SetProviderThinking updates a provider's provider-specific thinking mode knob.
func (c *Config) SetProviderThinking(name, thinking string) error {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			c.Providers[i].Thinking = strings.ToLower(strings.TrimSpace(thinking))
			return nil
		}
	}
	return fmt.Errorf("set provider thinking: no provider %q", name)
}

// SetNetwork updates ordinary outbound network proxy settings. Invalid custom
// proxy settings are rejected here so the desktop panel cannot save a config that
// would break provider startup.
func (c *Config) SetNetwork(n NetworkConfig) error {
	n.ProxyMode = netclient.NormalizeMode(n.ProxyMode)
	n.ProxyURL = strings.TrimSpace(n.ProxyURL)
	n.NoProxy = strings.TrimSpace(n.NoProxy)
	n.Proxy.Type = strings.ToLower(strings.TrimSpace(n.Proxy.Type))
	n.Proxy.Server = strings.TrimSpace(n.Proxy.Server)
	n.Proxy.Username = strings.TrimSpace(n.Proxy.Username)
	c.Network = n
	return netclient.Validate(c.NetworkProxySpec())
}

// RemoveProvider deletes the named provider. It refuses to remove the current
// default_model (reassign it first, so the config never points at a missing
// model); if the removed provider was the planner, planner_model is cleared as
// a side effect since it is optional. Errors when the name isn't configured.
func (c *Config) RemoveProvider(name string) error {
	idx := -1
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("remove provider: no provider %q", name)
	}
	if c.DefaultModel == name {
		return fmt.Errorf("remove provider: %q is the default model — set a different default_model first", name)
	}
	c.Providers = append(c.Providers[:idx], c.Providers[idx+1:]...)
	if c.Agent.PlannerModel == name {
		c.Agent.PlannerModel = ""
	}
	return nil
}

// validateProvider checks the fields a provider can't function without.
func validateProvider(e ProviderEntry) error {
	switch {
	case strings.TrimSpace(e.Name) == "":
		return fmt.Errorf("provider: name is required")
	case strings.TrimSpace(e.Kind) == "":
		return fmt.Errorf("provider %q: kind is required", e.Name)
	case strings.TrimSpace(e.BaseURL) == "":
		return fmt.Errorf("provider %q: base_url is required", e.Name)
	case strings.TrimSpace(e.Model) == "":
		return fmt.Errorf("provider %q: model is required", e.Name)
	}
	return nil
}

// SetPermissionMode sets the writer-fallback mode. Accepts "ask", "allow", or
// "deny" (case-insensitive); anything else errors rather than silently
// defaulting, so a UI surfaces a typo instead of installing a surprising mode.
func (c *Config) SetPermissionMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ask", "allow", "deny":
		c.Permissions.Mode = strings.ToLower(strings.TrimSpace(mode))
		return nil
	default:
		return fmt.Errorf("permission mode %q: must be ask|allow|deny", mode)
	}
}

// AddPermissionRule appends a rule ("ToolName" or "ToolName(glob)") to the
// allow / ask / deny list. The rule is validated with the same parser the gate
// uses, and a duplicate is a no-op so a UI can call it idempotently.
func (c *Config) AddPermissionRule(list, rule string) error {
	target, err := c.ruleList(list)
	if err != nil {
		return err
	}
	rule = strings.TrimSpace(rule)
	if _, ok := permission.ParseRule(rule); !ok {
		return fmt.Errorf("invalid permission rule %q (want \"ToolName\" or \"ToolName(glob)\")", rule)
	}
	for _, existing := range *target {
		if existing == rule {
			return nil // already present
		}
	}
	*target = append(*target, rule)
	return nil
}

// RemovePermissionRule drops the first exact match of rule from the named list,
// reporting whether anything was removed.
func (c *Config) RemovePermissionRule(list, rule string) (bool, error) {
	target, err := c.ruleList(list)
	if err != nil {
		return false, err
	}
	rule = strings.TrimSpace(rule)
	for i, existing := range *target {
		if existing == rule {
			*target = append((*target)[:i], (*target)[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// ruleList returns a pointer to the named rule slice so mutators can append to
// it in place. An unknown list name errors.
func (c *Config) ruleList(list string) (*[]string, error) {
	switch strings.ToLower(strings.TrimSpace(list)) {
	case listAllow:
		return &c.Permissions.Allow, nil
	case listAsk:
		return &c.Permissions.Ask, nil
	case listDeny:
		return &c.Permissions.Deny, nil
	default:
		return nil, fmt.Errorf("unknown permission list %q (want allow|ask|deny)", list)
	}
}

// AddSkillPath appends a custom skill root, deduping by its expanded absolute
// path while preserving the caller's original spelling in the config file.
func (c *Config) AddSkillPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	for _, existing := range c.Skills.Paths {
		if CanonicalSkillPath(existing) == want {
			return nil
		}
	}
	c.Skills.Paths = append(c.Skills.Paths, path)
	return nil
}

// RemoveSkillPath removes the first custom skill root matching path after
// expansion and path cleaning. It reports whether anything changed.
func (c *Config) RemoveSkillPath(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	for i, existing := range c.Skills.Paths {
		if CanonicalSkillPath(existing) == want {
			c.Skills.Paths = append(c.Skills.Paths[:i], c.Skills.Paths[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// CanonicalSkillPath expands env vars, ~ and relative segments to an absolute
// cleaned path for comparing skill roots. On Windows it folds case so paths that
// differ only in casing dedupe. Use only for comparison, never as stored config.
func CanonicalSkillPath(path string) string {
	path = ExpandVars(strings.TrimSpace(path))
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// UpsertPlugin adds e, or replaces an MCP server with the same name (preserving
// position). The transport-specific required fields are validated: stdio needs
// a command, http/sse need a url.
func (c *Config) UpsertPlugin(e PluginEntry) error {
	if err := validatePlugin(e); err != nil {
		return err
	}
	for i := range c.Plugins {
		if c.Plugins[i].Name == e.Name {
			c.Plugins[i] = e
			return nil
		}
	}
	c.Plugins = append(c.Plugins, e)
	return nil
}

// RemovePlugin deletes the named MCP server, reporting whether it was present.
func (c *Config) RemovePlugin(name string) bool {
	for i := range c.Plugins {
		if c.Plugins[i].Name == name {
			c.Plugins = append(c.Plugins[:i], c.Plugins[i+1:]...)
			return true
		}
	}
	return false
}

// validatePlugin checks a plugin entry by transport. An empty Type means stdio.
func validatePlugin(e PluginEntry) error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("plugin: name is required")
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "", "stdio":
		if strings.TrimSpace(e.Command) == "" {
			return fmt.Errorf("plugin %q: command is required for a stdio server", e.Name)
		}
	case "http", "sse", "streamable-http":
		if strings.TrimSpace(e.URL) == "" {
			return fmt.Errorf("plugin %q: url is required for a %s server", e.Name, e.Type)
		}
	default:
		return fmt.Errorf("plugin %q: unknown type %q (want stdio|http|sse)", e.Name, e.Type)
	}
	return nil
}

// SaveTo writes the configuration to path as annotated TOML, atomically: it
// writes a sibling temp file then renames, so a crash mid-write can't leave a
// half-written reasonix.toml that fails to parse on next load. Parent directories
// are created as needed.
func (c *Config) SaveTo(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("save: empty config path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("save: create dir: %w", err)
	}
	rendered := RenderTOML(c)
	// 原子 rename 前先校验渲染结果能被同一个解码器解析回来:防 H5 —— 任何字段(尤其
	// system_prompt 含 """ 或末尾反斜杠)若写出非法 TOML,这里直接拒绝写入并报错,旧配置
	// 原封不动,绝不把配置文件写 brick(否则下次 Load 失败、app/CLI 起不来)。
	var probe Config
	if _, err := toml.Decode(rendered, &probe); err != nil {
		return fmt.Errorf("save: 渲染出的配置不是合法 TOML,已拒绝写入以避免损坏配置文件: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".reasonix.*.toml.tmp")
	if err != nil {
		return fmt.Errorf("save: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(rendered); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("save: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("save: close temp: %w", err)
	}
	return fileutil.ReplaceFile(tmpPath, path)
}

// RemovePluginFromFile 从 path 指向的单个 TOML 文件里【外科式】删掉 name 匹配的那个
// [[plugins]] 块(含它的 [plugins.*] 子表、以及紧贴其上无空行间隔的注释行),文件其余内容
// 保持原样——不经 RenderTOML 整份重渲染。这样绝不会把 Default() 的 provider/default_model
// 等无关键写进项目文件(那正是 1b 全量快照遮蔽用户配置的根因)。返回是否删到了条目;写盘前
// 用同一个解码器校验结果仍是合法 TOML,再原子替换。文件不存在或没有该条目 → (false, nil)。
func RemovePluginFromFile(path, name string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := strings.Split(string(raw), "\n")

	// 定位 name 匹配的顶层 [[plugins]] 块的行区间 [start,end)。
	start, end := -1, -1
	for i := 0; i < len(lines); i++ {
		if !isPluginsArrayHeader(lines[i]) {
			continue
		}
		j := i + 1
		for j < len(lines) && !endsPluginBlock(lines[j]) {
			j++
		}
		if pluginBlockName(lines[i:j]) == name {
			start, end = i, j
			break
		}
		i = j - 1
	}
	if start < 0 {
		return false, nil
	}
	// 紧贴块头之上、无空行间隔的注释行是这个块的注解,一并删掉。
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}

	kept := make([]string, 0, len(lines)-(end-start))
	kept = append(kept, lines[:start]...)
	kept = append(kept, lines[end:]...)
	result := strings.Join(kept, "\n")

	// 写盘前校验删除后仍是合法 TOML,避免把项目配置改坏。
	var probe Config
	if _, derr := toml.Decode(result, &probe); derr != nil {
		return false, fmt.Errorf("remove plugin %q from %s: 删除后不是合法 TOML,已放弃写入: %w", name, path, derr)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reasonix.*.toml.tmp")
	if err != nil {
		return false, fmt.Errorf("remove plugin: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(result); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return false, fmt.Errorf("remove plugin: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("remove plugin: close temp: %w", err)
	}
	if err := fileutil.ReplaceFile(tmpPath, path); err != nil {
		return false, err
	}
	return true, nil
}

// PluginRemoveOutcome reports which file (if any) RemovePluginPersisted edited, so
// callers can message the user precisely.
type PluginRemoveOutcome int

const (
	PluginNotFound       PluginRemoveOutcome = iota // 三处都没有
	PluginRemovedUser                               // 从用户级 config.toml 删除
	PluginRemovedProject                            // 从项目 onecreat.toml/reasonix.toml 外科式删除
	PluginFromMCPJSON                               // 声明在 .mcp.json,不由我们写回(未删)
)

// AddPluginPersisted upserts e into the【user-level】config and saves it there —
// never the project-level SourcePath, whose full snapshot would shadow the user's
// settings (the 1b regression). Shared persistence path for the desktop AddMCPServer
// and the `reasonix mcp add` CLI, so the two can't drift.
func AddPluginPersisted(e PluginEntry) error {
	path := UserConfigPath()
	if path == "" {
		return fmt.Errorf("cannot resolve user config path")
	}
	cfg := LoadForEdit(path)
	if err := cfg.UpsertPlugin(e); err != nil {
		return err
	}
	return cfg.SaveTo(path)
}

// RemovePluginPersisted removes the named MCP server from persisted config,
// honoring its source: user-level toml → edit the user file; project-level
// onecreat.toml/reasonix.toml → surgical [[plugins]] block removal (never a full
// snapshot); .mcp.json → not ours to edit (reported, not removed). Shared by the
// desktop RemoveMCPServer and the `reasonix mcp remove` CLI.
func RemovePluginPersisted(name string) (PluginRemoveOutcome, error) {
	// .mcp.json 声明的服务器不由我们写回。
	if cfg, err := Load(); err == nil {
		for _, p := range cfg.Plugins {
			if p.Name == name {
				if p.FromMCPJSON() {
					return PluginFromMCPJSON, nil
				}
				break
			}
		}
	}
	// 先试用户级(与 AddPluginPersisted 对称,不写项目级全量快照)。
	userPath := UserConfigPath()
	if userPath == "" {
		return PluginNotFound, fmt.Errorf("cannot resolve user config path")
	}
	userCfg := LoadForEdit(userPath)
	if userCfg.RemovePlugin(name) {
		if err := userCfg.SaveTo(userPath); err != nil {
			return PluginNotFound, err
		}
		return PluginRemovedUser, nil
	}
	// 不在用户级 → 声明在项目 toml:外科式删块。
	removed := false
	for _, projPath := range ProjectConfigPaths() {
		r, err := RemovePluginFromFile(projPath, name)
		if err != nil {
			return PluginNotFound, err
		}
		removed = removed || r
	}
	if removed {
		return PluginRemovedProject, nil
	}
	return PluginNotFound, nil
}

// isPluginsArrayHeader 判断一行是否是数组表头 [[plugins]](允许行首空白与行尾注释)。
func isPluginsArrayHeader(line string) bool {
	t := strings.TrimSpace(line)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t == "[[plugins]]"
}

// endsPluginBlock 判断一行是否终结当前 [[plugins]] 块:遇到下一个顶层表头即终结,但
// [plugins.xxx] / [[plugins.xxx]] 子表仍属于当前块,不终结。
func endsPluginBlock(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return false
	}
	if strings.HasPrefix(t, "[plugins.") || strings.HasPrefix(t, "[[plugins.") {
		return false
	}
	return true
}

// pluginBlockName 从一个 [[plugins]] 块的行里取出顶层 name 值("" 表示没写 name)。
func pluginBlockName(block []string) string {
	for _, ln := range block {
		t := strings.TrimSpace(ln)
		rest := strings.TrimPrefix(t, "name")
		if rest == t { // 该行不以 name 开头
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		return unquoteTOMLValue(strings.TrimSpace(rest[1:]))
	}
	return ""
}

// unquoteTOMLValue 取 TOML 基本/字面字符串的内容(去引号、忽略行尾注释),仅用于读 name。
func unquoteTOMLValue(s string) string {
	if len(s) < 2 {
		return ""
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return ""
	}
	if end := strings.IndexByte(s[1:], q); end >= 0 {
		return s[1 : 1+end]
	}
	return ""
}

// Save writes the configuration back to the file it was loaded from
// (SourcePath), or to ./onecreat.toml when none exists yet — the conventional
// project-local target a fresh GUI session would create. SourcePath still finds a
// legacy ./reasonix.toml, so an existing project keeps writing its old file (read
// old / write new); only brand-new projects get onecreat.toml.
func (c *Config) Save() error {
	path := SourcePath()
	if path == "" {
		path = "onecreat.toml"
	}
	return c.SaveTo(path)
}
