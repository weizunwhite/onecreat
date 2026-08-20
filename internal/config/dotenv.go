package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/workspace"
)

// loadDotEnv loads .env files into the process environment without overriding
// variables that are already set. The working-directory .env is read first, so a
// project-local key takes precedence; then ~/.env is read as a fallback. This
// unifies the key source across frontends: the desktop app's working dir is
// $HOME so it writes ~/.env, and the CLI — run from any project directory — now
// picks up that same key instead of needing a copy in every project's .env.
// Existing environment variables always win over both files.
func loadDotEnv() {
	loadDotEnvIn(workspace.Context{})
}

// loadDotEnvIn reads the workspace's .env, then the user's ~/.env. The zero
// Context reads ".env" relative to the process working directory — the
// behaviour this had before workspaces became explicit.
//
// 注意:值最终仍落到进程环境(os.Setenv),这条「环境变量当应用状态总线」的问题
// 属于 Plan 09(Credential / Gateway runtime),Plan 01 只把【读哪个文件】变成
// 显式的 workspace 决定。
func loadDotEnvIn(ws workspace.Context) {
	loadDotEnvFile(ws.Resolve(".env"))
	if home, err := os.UserHomeDir(); err == nil {
		loadDotEnvFile(filepath.Join(home, ".env"))
	}
}

// loadDotEnvFile reads one .env file (if present) and sets any keys not already
// present in the environment. Lenient, zero-dependency parsing.
func loadDotEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
