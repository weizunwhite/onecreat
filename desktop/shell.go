package main

import "errors"

// Shell 是「宿主外壳」抽象:把 App 对宿主环境(原本是 Wails runtime)的全部依赖收口
// 到一个接口上,于是同一份 App 既能跑在 Wails 原生窗口里,也能跑在「本地 HTTP 服务 +
// 浏览器」的 Web 模式下。
//
// 收口的四类能力:
//   - Emit:把事件推给前端(Wails → runtime.EventsEmit;Web → SSE 广播)
//   - 三个原生对话框:选文件夹 / 选单个文件 / 选多个文件
//   - BrowserOpenURL:用系统默认浏览器打开外链
//   - RaiseWindow:把原生窗口显示/取消最小化/居中(Web 模式无窗口,空实现)
//
// 实现见 shell_wails.go(//go:build !web)与 shell_web.go(//go:build web)。
type Shell interface {
	// Emit 把一条事件推给前端。channel 是事件通道名(如 "agent:event:main"),
	// payload 会被 JSON 序列化。必须是 goroutine 安全的:agent 循环会直接调它。
	Emit(channel string, payload any)

	// OpenDirectoryDialog 弹出目录选择对话框;返回 "" 表示用户取消。
	OpenDirectoryDialog(opts DialogOptions) (string, error)

	// OpenFileDialog 弹出单文件选择对话框;返回 "" 表示用户取消。
	OpenFileDialog(opts DialogOptions) (string, error)

	// OpenMultipleFilesDialog 弹出多文件选择对话框;返回空切片表示用户取消。
	OpenMultipleFilesDialog(opts DialogOptions) ([]string, error)

	// BrowserOpenURL 用系统默认浏览器打开外链(避免把 webview 导航走)。
	BrowserOpenURL(url string)

	// RaiseWindow 显示 + 取消最小化 + 居中原生窗口。Web 模式下是空操作。
	RaiseWindow()

	// Quit 请求优雅退出整个程序。Wails → runtime.Quit(关窗触发 OnShutdown);
	// Web → 通知本地服务的运行循环走 app.shutdown 并停服(见 shell_web.go)。
	Quit()
}

// DialogOptions 是原生对话框参数,字段刻意与 wails runtime.OpenDialogOptions 的
// 同名子集对齐,便于 wailsShell 直接透传。
type DialogOptions struct {
	Title            string
	DefaultDirectory string
	Filters          []FileFilter
}

// FileFilter 是文件类型过滤器(DisplayName 显示名,Pattern 形如 "*.txt;*.md")。
type FileFilter struct {
	DisplayName string
	Pattern     string
}

// ErrNoNativeDialog 是 Web 模式下调用原生对话框的统一错误:浏览器里没有可用的
// 系统对话框(而且后端要的是磁盘绝对路径,浏览器 File API 给不了)。
var ErrNoNativeDialog = errors.New("web 模式不支持原生文件对话框")

// noopShell 在 shell 尚未装配时兜底(单元测试里的 &App{} 就是这种情况):事件丢弃、
// 对话框直接报错、窗口操作空转。这样测试构造裸 App 不会 panic。
type noopShell struct{}

func (noopShell) Emit(string, any)                                  {}
func (noopShell) OpenDirectoryDialog(DialogOptions) (string, error) { return "", ErrNoNativeDialog }
func (noopShell) OpenFileDialog(DialogOptions) (string, error)      { return "", ErrNoNativeDialog }
func (noopShell) OpenMultipleFilesDialog(DialogOptions) ([]string, error) {
	return nil, ErrNoNativeDialog
}
func (noopShell) BrowserOpenURL(string) {}
func (noopShell) RaiseWindow()          {}
func (noopShell) Quit()                 {}

// sh 返回可用的 Shell:未装配时给 noopShell,免得每个调用点都判空。
func (a *App) sh() Shell {
	if a == nil || a.shell == nil {
		return noopShell{}
	}
	return a.shell
}
