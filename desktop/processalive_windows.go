//go:build web && windows

package main

import "os"

// processAlive 判断给定 pid 的进程是否还活着。
// Windows:os.FindProcess 会真的 OpenProcess,pid 不存在时返回错误 —— 以此判活,零依赖。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
