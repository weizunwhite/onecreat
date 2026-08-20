//go:build web

// Command onecreat-web 是 OneCreat 的「Web 模式」入口:同一个 *App(与 Wails 桌面版
// 逐字相同的 143 个方法),换一层传输——起一个只绑回环地址的本地 HTTP 服务,前端
// bundle 内嵌在二进制里,浏览器打开就是完整 UI。
//
// 之所以是「本地服务 + 浏览器」而不是云端部署:agent 必须跑在用户本机才能碰 USB
// 串口、烧录固件、读写本地工程目录。
//
// 构建:cd desktop && go build -tags web -o ../bin/onecreat-web .
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// 与 cmd/reasonix / main.go 一致:空导入把编译期内建注册进各自注册表,
	// boot.Build 从注册表里解析 provider/tool。
	_ "reasonix/internal/provider/anthropic"
	_ "reasonix/internal/provider/openai"
	_ "reasonix/internal/tool/builtin"
)

// assets 内嵌构建好的前端。`all:` 让点开头的文件(dist/.gitkeep)也进来,
// 这样首次 pnpm build 之前该指令也能编译。真跑起来需要先 pnpm build。
//
//go:embed all:frontend/dist
var assets embed.FS

// version 由构建期 -ldflags "-X main.version=…" 注入,与 Wails 版一致。
var version = "dev"

func main() {
	port := flag.Int("port", 3700, "本地服务端口")
	host := flag.String("host", "127.0.0.1", "绑定地址;默认只绑回环,显式改成非回环会打印警告")
	noOpen := flag.Bool("no-open", false, "启动后不自动打开浏览器")
	workspace := flag.String("workspace", "", "启动时切到该工作目录(默认沿用上次记住的)")
	flag.Parse()

	if err := run(*host, *port, *workspace, *noOpen); err != nil {
		fmt.Fprintln(os.Stderr, "onecreat-web:", err)
		os.Exit(1)
	}
}

func run(host string, port int, workspace string, noOpen bool) error {
	// 非回环绑定是显式选择(必须自己写 --host),这里再吼一嗓子:Web 模式暴露的是
	// 一个能在本机执行命令、读写文件、烧录设备的 agent,不是一个只读页面。
	remote := !isLoopback(host)
	if remote {
		fmt.Fprintf(os.Stderr, "⚠️  警告:正在绑定非回环地址 %s —— 任何能访问该地址并拿到 token 的人,都能在你的电脑上执行命令、读写文件、烧录设备。\n", host)
	}

	if workspace != "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return fmt.Errorf("--workspace 路径无效: %w", err)
		}
		if err := os.Chdir(abs); err != nil {
			return fmt.Errorf("切到 --workspace 失败: %w", err)
		}
		saveWorkspace(abs) // 让 resolveStartupWorkspace 在 startup 里沿用它
	}

	sub, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		return fmt.Errorf("定位内嵌前端失败: %w", err)
	}

	// 单实例守卫:回环绑定时,若已有实例在跑,直接把浏览器开到它并退出(「第二次双击」)。
	// 远程绑定是显式多机场景,不套这层。
	lockPath := ""
	if !remote {
		lockPath = webLockPath()
		if reuseExistingInstance(lockPath, noOpen) {
			return nil // 已有实例接管,本进程干净退出
		}
	}

	token, err := newWebToken()
	if err != nil {
		return fmt.Errorf("生成访问 token 失败: %w", err)
	}

	// 先监听再 startup:端口被占的话立刻知道,不必等 boot.Build 跑完。端口被「别的程序」
	// 占用时自动向上探端口(最多 20 个),不直接死 —— 实际监听到的端口可能与请求的不同。
	ln, actualPort, err := listenWithFallback(host, port, 20)
	if err != nil {
		return err
	}

	app := NewApp()
	srv, err := newWebServer(app, sub, app.webEvents(), token, host, actualPort, remote, version)
	if err != nil {
		_ = ln.Close()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 与 Wails 的 OnStartup 走同一个函数:装配工作区、config、controller。
	app.startup(ctx)

	httpSrv := &http.Server{Handler: srv.Handler()}
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 服务真起来了才写锁:记实际端口 + token,给「第二次双击」复用。退出时删。
	if lockPath != "" {
		if err := writeWebLock(lockPath, webLock{PID: os.Getpid(), Port: actualPort, Token: token, StartedAt: time.Now().Unix()}); err != nil {
			fmt.Fprintln(os.Stderr, "onecreat-web: 写单实例锁失败(不影响本次启动):", err)
		}
		defer removeWebLock(lockPath)
	}

	url := fmt.Sprintf("http://%s/?token=%s", net.JoinHostPort(displayHost(host), fmt.Sprint(actualPort)), token)
	fmt.Println("OneCreat Web 已启动:")
	fmt.Println("  " + url)
	if actualPort != port {
		fmt.Printf("(请求的端口 %d 被占用,已自动改用 %d)\n", port, actualPort)
	}
	fmt.Println("(带 token 的链接只在本次进程有效;关掉进程即失效)")
	if !noOpen {
		_ = openWorkspacePath(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		fmt.Println("\n正在退出:保存会话…")
	case <-app.webQuit():
		// 前端「退出 OneCreat」按钮:与 Ctrl-C 同一条优雅关闭路径。
		fmt.Println("\n收到退出请求:保存会话…")
	}

	// 与 Wails 的 OnShutdown 走同一个函数:每个标签快照 + 关 controller(含 MCP 子进程)。
	app.shutdown(ctx)
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	return nil
}

// isLoopback 判断绑定地址是否是回环(含未解析的 "localhost")。
func isLoopback(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// displayHost 把 0.0.0.0 / :: 这类"全接口"绑定换成可点的回环地址。
func displayHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return host
	}
}
