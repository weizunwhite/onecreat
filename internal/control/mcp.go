package control

// mcpService owns this session's MCP servers: the plugin host, the live tool
// registry the executor reads each turn, and the session-scoped context a
// hot-added stdio server binds its subprocess to.
//
// Split out of control.Controller in Plan 07. The three fields are one unit —
// connecting a server means adding to the host *and* replacing that server's
// tools in the registry, and a hot-added stdio server's lifetime is the session
// context — so they were never independently useful.
//
// Note the host pointer is created lazily on first connect and is not guarded by
// a mutex. That matches the pre-split behaviour and the actual usage (connects
// come from a frontend command path, serialised by the UI), but it is a real
// constraint: do not start calling these from a turn goroutine without adding
// one.

import (
	"context"
	"fmt"
	"os"

	"reasonix/internal/codegraph"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
	"reasonix/internal/tool"
)

type mcpService struct {
	// sink carries the one user-facing notice this service emits: a server
	// declared in a project-root .mcp.json can be disconnected for the session but
	// not removed, since we never write that file back.
	sink event.Sink

	host *plugin.Host
	reg  *tool.Registry
	// ctx is the session-scoped context a hot-added stdio server binds its
	// subprocess to, so the server dies with the session.
	ctx context.Context
	// wsRoot is the project directory workspace-scoped subprocesses (CodeGraph)
	// are pinned to.
	wsRoot string
}

func newMCPService(sink event.Sink, host *plugin.Host, reg *tool.Registry, ctx context.Context, wsRoot string) *mcpService {
	return &mcpService{sink: sink, host: host, reg: reg, ctx: ctx, wsRoot: wsRoot}
}

// Host exposes the plugin host for status readouts.
func (m *mcpService) Host() *plugin.Host { return m.host }

// AddMCPServer connects an MCP server live and persists it to the config file. Its
// tools are registered immediately and become available on the next turn (the
// agent reads the registry per turn). The raw entry — ${VARS} intact — is what's
// written to disk; the live connection uses the expanded form. Returns the number
// of tools the server exposed. A save failure after a successful connect is
// reported but non-fatal: the server still works this session.
func (m *mcpService) AddAndSave(e config.PluginEntry) (int, error) {
	n, err := m.connectEntry(e)
	if err != nil {
		return 0, err
	}
	// 持久化到【用户级】配置(和桌面设置面板 applyConfigChange 同一层单文件编辑),而不是
	// SourcePath 偏好的项目级 onecreat.toml。写项目级会把全量合并快照落进项目文件,之后
	// 遮蔽用户级配置的所有修改(默认模型 / provider / system_prompt)静默失效。项目级 MCP
	// 隔离应走 .mcp.json,不经这里。
	path := config.UserConfigPath()
	if path == "" {
		return n, fmt.Errorf("connected, but cannot resolve user config path to save")
	}
	cfg := config.LoadForEdit(path)
	if err := cfg.UpsertPlugin(e); err != nil {
		return n, fmt.Errorf("connected, but config rejected the entry: %w", err)
	}
	if err := cfg.SaveTo(path); err != nil {
		return n, fmt.Errorf("connected, but saving config failed: %w", err)
	}
	return n, nil
}

// ConnectMCPServer connects an MCP server for this controller only. It does not
// write the entry to config, so UI affordances such as a one-click hardware
// assistant can expose tools to the current conversation without changing the
// user's persistent MCP list.
func (m *mcpService) Connect(e config.PluginEntry) (int, error) {
	return m.connectEntry(e)
}

func (m *mcpService) connectEntry(e config.PluginEntry) (int, error) {
	exp := e.ExpandedPlugin()
	return m.connectSpec(plugin.Spec{
		Name:    exp.Name,
		Type:    exp.Type,
		Command: exp.Command,
		Args:    exp.Args,
		Env:     exp.Env,
		URL:     exp.URL,
		Headers: exp.Headers,
	})
}

func (m *mcpService) connectSpec(s plugin.Spec) (int, error) {
	if m.host == nil {
		m.host = plugin.NewHost()
	}
	tools, err := m.host.Add(m.ctx, s)
	if err != nil {
		return 0, err
	}
	if m.reg != nil {
		m.reg.RemovePrefix(plugin.ToolPrefix(s.Name))
		for _, t := range tools {
			m.reg.Add(t)
		}
	}
	return len(tools), nil
}

