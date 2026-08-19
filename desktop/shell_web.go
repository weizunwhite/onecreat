//go:build web

package main

// webShell 是 Web 模式(本地 HTTP 服务 + 浏览器当 UI)下的 Shell 实现。
//
//   - Emit           → 投给 SSE 广播器,由浏览器的单条 EventSource 收
//   - 三个对话框     → 浏览器里没有系统对话框,而且后端要的是磁盘绝对路径
//     (浏览器 File API 只给 File 对象、拿不到路径),所以一律
//     返回 ErrNoNativeDialog,由前端提示降级方案
//   - BrowserOpenURL → 走系统 open / xdg-open / ShellExecute(与「在访达中打开」同一路径)
//   - RaiseWindow    → 无原生窗口,空操作
type webShell struct {
	events *eventBroadcaster
	// quit 是「请求退出」信号:前端点退出按钮 → App.Quit → webShell.Quit 往这里投一下,
	// main_web 的运行循环收到后走 app.shutdown + 停服。缓冲 1 + 非阻塞发,重复点也不阻塞。
	quit chan struct{}
}

// newShell 由 NewApp 调用。webShell 需要一个 SSE 广播器,但 NewApp() 时 HTTP 服务
// 还没起来,所以先在这里建好,main_web.go 起服务时用 App.webEvents() 取回同一个实例
// —— 这样 NewApp 的签名在两种模式下保持一致。
func newShell(*App) Shell {
	return &webShell{events: newEventBroadcaster(), quit: make(chan struct{}, 1)}
}

// webEvents 取出当前 shell 的 SSE 广播器。
func (a *App) webEvents() *eventBroadcaster {
	if s, ok := a.sh().(*webShell); ok {
		return s.events
	}
	return nil
}

// webQuit 取出「请求退出」信号 channel,供 main_web 的运行循环 select。
func (a *App) webQuit() <-chan struct{} {
	if s, ok := a.sh().(*webShell); ok {
		return s.quit
	}
	return nil
}

func (s *webShell) Emit(channel string, payload any) {
	if s.events != nil {
		s.events.Emit(channel, payload)
	}
}

func (s *webShell) OpenDirectoryDialog(DialogOptions) (string, error) {
	return "", ErrNoNativeDialog
}

func (s *webShell) OpenFileDialog(DialogOptions) (string, error) {
	return "", ErrNoNativeDialog
}

func (s *webShell) OpenMultipleFilesDialog(DialogOptions) ([]string, error) {
	return nil, ErrNoNativeDialog
}

func (s *webShell) BrowserOpenURL(url string) {
	_ = openWorkspacePath(url) // open/xdg-open/ShellExecute 对 http(s) URL 同样适用
}

func (s *webShell) RaiseWindow() {}

// Quit 往 quit channel 非阻塞投一个信号;main_web 的运行循环收到后优雅停服。
func (s *webShell) Quit() {
	select {
	case s.quit <- struct{}{}:
	default: // 已经在退了,不重复
	}
}
