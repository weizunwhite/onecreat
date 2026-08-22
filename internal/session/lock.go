package session

// 索引文件的跨进程互斥(AR-R09)。
//
// 每个改动都是「读整份 → 改一条 → 写回整份」。`Registry.mu` 只管得住自己这一个实例,
// 而一个进程里必然有多个 Registry(desktop 的 sessionService 一个、每个标签的
// sessionStore 又各有一个),两个进程(Desktop + CLI)更是常态。于是两个 owner 可以
// 同时读到同一份旧内容,各自写回,后写的那个把前一个的改动整条抹掉 —— 改标题、
// 新建会话、Touch 都可能凭空消失。
//
// 原来的注释把这个窗口如实记着但没有关掉。这里用索引目录下的一个 lockfile 关掉它:
// `O_CREATE|O_EXCL` 在所有平台上都是原子的,不需要 cgo,也不依赖平台各异的 flock。
//
// 两处刻意的取舍:
//
//   - **陈旧锁会被抢走。** 持锁进程被 kill -9 时,锁文件会留下。超过 staleAfter 就
//     强行接管 —— 否则一次崩溃会让这台机器上的会话元数据永久只读。
//   - **平台差异要显式处理。** Windows 上「已经有人持锁」不一定报 EEXIST:释放方
//     `os.Remove` 之后文件会短暂进入 delete-pending 状态,这期间 `CreateFile` 返回
//     ERROR_ACCESS_DENIED。只认 `os.IsExist` 的话,这条路径会直接放弃取锁 ——
//     **锁在 Windows 上等于不存在**。见 contendedOn。
//   - **拿不到锁时放行,而不是报错。** 这些都是用户触发的低频动作(改个标题)。
//     为了一个卡住的锁去失败一次重命名,比那个锁本来要防的问题更烦人。真走到这一步
//     会记一条 warn。

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// lockFile 与索引同目录,同进同出。
	lockFile = ".sessions.lock"
	// lockTimeout 是放弃前的总等待时间。这些操作都是几毫秒的读写,等这么久
	// 只可能是对方崩了。
	lockTimeout = 2 * time.Second
	// lockPoll 是两次尝试之间的间隔。
	lockPoll = 2 * time.Millisecond
	// staleAfter 之后的锁视为遗留物。取值远大于一次正常操作的耗时。
	staleAfter = 15 * time.Second
)

// lockPath 是索引锁的路径。
func (r *Registry) lockPath() string { return filepath.Join(r.dir, lockFile) }

// withFileLock 在跨进程锁下执行 fn。拿不到锁时**照样执行** fn(见文件头的取舍),
// 所以 fn 的错误才是调用方唯一要关心的。
func (r *Registry) withFileLock(fn func() error) error {
	release, err := r.acquireLock()
	if err != nil {
		slog.Warn("session: 未能取得索引锁,本次更新在无锁下进行",
			"path", r.lockPath(), "err", err)
	} else {
		defer release()
	}
	return fn()
}

// contended 报告 err 是不是「有别人正持着锁,等一下再来」,而不是「这把锁根本拿不到」。
func contended(err error) bool { return contendedOn(runtime.GOOS, err) }

// contendedOn 是 contended 把平台注入进来的版本,好让 Windows 那条分支在**每个**平台上
// 都被测到。不这么做的话它只有 Windows CI 才验证得了 —— 而这个 bug 恰恰是本地只做交叉
// 编译、没跑过 Windows 运行期才漏掉的(CI run 32441069610 attempt 2 抓到)。
func contendedOn(goos string, err error) bool {
	// 所有平台通用的形态:O_EXCL 撞上已存在的文件。
	if errors.Is(err, fs.ErrExist) {
		return true
	}
	// Windows 独有:锁文件处于 delete-pending 时,打开它报的是 access denied 而不是
	// exists。这是个微秒级的瞬态窗口,重试正是对的处理。
	//
	// 只在 Windows 上这么放宽:unix 上的权限错误是真故障(目录只读之类),把它当成
	// 锁竞争会白等 2 秒再静默 fail-open,把一个明确的错误拖成一个难查的行为。
	return goos == "windows" && errors.Is(err, fs.ErrPermission)
}

// acquireLock 反复尝试原子地创建锁文件,直到成功或超时。
func (r *Registry) acquireLock() (func(), error) {
	if r.dir == "" {
		return func() {}, nil // 未启用持久化:没有索引,也就没有要互斥的东西
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return nil, err
	}
	path := r.lockPath()
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !contended(err) {
			return nil, err
		}
		// 已经有人持锁。它是活的还是遗留物?
		if fi, statErr := os.Stat(path); statErr == nil {
			if time.Since(fi.ModTime()) > staleAfter {
				// 抢走它。两个进程可能同时判定陈旧并各抢一次 —— 这个窗口比它
				// 替代的那个(永久只读)小得多,也短得多。
				os.Remove(path)
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待索引锁超时(%s)", lockTimeout)
		}
		time.Sleep(lockPoll)
	}
}
