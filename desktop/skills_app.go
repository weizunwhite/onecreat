package main

// 技能(skills)面板的 transport facade。这一片没有自己的运行时状态:技能根来自
// 配置,技能索引是每次 boot.Build 的快照,所以改动一律走「改配置 → 重建会话」
// 这条既有路径(applyConfigChange / rebuild),这里只做选目录、路径规范化和视图组装。

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/skill"
	"reasonix/internal/workspace"
)

// SkillView is one discoverable skill for the drawer.
type SkillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	RunAs       string `json:"runAs"`
}

// SkillRootView is one skill discovery root for the drawer's Sources section.
type SkillRootView struct {
	Dir        string `json:"dir"`
	Scope      string `json:"scope"`
	Priority   int    `json:"priority"`
	Status     string `json:"status"`
	Configured bool   `json:"configured"`
	Skills     int    `json:"skills"`
	Warning    string `json:"warning,omitempty"`
}

// skillRootsView lists the skill roots for one project. ws is the workspace whose
// project-level skills are counted, so the panel shows the open project's skills
// rather than whatever directory the process happens to stand in.
func skillRootsView(ws workspace.Context) []SkillRootView {
	cfg, _ := config.LoadIn(ws)
	userCfg := config.LoadForEdit(config.UserConfigPath())
	var custom []string
	if cfg != nil {
		custom = cfg.SkillCustomPaths()
	}
	st := skill.New(skill.Options{ProjectRoot: ws.Root(), CustomPaths: custom, DisableBuiltins: true, Stderr: io.Discard})
	counts := map[string]int{}
	for _, sk := range st.List() {
		counts[config.CanonicalSkillPath(filepath.Dir(skillRootPath(sk.Path)))]++
	}
	userConfigured := map[string]bool{}
	if userCfg != nil {
		for _, p := range userCfg.Skills.Paths {
			userConfigured[config.CanonicalSkillPath(p)] = true
		}
	}
	var out []SkillRootView
	for _, r := range st.Roots() {
		dir := config.CanonicalSkillPath(r.Dir)
		view := SkillRootView{
			Dir:        r.Dir,
			Scope:      string(r.Scope),
			Priority:   r.Priority + 1,
			Status:     string(r.Status),
			Configured: r.Scope == skill.ScopeCustom && userConfigured[dir],
			Skills:     counts[dir],
		}
		out = append(out, view)
	}
	if userCfg != nil {
		for _, p := range userCfg.Skills.Paths {
			if rootActive(out, p) {
				continue
			}
			out = append(out, SkillRootView{
				Dir:        p,
				Scope:      string(skill.ScopeCustom),
				Status:     "inactive",
				Configured: true,
				Warning:    "configured in user config but not active in this workspace; project [skills].paths may override it",
			})
		}
	}
	return out
}

func rootActive(roots []SkillRootView, path string) bool {
	want := config.CanonicalSkillPath(path)
	for _, r := range roots {
		if r.Scope == string(skill.ScopeCustom) && config.CanonicalSkillPath(r.Dir) == want {
			return true
		}
	}
	return false
}

// PickSkillFolder opens a directory picker for adding custom skill roots. It only
// returns a path; AddSkillPath performs normalization and writes config.
func (a *App) PickSkillFolder() (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	dir, err := a.sh().OpenDirectoryDialog(DialogOptions{
		Title:            "Choose skills folder",
		DefaultDirectory: a.workspaceRoot(),
	})
	if err != nil || dir == "" {
		return "", err
	}
	return normalizeSkillPath(dir), nil
}

// AddSkillPath adds a custom skill root to the user config and rebuilds the
// controller so the skills index and slash menu reflect it immediately.
func (a *App) AddSkillPath(path string) error {
	path = normalizeSkillPath(path)
	return a.applyConfigChange(func(c *config.Config) error {
		return c.AddSkillPath(path)
	})
}

// RemoveSkillPath removes a custom skill root from the user config and rebuilds.
func (a *App) RemoveSkillPath(path string) error {
	path = normalizeSkillPath(path)
	return a.applyConfigChange(func(c *config.Config) error {
		_, err := c.RemoveSkillPath(path)
		return err
	})
}

// RefreshSkills rebuilds the controller without changing config, reloading skill
// discovery, the system prompt index, and slash completions.
func (a *App) RefreshSkills() error {
	return a.rebuild()
}

func normalizeSkillPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if info.Mode().IsRegular() {
		if filepath.Base(path) == skill.SkillFile {
			return filepath.Clean(filepath.Dir(filepath.Dir(path)))
		}
		return filepath.Clean(filepath.Dir(path))
	}
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(path, skill.SkillFile)); err == nil {
			return filepath.Clean(filepath.Dir(path))
		}
	}
	return filepath.Clean(path)
}

func skillRootPath(path string) string {
	if filepath.Base(path) == skill.SkillFile {
		return filepath.Dir(path)
	}
	return path
}
