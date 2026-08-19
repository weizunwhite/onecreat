//go:build !web

package main

// 非 Web(Wails 桌面版)构建下的更新检查桩:返回 (nil, false) 表示「不接管」,
// CheckUpdate 于是走 updater_app.go 里刻意禁用的路径,行为与历史完全一致。
func (a *App) webUpdateOverride() (*UpdateInfo, bool) { return nil, false }

// webDownloadURL 在桌面版下无 Web 专属下载页,返回空串 → OpenDownloadPage 用默认发布页。
func webDownloadURL() string { return "" }
