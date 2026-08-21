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
func loadDotEnv() Env {
	return loadDotEnvIn(workspace.Context{})
}

// loadDotEnvIn reads the workspace's .env, then the user's ~/.env. The zero
// Context reads ".env" relative to the process working directory — the
// behaviour this had before workspaces became explicit.
//
// 返回一个**按工作区隔离**的只读叠加层(AR-R10):凭据解析走它,于是两个工作区的
// 同名 key 不再互相污染。
//
// 同时**仍然**把值导出到进程环境(仅当该 key 尚未存在)。这一份是给子进程用的
// 兼容层:`bash` 工具、MCP 插件、LSP 都不设 cmd.Env,靠继承进程环境拿到 `.env` 里的
// `GITHUB_TOKEN` 之类。直接去掉它是一个**用户可见的回归**,不能悄悄做 —— 那意味着
// 用户 `.env` 里的 token 突然对 `gh`、`docker` 不生效了。
//
// 所以 AR-R10 在这里只修了有实际后果的那一半(凭据解析)。另一半 —— 让子进程环境
// 也按工作区隔离 —— 需要给 bash/plugin/LSP 显式组装 env,是一个有用户可见取舍的
// 设计决定,登记为遗留项,见总计划 §21。
func loadDotEnvIn(ws workspace.Context) Env {
	var env Env
	loadDotEnvFile(ws.Resolve(".env"), &env)
	if home, err := os.UserHomeDir(); err == nil {
		loadDotEnvFile(filepath.Join(home, ".env"), &env)
	}
	return env
}

// loadDotEnvFile reads one .env file (if present) into the workspace overlay, and
// also exports it to the process environment for subprocess compatibility (see
// loadDotEnvIn). Lenient, zero-dependency parsing.
func loadDotEnvFile(path string, env *Env) {
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
		// 叠加层:本工作区的权威来源,先读到的文件优先。
		env.set(key, val)
		// 进程环境:仅供子进程继承的兼容层,不覆盖用户显式设的值。导出的 key 都
		// 记下来 —— 查找时要能认出「这是别的工作区导出的」并跳过(见 env.go)。
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
			markExported(key)
		}
	}
}
