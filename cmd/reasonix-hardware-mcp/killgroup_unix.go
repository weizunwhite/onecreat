//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// setKillGroup 让命令在超时/取消时杀掉整个进程组,而不仅是直接子进程。idf.py monitor 会
// fork esp_idf_monitor 子进程,只 SIGKILL idf.py 会留下占着串口的孙进程,阻塞后续 flash;
// 本地回退路径(bash -lc … exec idf.py monitor)同形。给子进程单独建组(Setpgid),Cancel
// 时对 -pid 发 SIGKILL 让信号覆盖整组,WaitDelay 兜底确保 Run 及时返回(F2)。
func setKillGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
