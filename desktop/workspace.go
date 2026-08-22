package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/workspace"
)

// The desktop is a GUI app: launched from Finder or `open`, it starts with the
// working directory set to "/" (read-only), so anything cwd-relative — config,
// .env writes, memory/skill discovery — fails or lands nowhere useful. We keep a
// real working folder instead: remember the last one the user picked and chdir
// into it at startup, falling back to the home directory when there's none and
// cwd isn't writable.

// workspaceStatePath is where the last working folder is remembered (under the
// user config dir, shared with the rest of Reasonix's state).
func workspaceStatePath() string {
	dir := config.MemoryUserDir() // …/reasonix
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "desktop-workspace")
}

func workspaceListPath() string {
	dir := config.MemoryUserDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "desktop-workspaces.json")
}

// saveWorkspace records dir as the last working folder.
func saveWorkspace(dir string) {
	p := workspaceStatePath()
	if p == "" || dir == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(dir), 0o644)
	rememberWorkspace(dir)
}

// loadWorkspace returns the remembered working folder, or "" if none.
func loadWorkspace() string {
	p := workspaceStatePath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func loadWorkspaces() []string {
	p := workspaceListPath()
	if p == "" {
		return nil
	}
	var paths []string
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &paths) != nil {
		return nil
	}
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func rememberWorkspace(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	paths := []string{dir}
	for _, path := range loadWorkspaces() {
		if path != dir {
			paths = append(paths, path)
		}
		if len(paths) >= 12 {
			break
		}
	}
	p := workspaceListPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	if b, err := json.MarshalIndent(paths, "", "  "); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
}

// resolveStartupWorkspace picks the folder the app opens in: the remembered one
// if it is still a directory, else the home directory when the current cwd isn't
// writable (the Finder/`open` "/" case). A writable cwd with no remembered
// folder (e.g. `wails dev` in the repo) is kept.
//
// It also chdirs there. That is deliberate and is *startup* compatibility, not
// runtime state: the process should not sit in a read-only "/" for the parts of
// the app that still resolve paths against the process cwd. Runtime workspace
// identity is the returned Context — switching projects later re-points that
// Context and never calls os.Chdir, so tabs can hold different roots at once.
func resolveStartupWorkspace() workspace.Context {
	if remembered := loadWorkspace(); remembered != "" {
		if info, err := os.Stat(remembered); err == nil && info.IsDir() && os.Chdir(remembered) == nil {
			if ws, err := workspace.New(remembered); err == nil {
				return ws
			}
		}
	}
	if !cwdWritable() {
		if home, err := os.UserHomeDir(); err == nil && os.Chdir(home) == nil {
			if ws, err := workspace.New(home); err == nil {
				return ws
			}
		}
	}
	// Whatever we ended up in is this launch's workspace; making it explicit here
	// means every later reader asks the Context, not the process.
	ws, err := workspace.Current()
	if err != nil {
		return workspace.Context{}
	}
	return ws
}

// cwdWritable reports whether the current directory accepts a file write — the
// reliable test for the read-only "/" a GUI launch lands in.
func cwdWritable() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(cwd, ".reasonix-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
