//go:build !windows

package codegraph

// 这里的守护进程存活探测用 syscall.Kill(pid, 0),Windows 上没有这个符号:整包会在
// 跑到 t.Skip 之前就编译失败(Windows CI 的 vet/test 基线缺口)。用 build tag 表达
// 「本文件只在 Unix 编译」,跨平台的那半个用例留在 stopdaemon_test.go 里照常跑。

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// StopDaemon 读 .codegraph/daemon.pid 里的 pid 并终止它,然后删掉 pidfile。
func TestStopDaemonKillsAndCleans(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 造一个长命进程当「守护进程」。
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// 被杀后要 reap,否则子进程变僵尸,kill(pid,0) 仍返回 nil 会误判「还活着」。
	// (真实场景里守护进程是 detached、非本进程子进程,无此僵尸问题。)
	go func() { _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()

	pidPath := filepath.Join(root, ".codegraph", "daemon.pid")
	content := []byte(`{"pid":` + itoa(pid) + `,"version":"0.9.7","socketPath":"/tmp/x.sock"}`)
	if err := os.WriteFile(pidPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	StopDaemon(root)

	// 进程应在短时间内死亡。
	dead := false
	for i := 0; i < 30; i++ {
		if syscall.Kill(pid, 0) != nil {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		t.Fatal("StopDaemon 没能终止守护进程")
	}
	// pidfile 应被清掉。
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("StopDaemon 应删除 daemon.pid")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
