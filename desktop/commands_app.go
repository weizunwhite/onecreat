package main

// 斜杠命令面板的 transport facade:列命令、解析参数补全。纯 DTO 组装 —— 命令目录
// 与参数解析都在内核侧,这里只把结果翻译成前端要的形状。

import (
	"strings"

	"reasonix/internal/control"
	"reasonix/internal/i18n"
)

func isGatewayManagedSlash(trimmed string) bool {
	return trimmed == "/model" || strings.HasPrefix(trimmed, "/model ") ||
		trimmed == "/effort" || strings.HasPrefix(trimmed, "/effort ")
}

// CommandInfo describes one available slash command for the composer's "/" menu.
type CommandInfo struct {
	Name        string `json:"name"` // without the leading slash
	Description string `json:"description"`
	Hint        string `json:"hint,omitempty"` // argument hint, if any
	Kind        string `json:"kind"`           // "builtin" | "custom" | "mcp"
}

// Commands lists the slash commands available this session — built-in actions,
// custom commands (.reasonix/commands), and MCP prompts — for the composer's "/"
// autocomplete menu.
func (a *App) Commands() []CommandInfo {
	out := []CommandInfo{
		{Name: "new", Description: i18n.M.CmdNew, Kind: "builtin"},
		{Name: "compact", Description: i18n.M.CmdCompact, Kind: "builtin"},
	}
	if !a.gatewayActive() {
		out = append(out,
			CommandInfo{Name: "model", Description: i18n.M.CmdModel, Kind: "builtin"},
			CommandInfo{Name: "effort", Description: i18n.M.CmdEffort, Kind: "builtin"},
		)
	}
	out = append(out,
		CommandInfo{Name: "memory", Description: i18n.M.CmdMemory, Kind: "builtin"},
		CommandInfo{Name: "mcp", Description: i18n.M.CmdMcp, Kind: "builtin"},
		CommandInfo{Name: "hooks", Description: i18n.M.CmdHooks, Kind: "builtin"},
		CommandInfo{Name: "theme", Description: i18n.M.CmdTheme, Kind: "builtin"},
		CommandInfo{Name: "skill", Description: i18n.M.CmdSkill, Kind: "builtin"},
	)
	ctrl := a.activeCtrl()
	if ctrl == nil {
		return out
	}
	// Skills are invocable as /<name> (the model runs inline ones; subagent ones
	// run isolated). Listing them here is what surfaces /init, /explore, … in the
	// composer's slash menu; selecting one submits "/<name>", which the controller
	// resolves via RunSkill.
	for _, s := range ctrl.Skills() {
		out = append(out, CommandInfo{Name: s.Name, Description: s.Description, Kind: "skill"})
	}
	for _, c := range ctrl.Commands() {
		out = append(out, CommandInfo{Name: c.Name, Description: c.Description, Hint: c.ArgHint, Kind: "custom"})
	}
	if h := ctrl.Host(); h != nil {
		for _, p := range h.Prompts() {
			out = append(out, CommandInfo{Name: p.Name, Description: p.Description, Kind: "mcp"})
		}
	}
	return out
}

// SlashArgItem is one sub-command / argument suggestion for the composer's slash
// menu (the part after the command word). Mirrors the CLI's arg completion via
// the shared control.SlashArgItems, so desktop and CLI offer the same hints.
type SlashArgItem struct {
	Label   string `json:"label"`
	Insert  string `json:"insert"`
	Hint    string `json:"hint"`
	Descend bool   `json:"descend"`
}

// SlashArgsResult carries the suggestions plus the byte offset in the input where
// the current token begins, so the composer replaces just that token.
type SlashArgsResult struct {
	Items []SlashArgItem `json:"items"`
	From  int            `json:"from"`
}

// SlashArgs completes the arguments of a management slash command (/mcp, /model,
// /skill, /hooks) for the composer — the same logic the chat TUI uses. Empty
// Items means the input has no structured arguments to complete.
func (a *App) SlashArgs(input string) SlashArgsResult {
	if a.gatewayActive() && isGatewayManagedSlash(strings.TrimSpace(input)) {
		return SlashArgsResult{Items: []SlashArgItem{}}
	}
	v, _ := a.tabs.View("")
	ctrl, model := v.ctrl, v.model
	if ctrl == nil {
		return SlashArgsResult{}
	}
	data := control.ArgData{
		Skills:       ctrl.Skills(),
		CurrentModel: model,
	}
	if !a.gatewayActive() {
		for _, m := range a.Models() {
			data.ModelRefs = append(data.ModelRefs, m.Ref)
		}
	}
	if h := ctrl.Host(); h != nil {
		data.ServerNames = h.ServerNames()
	}
	items, from := control.SlashArgItems(input, data)
	// Non-nil so it serializes as a JSON array, never null — the frontend filters
	// over it directly.
	out := SlashArgsResult{Items: []SlashArgItem{}, From: from}
	for _, it := range items {
		out.Items = append(out.Items, SlashArgItem{Label: it.Label, Insert: it.Insert, Hint: it.Hint, Descend: it.Descend})
	}
	return out
}
