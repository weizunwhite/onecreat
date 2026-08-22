package main

// mcpService 管「MCP 服务器抽屉」:增删、重连、开关,以及那份「本会话被关掉的服务器」
// 视图状态 —— 配置里还留着、但这个会话没连(auto_start=false 或用户手动关),抽屉要把
// 它显示成一个关着的开关,而不是干脆消失。
//
// 这份视图状态过去挂在 App.mu 下面,和「当前项目文件夹」共用一把锁;现在归它自己。
// 依赖只有一个:活动标签的 controller(MCP host 挂在 controller 上)。

import (
	"fmt"
	"sort"
	"sync"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/plugin"
)

type mcpService struct {
	ctrlFn func() *control.Controller

	mu sync.RWMutex
	// disabled 是「配置里有、这个会话没连」的服务器视图,按名字存。
	disabled map[string]ServerView
	// order 记住抽屉里的展示顺序,避免每次刷新都跳。
	order []string
}

func newMCPService(ctrl func() *control.Controller) *mcpService {
	return &mcpService{ctrlFn: ctrl, disabled: map[string]ServerView{}}
}

// ctrl 是活动标签的 controller;没有标签或未注入时为 nil。
func (m *mcpService) ctrl() *control.Controller {
	if m == nil || m.ctrlFn == nil {
		return nil
	}
	return m.ctrlFn()
}

// MCPServerInput is the drawer's "add server" form. Transport is "stdio" (Command
// + Args + Env) or "http"/"sse" (URL). Mirrors config.PluginEntry's writable shape.
type MCPServerInput struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	URL       string            `json:"url"`
	Env       map[string]string `json:"env"`
}

// AddMCPServer connects a server live and persists it to config (Customize → MCP →
// Add). Returns the number of tools it exposed.
func (m *mcpService) AddServer(in MCPServerInput) (int, error) {
	ctrl := m.ctrl()
	if ctrl == nil {
		return 0, fmt.Errorf("no active session")
	}
	return ctrl.AddMCPServer(config.PluginEntry{
		Name:    in.Name,
		Type:    in.Transport,
		Command: in.Command,
		Args:    in.Args,
		URL:     in.URL,
		Env:     in.Env,
	})
}

// AddHardwareMCPServer connects the first available hardware MCP binary to the
// current session only. Direct hardware buttons call the MCP binary themselves;
// this method exposes the same tools to the AI conversation without silently
// changing the persistent config file.
func (m *mcpService) AddHardwareServer() (int, error) {
	command, _, err := resolveHardwareMCP()
	if err != nil {
		return 0, err
	}
	ctrl := m.ctrl()
	if ctrl == nil {
		return 0, fmt.Errorf("no active session")
	}
	return ctrl.ConnectMCPServer(config.PluginEntry{
		Name:    "hardware",
		Type:    "stdio",
		Command: command,
		Args:    []string{},
		Env:     map[string]string{},
	})
}

// RemoveMCPServer disconnects a live server and drops it from config (the row's ✕).
func (m *mcpService) RemoveServer(name string) error {
	ctrl := m.ctrl()
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	_, err := ctrl.RemoveMCPServer(name)
	if err == nil {
		m.mu.Lock()
		delete(m.disabled, name)
		m.order = removeServerOrder(m.order, name)
		m.mu.Unlock()
	}
	return err
}

// RetryMCPServer reconnects a configured server that failed or was disconnected,
// without touching config (the failed row's retry button).
func (m *mcpService) RetryServer(name string) error {
	ctrl := m.ctrl()
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	_, err := ctrl.ConnectConfiguredMCPServer(name)
	return err
}

// SetMCPServerEnabled is the connector toggle: on reconnects a configured server
// for this session, off disconnects it (config untouched either way — like Claude
// Code's per-conversation enable/disable, it resets on the next session start).
func (m *mcpService) SetServerEnabled(name string, enabled bool) error {
	ctrl := m.ctrl()
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	if enabled {
		_, err := ctrl.ConnectConfiguredMCPServer(name)
		if err == nil {
			m.mu.Lock()
			delete(m.disabled, name)
			m.mu.Unlock()
		}
		return err
	}
	if s, ok := findMCPServerView(ctrl, name); ok {
		s.Status = "disabled"
		s.Error = ""
		m.mu.Lock()
		if m.disabled == nil {
			m.disabled = map[string]ServerView{}
		}
		m.disabled[name] = s
		m.order = mergeServerOrder(m.order, []ServerView{s})
		m.mu.Unlock()
	}
	ctrl.DisconnectMCPServer(name)
	return nil
}

