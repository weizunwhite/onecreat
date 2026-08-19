package codegraph

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// StopDaemon 读 .codegraph/daemon.pid 里的 pid 并终止它,然后删掉 pidfile。
func TestStopDaemonKillsAndCleans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sleep 造被杀进程,Windows 上跳过")
	}
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

// 没有 pidfile / root 为空:静默返回,不 panic。
func TestStopDaemonNoop(t *testing.T) {
	StopDaemon("")
	StopDaemon(t.TempDir()) // 无 .codegraph/daemon.pid
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
