package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/workspace"
)

// loadDotEnv reads the .env files for the process working directory. The
// working-directory .env is read first, so a project-local key takes precedence;
// then ~/.env is read as a fallback. This unifies the key source across
// frontends: the desktop app's working dir is $HOME so it writes ~/.env, and the
// CLI — run from any project directory — now picks up that same key instead of
// needing a copy in every project's .env. Existing environment variables always
// win over both files.
func loadDotEnv() Env {
	return loadDotEnvIn(workspace.Context{})
}

// loadDotEnvIn reads the workspace's .env, then the user's ~/.env. The zero
// Context reads ".env" relative to the process working directory — the
// behaviour this had before workspaces became explicit.
//
// 返回一个**按工作区隔离**的只读叠加层(AR-R10 / C1):凭据解析走它,子进程环境由
// `Env.Environ()` 显式组装,两个工作区的同名 key 不再互相污染。
//
// 这里**不再** os.Setenv。那一步曾经是给子进程用的兼容层(bash 工具、MCP 插件、
// 钩子、语言服务器都靠继承进程环境拿 `.env` 里的 `GITHUB_TOKEN`),但它同时就是污染的
// 根:进程环境只有一份,谁先加载谁的值就留在里面。改法不是删掉了事 —— 那会让用户
// `.env` 里的 token 对 `gh`、`docker` 静默失效 —— 而是让每个起子进程的地方显式收下
// `cfg.Env().Environ()`。装配点见 boot.Build 里的 childEnv。
func loadDotEnvIn(ws workspace.Context) Env {
	var env Env
	loadDotEnvFile(ws.Resolve(".env"), &env)
	if home, err := os.UserHomeDir(); err == nil {
		loadDotEnvFile(filepath.Join(home, ".env"), &env)
	}
	return env
}

// loadDotEnvFile reads one .env file (if present) into the workspace overlay.
// Lenient, zero-dependency parsing. It has no process-global effect.
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
	}
}
