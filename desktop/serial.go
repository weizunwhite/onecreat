package main

// 串口监视器的 transport facade:方法照旧挂在 *App 上(Wails 绑定 / Web RPC 都按
// 平面方法名分发),实现全在 serialService 里。App 不再持有串口状态。

import (
	"context"
	"encoding/json"
)

// SerialPorts 列出可用串口(给面板下拉)。
func (a *App) SerialPorts() []string { return a.serial.Ports() }

// SerialOpen 按指定波特率打开串口并开始持续读。
func (a *App) SerialOpen(portName string, baud int) SerialResult {
	return a.serial.Open(portName, baud)
}

// SerialWrite 往当前串口写一段数据。
func (a *App) SerialWrite(data string) SerialResult { return a.serial.Write(data) }

// SerialClose 关闭当前串口连接(幂等)。
func (a *App) SerialClose() { a.serial.Close() }

// serialReleaseForToolUse 是 boot.Build 的 PreToolUse 回调,见 serialService.ReleaseForToolUse。
func (a *App) serialReleaseForToolUse(ctx context.Context, name string, args json.RawMessage) {
	a.serial.ReleaseForToolUse(ctx, name, args)
}
