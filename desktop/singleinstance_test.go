//go:build web

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// 锁文件写→读→内容一致。
func TestWebLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.lock")
	want := webLock{PID: 4242, Port: 3711, Token: "deadbeef", StartedAt: 1700000000}
	if err := writeWebLock(path, want); err != nil {
		t.Fatalf("writeWebLock: %v", err)
	}
	// 权限必须是 0600(锁里含 token)。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("锁文件权限 = %o, 期望 0600", perm)
	}
	got, err := readWebLock(path)
	if err != nil {
		t.Fatalf("readWebLock: %v", err)
	}
	if *got != want {
		t.Fatalf("读回 = %+v, 期望 %+v", *got, want)
	}
}

// 读不存在的锁文件应报错(调用方据此按「无锁」处理)。
func TestReadWebLockMissing(t *testing.T) {
	if _, err := readWebLock(filepath.Join(t.TempDir(), "nope.lock")); err == nil {
		t.Fatal("读不存在的锁文件应返回错误")
	}
}

// 陈旧锁判定:锁里的 pid 早已不存在 → reuseExistingInstance 应删掉锁并返回 false。
func TestReuseExistingInstanceStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.lock")
	// pid=1<<30 几乎不可能存在;端口给个没人监听的。
	if err := writeWebLock(path, webLock{PID: 1 << 30, Port: 65000, Token: "x", StartedAt: 1}); err != nil {
		t.Fatalf("writeWebLock: %v", err)
	}
	if reuseExistingInstance(path, true) {
		t.Fatal("陈旧锁不应被判为「已有实例」")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("陈旧锁应被删除")
	}
}

// 当前进程 pid 必然存活;不存在的 pid 必然不存活。
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("当前进程应判为存活")
	}
	if processAlive(-1) || processAlive(0) {
		t.Fatal("非法 pid 应判为不存活")
	}
}

// 端口自动避让:占住一个端口后,listenWithFallback 应跳过它、落到下一个。
func TestListenWithFallbackSkipsBusy(t *testing.T) {
	// 先占一个端口。
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	ln, got, err := listenWithFallback("127.0.0.1", busyPort, 20)
	if err != nil {
		t.Fatalf("listenWithFallback: %v", err)
	}
	defer ln.Close()
	if got == busyPort {
		t.Fatalf("应跳过被占端口 %d,实际却落在它上面", busyPort)
	}
	if got <= busyPort || got > busyPort+20 {
		t.Fatalf("落到的端口 %d 超出探测范围 (%d, %d]", got, busyPort, busyPort+20)
	}
}

// maxTries<1 时应至少尝试一次。
func TestListenWithFallbackSingle(t *testing.T) {
	ln, _, err := listenWithFallback("127.0.0.1", 0, 0)
	if err != nil {
		t.Fatalf("listenWithFallback(port=0): %v", err)
	}
	_ = ln.Close()
}
