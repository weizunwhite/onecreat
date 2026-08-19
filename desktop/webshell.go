package main

// webShell 是 Web 模式(本地 HTTP 服务 + 浏览器当 UI)下的 Shell 实现。
//
//   - Emit          → 投给 SSE 广播器,由浏览器的单条 EventSource 收
//   - 三个对话框    → 浏览器里没有系统对话框,而且后端要的是磁盘绝对路径
//     (浏览器 File API 只给 File 对象,拿不到路径),所以一律
//     返回 ErrNoNativeDialog,由前端提示降级方案
//   - BrowserOpenURL→ 走系统 open / xdg-open / ShellExecute(与"在访达中打开"同一路径)
//   - RaiseWindow   → 无原生窗口,空操作
type webShell struct {
	events *eventBroadcaster
}

func newWebShell(events *eventBroadcaster) *webShell {
	return &webShell{events: events}
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
