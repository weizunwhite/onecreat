//go:build windows

package main

import "os/exec"

// Windows 没有 POSIX 进程组的负 pid 语义;退回 CommandContext 默认的取消行为(杀直接
// 子进程)。硬件 MCP 实际只在 macOS/Linux 上跑 idf.py/arduino-cli,这里仅为可编译(F2)。
func setKillGroup(cmd *exec.Cmd) {}
