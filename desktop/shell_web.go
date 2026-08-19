//go:build web

package main

// Web 模式下的 Shell 装配。webShell 需要一个 SSE 广播器,但 NewApp() 时 HTTP 服务
// 还没起来,所以先建广播器、由 main_web.go 起服务时复用同一个实例(通过
// App.webEvents 取回)。这样 NewApp 的签名在两种模式下保持一致。
func newShell(a *App) Shell {
	sh := newWebShell(newEventBroadcaster())
	return sh
}

// webEvents 取出当前 shell 的 SSE 广播器(仅 Web 模式有意义)。
func (a *App) webEvents() *eventBroadcaster {
	if s, ok := a.sh().(*webShell); ok {
		return s.events
	}
	return nil
}