func findMCPServerView(ctrl *control.Controller, name string) (ServerView, bool) {
	if ctrl == nil || ctrl.Host() == nil {
		return ServerView{}, false
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == name {
			return ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				ToolList: pluginToolsToView(s.ToolList),
			}, true
		}
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == name {
			return ServerView{Name: f.Name, Transport: f.Transport, Status: "failed", Error: f.Error}, true
		}
	}
	return ServerView{}, false
}

func pluginToolsToView(tools []plugin.ToolInfo) []ToolView {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{Name: t.Name, Description: t.Description})
	}
	return out
}

func orderServerViews(servers []ServerView, order []string) []ServerView {
	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	sort.SliceStable(servers, func(i, j int) bool {
		pi, iok := pos[servers[i].Name]
		pj, jok := pos[servers[j].Name]
		switch {
		case iok && jok:
			return pi < pj
		case iok:
			return true
		case jok:
			return false
		default:
			return false
		}
	})
	return servers
}

func mergeServerOrder(order []string, servers []ServerView) []string {
	seen := make(map[string]bool, len(order)+len(servers))
	next := make([]string, 0, len(order)+len(servers))
	for _, name := range order {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		next = append(next, name)
	}
	for _, s := range servers {
		if s.Name == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		next = append(next, s.Name)
	}
	return next
}

func removeServerOrder(order []string, name string) []string {
	if name == "" || len(order) == 0 {
		return order
	}
	next := order[:0]
	for _, n := range order {
		if n != name {
			next = append(next, n)
		}
	}
	return next
}

// ServerViews projects the session's MCP servers for the drawer: connected and
// failed ones come from the session's plugin host, and configured-but-absent ones
// are shown as an off switch (disconnected this session, or auto_start=false).
//
// It also folds the result back into the service's own disabled/order state, so
// the drawer keeps a stable ordering and remembers which servers are off.
func (m *mcpService) ServerViews(ctrl *control.Controller) []ServerView {
	servers := []ServerView{}
	if ctrl == nil {
		return servers
	}
	m.mu.RLock()
	disabled := make(map[string]ServerView, len(m.disabled))
	for name, s := range m.disabled {
		disabled[name] = s
	}
	order := append([]string(nil), m.order...)
	m.mu.RUnlock()
	seen := map[string]bool{}
	connected := map[string]bool{}
	retainedDisabled := map[string]ServerView{}
	codegraphConfigured := false
	if h := ctrl.Host(); h != nil {
		for _, s := range h.Servers() {
			seen[s.Name] = true
			connected[s.Name] = true
			servers = append(servers, ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				ToolList: pluginToolsToView(s.ToolList),
			})
		}
		for _, f := range h.Failures() {
			seen[f.Name] = true
			servers = append(servers, ServerView{
				Name: f.Name, Transport: f.Transport, Status: "failed", Error: f.Error,
			})
		}
	}
	// Configured servers that are neither connected nor failed are toggled off
	// (disconnected this session, or auto_start=false) — shown with an off switch.
	if cfg, err := config.Load(); err == nil {
		codegraphConfigured = cfg.Codegraph.Enabled
		for _, p := range cfg.Plugins {
			if seen[p.Name] {
				continue
			}
			tt := p.Type
			if tt == "" {
				tt = "stdio"
			}
			if s, ok := disabled[p.Name]; ok {
				s.Status = "disabled"
				s.Transport = tt
				s.Error = ""
				servers = append(servers, s)
				retainedDisabled[p.Name] = s
				seen[p.Name] = true
				delete(disabled, p.Name)
				continue
			}
			servers = append(servers, ServerView{Name: p.Name, Transport: tt, Status: "disabled"})
			seen[p.Name] = true
		}
	}
	for name, s := range disabled {
		if seen[name] {
			continue
		}
		if name != "codegraph" || !codegraphConfigured {
			continue
		}
		s.Status = "disabled"
		s.Error = ""
		servers = append(servers, s)
		retainedDisabled[name] = s
	}
	servers = orderServerViews(servers, order)

	m.mu.Lock()
	for name := range connected {
		delete(retainedDisabled, name)
	}
	m.disabled = retainedDisabled
	m.order = mergeServerOrder(m.order, servers)
	m.mu.Unlock()
	return servers
}
