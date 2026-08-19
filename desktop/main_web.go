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
		saveWorkspace(abs) // 让 ensureWorkspace 在 startup 里沿用它
	}

	sub, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		return fmt.Errorf("定位内嵌前端失败: %w", err)
	}

	token, err := newWebToken()
	if err != nil {
		return fmt.Errorf("生成访问 token 失败: %w", err)
	}

	app := NewApp()
	srv, err := newWebServer(app, sub, app.webEvents(), token, host, port, remote)
	if err != nil {
		return err
	}

	// 先监听再 startup:端口被占的话立刻失败,不必等 boot.Build 跑完。
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return fmt.Errorf("监听 %s:%d 失败(端口被占用?): %w", host, port, err)
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

	url := fmt.Sprintf("http://%s/?token=%s", net.JoinHostPort(displayHost(host), fmt.Sprint(port)), token)
	fmt.Println("OneCreat Web 已启动:")
	fmt.Println("  " + url)
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
