package config

// 工作区作用域的环境叠加层(AR-R10)。
//
// 问题:`.env` 原来直接 `os.Setenv` 进进程环境,而且"已有值不覆盖"。同一个进程里开两个
// 工作区时,**先加载的那个同名 key 永久获胜** —— 工作区 A 的 `.env` 写 `API_KEY=A`、
// 工作区 B 写 `API_KEY=B`,B 的 provider 会一直拿着 A 的 key。后果不是配置没生效那么轻:
// 用错 key 意味着请求打到别的账号上,或者计费记到别人头上。
//
// 这里把 `.env` 解析成一个**按工作区隔离、构造后只读**的叠加层。查一个 key 的顺序:
//
//	1. 用户自己的环境变量 —— 永远最高优先级
//	2. 本工作区的 .env,然后 ~/.env
//	3. 都没有 —— 就是没有
//
// 修复的另一半在 C1:`.env` 不再 os.Setenv,子进程环境由 Environ() 按工作区显式组装。
// 于是第 1 条不需要任何例外 —— 进程环境里的值,现在真的只可能是用户自己设的。

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// Env 是一份工作区作用域的只读环境叠加层。零值可用,行为与直接读进程环境一致 ——
// 所以那些不经 Load 直接构造 Config/ProviderEntry 的地方(测试、向导)不受影响。
type Env struct {
	// overlay 在 Load 时一次性建好,之后只读 —— 复制 Env 是安全的,也不需要锁。
	overlay map[string]string
}

// Lookup 按文件头的顺序查一个变量。
func (e Env) Lookup(key string) (string, bool) {
	// 1. 用户自己的环境变量,最高优先级。`.env` 不再写进进程环境(C1),所以这里
	//    读到的东西一定是用户或宿主设的,不会是别的工作区漏过来的。
	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}
	// 2. 本工作区的 .env,然后 ~/.env。
	if v, ok := e.overlay[key]; ok {
		return v, true
	}
	return "", false
}

// Get 是 Lookup 的取值版本,查不到返回空串。
func (e Env) Get(key string) string {
	v, _ := e.Lookup(key)
	return v
}

// Environ 返回**这个工作区的子进程**应该看到的环境,可直接赋给 `exec.Cmd.Env`。
//
// 组成:进程环境 + 本工作区 `.env` 里进程环境**没有**的那些键。顺序反映优先级 ——
// 用户显式设的永远赢过项目文件。
//
// 有了它,`.env` 就不必再往进程环境里 `os.Setenv`(复核 C1):谁需要这些值,谁显式地把
// 这份环境交给自己的子进程。这是两个工作区能真正隔离的前提 —— 进程环境只有一份,
// 而 Environ() 每个工作区各有各的。
func (e Env) Environ() []string { return environFrom(os.Environ(), e.overlay) }

// environFrom is Environ with the process environment passed in, so the merge
// rule is testable without mutating the real one.
func environFrom(base []string, overlay map[string]string) []string {
	return environMerge(base, overlay, envKey)
}

// environMerge is environFrom with the name normaliser injected, so **both**
// platform rules are covered on every platform. Without this the Windows branch
// of envKey would only ever run on a Windows CI leg — and that is precisely the
// branch whose absence silently inverts the process-env-wins priority.
func environMerge(base []string, overlay map[string]string, norm func(string) string) []string {
	if len(overlay) == 0 {
		return base
	}
	have := make(map[string]bool, len(base))
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok {
			have[norm(k)] = true
		}
	}
	out := make([]string, 0, len(base)+len(overlay))
	out = append(out, base...)
	// 排序后追加:同一份配置每次得到同样的切片,免得子进程环境的顺序随 map 迭代
	// 抖动 —— 那种不确定性调试起来极其费劲。
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		if !have[norm(k)] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overlay[k])
	}
	return out
}

// envKey normalises an environment variable name for comparison.
//
// Windows 的环境变量名不区分大小写,而 `os/exec` 在 Windows 上做的去重也不区分 ——
// 并且**保留最后一个**。所以如果这里按大小写敏感比较,进程里的 `Path` 和 `.env` 里的
// `PATH` 会双双进入切片,最终子进程拿到的是 `.env` 那一份 —— 恰好把「进程环境优先」
// 这条语义倒过来。Unix 上名字是大小写敏感的,原样比较才对。
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// Expand 替换 ${VAR} / ${VAR:-default},取值经本工作区的叠加层 —— 与 ExpandVars 的区别
// 就是"哪个工作区"。插件的 command/args/env/headers 都走它。
func (e Env) Expand(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		g := varRef.FindStringSubmatch(m)
		name, hasDefault, def := g[1], g[2] != "", g[3]
		if v, ok := e.Lookup(name); ok && v != "" {
			return v
		}
		if hasDefault {
			return def
		}
		return ""
	})
}

// Empty reports whether this Env carries no `.env` values at all — i.e. it
// behaves exactly like the process environment.
func (e Env) Empty() bool { return len(e.overlay) == 0 }

// Overlay 返回叠加层的副本(只含来自 .env 的键)。给需要显式组装子进程环境的调用方用 ——
// 返回副本,免得谁拿去改就把"构造后只读"这条破坏掉。
func (e Env) Overlay() map[string]string {
	if len(e.overlay) == 0 {
		return nil
	}
	out := make(map[string]string, len(e.overlay))
	for k, v := range e.overlay {
		out[k] = v
	}
	return out
}

// set 记下一个来自文件的值。先读到的文件优先(工作区 .env 早于 ~/.env),与原来的
// 「已有值不覆盖」一致。仅供 Load 期间使用。
func (e *Env) set(key, val string) {
	if e.overlay == nil {
		e.overlay = map[string]string{}
	}
	if _, exists := e.overlay[key]; !exists {
		e.overlay[key] = val
	}
}
