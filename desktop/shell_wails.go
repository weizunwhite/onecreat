//go:build !web

package main

import (
	"context"
	"errors"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// wailsShell 是原生桌面(Wails)下的 Shell 实现:每个方法都是对 wails runtime 的
// 直通,行为与抽出 Shell 之前逐字一致。ctx 从 App 上惰性读取——Wails 的 context 要
// 到 startup() 才存在,而 shell 在 NewApp() 时就建好了。
type wailsShell struct {
	app *App
}

// newShell 由 NewApp 调用,按构建标签选择宿主实现。
func newShell(a *App) Shell { return &wailsShell{app: a} }

func (s *wailsShell) ctx() (c context.Context, ok bool) {
	if s.app == nil || s.app.ctx == nil {
		return nil, false
	}
	return s.app.ctx, true
}

func (s *wailsShell) Emit(channel string, payload any) {
	if ctx, ok := s.ctx(); ok {
		runtime.EventsEmit(ctx, channel, payload)
	}
}

func toWailsOptions(opts DialogOptions) runtime.OpenDialogOptions {
	out := runtime.OpenDialogOptions{
		Title:            opts.Title,
		DefaultDirectory: opts.DefaultDirectory,
	}
	for _, f := range opts.Filters {
		out.Filters = append(out.Filters, runtime.FileFilter{DisplayName: f.DisplayName, Pattern: f.Pattern})
	}
	return out
}

func (s *wailsShell) OpenDirectoryDialog(opts DialogOptions) (string, error) {
	ctx, ok := s.ctx()
	if !ok {
		return "", nil
	}
	return runtime.OpenDirectoryDialog(ctx, toWailsOptions(opts))
}

func (s *wailsShell) OpenFileDialog(opts DialogOptions) (string, error) {
	ctx, ok := s.ctx()
	if !ok {
		return "", errors.New("file picker not ready")
	}
	return runtime.OpenFileDialog(ctx, toWailsOptions(opts))
}

func (s *wailsShell) OpenMultipleFilesDialog(opts DialogOptions) ([]string, error) {
	ctx, ok := s.ctx()
	if !ok {
		return nil, nil
	}
	return runtime.OpenMultipleFilesDialog(ctx, toWailsOptions(opts))
}

func (s *wailsShell) BrowserOpenURL(url string) {
	if ctx, ok := s.ctx(); ok {
		runtime.BrowserOpenURL(ctx, url)
	}
}

func (s *wailsShell) RaiseWindow() {
	ctx, ok := s.ctx()
	if !ok {
		return
	}
	runtime.WindowShow(ctx)
	runtime.WindowUnminimise(ctx)
	runtime.WindowCenter(ctx)
}

// Quit 关闭 Wails 应用(触发 OnShutdown → app.shutdown)。
func (s *wailsShell) Quit() {
	if ctx, ok := s.ctx(); ok {
		runtime.Quit(ctx)
	}
}
