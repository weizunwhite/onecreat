//go:build !windows

package codegraph

import "testing"

// 杀进程那半个用例是 Unix-only(syscall.Kill),在 stopdaemon_unix_test.go。
// 这里只留跨平台成立的部分,Windows 上也要跑。

// 没有 pidfile / root 为空:静默返回,不 panic。
func TestStopDaemonNoop(t *testing.T) {
	StopDaemon("")
	StopDaemon(t.TempDir()) // 无 .codegraph/daemon.pid
}
