//go:build web

package main

// 单实例守卫 + 「第二次双击」体验。
//
// 老师用的心智模型是「双击图标 = 打开软件」。Web 模式下软件是一个本地 HTTP 服务 + 浏览器
// 标签页;关掉标签页后再双击程序,如果只是「端口被占 → 报错退出」,体验就很差。
//
// 这里的做法:
//  1. 启动成功后在配置目录写一个锁文件 web.lock(0600),记 {pid, port, token, startedAt};
//     退出时删。
//  2. 再次启动时先读锁:若 pid 还活着且 GET /healthz 正常 → 说明已有实例在跑,用锁里的
//     token 把浏览器开到已有实例、打印一句说明、本进程退出码 0(不再抢端口)。
//  3. 锁存在但进程已死 / healthz 不通 → 视为陈旧锁,删掉照常启动。
//  4. 端口被「别的程序」占用时自动向上探端口(见 listenWithFallback),不直接死。
//
// 只在回环绑定时启用单实例(远程绑定是显式的多机访问场景,不套这层)。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/config"
)

// webLock 是锁文件的内容。token 也落进锁里,这样「第二次双击」不必重新生成 token,
// 直接用已有实例那把,浏览器就能免登录连回去。
type webLock struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
	StartedAt int64  `json:"startedAt"`
}

// webLockPath 返回锁文件路径(配置目录下的 web.lock)。配置目录取不到时返回 ""。
func webLockPath() string {
	dir := config.MemoryUserDir() // …/onecreat
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "web.lock")
}

// readWebLock 读并解析锁文件。文件不存在或坏掉都返回 (nil, err),调用方按「无锁」处理。
func readWebLock(path string) (*webLock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lk webLock
	if err := json.Unmarshal(b, &lk); err != nil {
		return nil, err
	}
	return &lk, nil
}

// writeWebLock 原子写锁文件(0600):先写临时文件再 rename,避免半截内容被别的进程读到。
func writeWebLock(path string, lk webLock) error {
	if path == "" {
		return fmt.Errorf("锁文件路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(lk)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeWebLock 删除锁文件(退出时调用);删不掉(比如已被别的进程删)静默忽略。
func removeWebLock(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// webHealthy 探活:GET http://127.0.0.1:<port>/healthz,2xx 即认为已有实例还活着。
// 短超时,避免卡住启动。
func webHealthy(port int) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// reuseExistingInstance 检查是否已有实例在跑。若有(pid 活 + healthz 通)→ 把浏览器开到
// 已有实例并返回 true(调用方据此退出码 0);锁陈旧则删掉并返回 false 让本进程照常启动。
func reuseExistingInstance(lockPath string, noOpen bool) bool {
	if lockPath == "" {
		return false
	}
	lk, err := readWebLock(lockPath)
	if err != nil || lk == nil {
		return false
	}
	if processAlive(lk.PID) && lk.Port > 0 && webHealthy(lk.Port) {
		url := fmt.Sprintf("http://%s/?token=%s", net.JoinHostPort("127.0.0.1", fmt.Sprint(lk.Port)), lk.Token)
		fmt.Println("OneCreat 已在运行,已为你打开页面:")
		fmt.Println("  " + url)
		if !noOpen {
			_ = openWorkspacePath(url)
		}
		return true
	}
	// 陈旧锁(进程已死或服务不通):删掉,照常启动。
	removeWebLock(lockPath)
	return false
}

// listenWithFallback 在 host 上从 startPort 起向上探最多 maxTries 个端口,返回第一个能监听
// 的 listener 和实际端口。用于「端口被别的程序占了就自动换一个」,而不是直接报错退出。
// 全部失败才返回最后一次错误。
func listenWithFallback(host string, startPort, maxTries int) (net.Listener, int, error) {
	if maxTries < 1 {
		maxTries = 1
	}
	var lastErr error
	for i := 0; i < maxTries; i++ {
		port := startPort + i
		ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
		if err == nil {
			return ln, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("从 %d 起连续 %d 个端口都无法监听: %w", startPort, maxTries, lastErr)
}
