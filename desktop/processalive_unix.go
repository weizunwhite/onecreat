//go:build web && !windows

package main

import (
	"os"
	"syscall"
)

// processAlive 判断给定 pid 的进程是否还活着。
// 类 Unix:os.FindProcess 永远成功,用「发信号 0」探活 —— 进程在且有权限时返回 nil,
// 进程不存在返回 ESRCH。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
