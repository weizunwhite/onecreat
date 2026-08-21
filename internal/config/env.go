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
//	1. 用户自己的环境变量(排除掉由我们导出的那些)—— 永远最高优先级
//	2. 本工作区的 .env,然后 ~/.env
//	3. 都没有 —— 别的工作区导出的兼容值不算数
//
// 第 1 条里那个"排除"是关键,理由见下面 exported 的注释。

import (
	"os"
	"sync"
)

// exported 记下所有由 .env 导出到进程环境的 key(见 dotenv.go)。
//
// 这一份记录是整个修复的支点。为了子进程兼容,`.env` 的值仍然会被 os.Setenv 出去;
// 要是查找时一上来就读 os.LookupEnv,工作区 A 导出的值就会赢过工作区 B 的叠加层 ——
// 修了等于没修。所以必须能认出"这个值是我们自己从某个 .env 导出的"。
//
// 一个关键前提让这条判断是安全的:导出**只在该 key 尚不存在时**发生,所以
// wasExported(key) 为真 ⇒ 用户自己没有设过它 ⇒ 跳过进程环境不会盖掉用户的显式设置。
var (
	exportedMu sync.RWMutex
	exported   = map[string]bool{}
)

func markExported(key string) {
	exportedMu.Lock()
	exported[key] = true
	exportedMu.Unlock()
}

func wasExported(key string) bool {
	exportedMu.RLock()
	defer exportedMu.RUnlock()
	return exported[key]
}

// Env 是一份工作区作用域的只读环境叠加层。零值可用,行为与直接读进程环境一致 ——
// 所以那些不经 Load 直接构造 Config/ProviderEntry 的地方(测试、向导)不受影响。
type Env struct {
	// overlay 在 Load 时一次性建好,之后只读 —— 复制 Env 是安全的,也不需要锁。
	overlay map[string]string
}

// Lookup 按文件头的顺序查一个变量。
func (e Env) Lookup(key string) (string, bool) {
	// 1. 用户自己的环境变量,最高优先级。只在这个 key 不是我们导出的时候才算数 ——
	//    导出只发生在该 key 原本不存在时,所以"是我们导出的"就等于"用户没设过"。
	if !wasExported(key) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
	}
	// 2. 本工作区的 .env,然后 ~/.env。
	if v, ok := e.overlay[key]; ok {
		return v, true
	}
	// 3. 剩下的只可能是**别的**工作区导出的兼容值。它不属于这里,当作没有。
	return "", false
}

// Get 是 Lookup 的取值版本,查不到返回空串。
func (e Env) Get(key string) string {
	v, _ := e.Lookup(key)
	return v
}

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