func (m *mcpService) ConfiguredNames() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Plugins))
	for _, p := range cfg.Plugins {
		names = append(names, p.Name)
	}
	return names
}

func (m *mcpService) DisconnectedNames() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	connected := map[string]bool{}
	if m.host != nil {
		for _, name := range m.host.ServerNames() {
			connected[name] = true
		}
	}
	var names []string
	for _, p := range cfg.Plugins {
		if !connected[p.Name] {
			names = append(names, p.Name)
		}
	}
	return names
}

func (m *mcpService) ConnectConfigured(name string) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, err
	}
	for _, p := range cfg.Plugins {
		if p.Name == name {
			return m.connectEntry(p)
		}
	}
	if name == "codegraph" {
		return m.ConnectCodegraph(cfg)
	}
	return 0, fmt.Errorf("no configured MCP server named %q", name)
}

func (m *mcpService) ConnectCodegraph(cfg *config.Config) (int, error) {
	if !cfg.Codegraph.Enabled {
		return 0, fmt.Errorf("codegraph is disabled in config")
	}
	bin, ok := codegraph.Resolve(cfg.Codegraph.Path)
	if !ok {
		return 0, fmt.Errorf("codegraph is not installed")
	}
	// Pin the daemon to this session's workspace, not the process working
	// directory: with several projects open at once they are not the same, and
	// indexing the wrong tree is silent and expensive.
	root := m.wsRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return 0, err
		}
		root = wd
	}
	if err := codegraph.EnsureInit(m.ctx, bin, root); err != nil {
		return 0, fmt.Errorf("codegraph init: %w", err)
	}
	return m.connectSpec(plugin.Spec{Name: "codegraph", Command: bin, Args: []string{"serve", "--mcp"}, Dir: root})
}

// RemoveMCPServer disconnects a live MCP server — its tools vanish from the next
// turn — and removes it from the config file. It reports whether a live server was
// disconnected; an error only when the name is neither connected nor in config (or
// the config save fails). The persistence target follows the entry's source:
// user-level toml → edit the user file; project-level onecreat.toml/reasonix.toml →
// surgically drop just that [[plugins]] block from the project file (never a full
// snapshot, which would shadow the user's config — the 1b regression); .mcp.json →
// disconnect for this session only and tell the user to edit that file (not ours).
func (m *mcpService) RemoveAndSave(name string) (disconnected bool, err error) {
	if m.host != nil {
		if prefix, ok := m.host.Remove(name); ok {
			disconnected = true
			if m.reg != nil {
				m.reg.RemovePrefix(prefix)
			}
		}
	}

	// 持久化按来源分流,走与 `reasonix mcp remove` CLI 共享的同一函数(消灭第二份实现)。
	outcome, perr := config.RemovePluginPersisted(name)
	if perr != nil {
		return disconnected, perr
	}
	switch outcome {
	case config.PluginFromMCPJSON:
		// .mcp.json 不由我们写回。本会话已断开,发 notice 告知需手动编辑,不再静默复活。
		if !disconnected && m.reg != nil {
			m.reg.RemovePrefix(plugin.ToolPrefix(name))
		}
		m.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: fmt.Sprintf("已断开 %q(本会话);它声明在项目根 .mcp.json 里,需手动编辑该文件才能永久移除", name)})
		return disconnected, nil
	case config.PluginRemovedUser, config.PluginRemovedProject:
		if !disconnected && m.reg != nil {
			m.reg.RemovePrefix(plugin.ToolPrefix(name))
		}
		return disconnected, nil
	default: // PluginNotFound
		if !disconnected {
			return false, fmt.Errorf("no MCP server named %q", name)
		}
		return disconnected, nil
	}
}

// DisconnectMCPServer disconnects a live server for this session without touching
// config — the connector toggle's "off". Its tools vanish next turn; it reconnects
// on the next session start, or now via ConnectConfiguredMCPServer (the "on").
// Reports whether a live server was actually disconnected.
func (m *mcpService) Disconnect(name string) bool {
	disconnected := false
	if m.host != nil {
		if prefix, ok := m.host.Remove(name); ok {
			disconnected = true
			if m.reg != nil {
				m.reg.RemovePrefix(prefix)
			}
		}
	}
	removedPlaceholder := 0
	if !disconnected && m.reg != nil {
		removedPlaceholder = m.reg.RemovePrefix(plugin.ToolPrefix(name))
	}
	return disconnected || removedPlaceholder > 0
}
