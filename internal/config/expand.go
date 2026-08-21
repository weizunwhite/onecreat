package config

import (
	"regexp"
)

// varRef matches ${VAR} and ${VAR:-default}: a shell-style reference with an
// optional ":-default" fallback used when the variable is unset or empty.
var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandVars substitutes ${VAR} / ${VAR:-default} references from the process
// environment. An unset variable with no default expands to "" (matching the
// MCP / Claude Code convention), so a missing secret yields an empty header
// rather than a literal "${TOKEN}" leaking onto the wire.
//
// 它只看进程环境。要让 `${VAR}` 也能取到**本工作区** `.env` 的值,用 `Env.Expand`
// —— `.env` 不再写进进程环境之后,这个区别就是真的了(复核 C1)。
func ExpandVars(s string) string { return Env{}.Expand(s) }

// ExpandedPlugin returns a copy of e with ${VAR} references expanded across the
// command, args, env values, url, and header values — the fields Claude Code
// also expands. The entry itself is left untouched.
func (e PluginEntry) ExpandedPlugin() PluginEntry { return e.ExpandedPluginIn(Env{}) }

// ExpandedPluginIn 是 ExpandedPlugin 的工作区版本:`${VAR}` 经该工作区的叠加层解析,
// 于是两个项目的 `.env` 各自配自己的插件密钥,不再抢同一个进程环境(复核 C1)。
func (e PluginEntry) ExpandedPluginIn(env Env) PluginEntry {
	out := e
	out.Command = env.Expand(e.Command)
	out.URL = env.Expand(e.URL)
	if len(e.Args) > 0 {
		out.Args = make([]string, len(e.Args))
		for i, a := range e.Args {
			out.Args[i] = env.Expand(a)
		}
	}
	out.Env = expandMapIn(env, e.Env)
	out.Headers = expandMapIn(env, e.Headers)
	return out
}

func expandMapIn(env Env, m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = env.Expand(v)
	}
	return out
}
