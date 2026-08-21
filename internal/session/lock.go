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
//   - **拿不到锁时放行,而不是报错。** 这些都是用户触发的低频动作(改个标题)。
//     为了一个卡住的锁去失败一次重命名,比那个锁本来要防的问题更烦人。真走到这一步
//     会记一条 warn。

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		if !os.IsExist(err) {
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
