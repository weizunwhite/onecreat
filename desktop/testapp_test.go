package main

import "context"

// newBareApp 装配一个「领域服务接好、但没有 Wails ctx、没有共享 Factory、也没有主
// 标签」的 App,给不想跑完整 startup 的单元测试用。
//
// 它替代过去到处出现的裸 &App{}:从 Plan 06 起 App 只是各领域服务的 transport
// facade,一个不接服务的 App 已经不是有效对象 —— 它的方法会打到 nil 服务上,测出来的
// 也不是产品行为。
func newBareApp(ctx context.Context, tabs *tabManager) *App {
	if tabs == nil {
		tabs = newTabManager()
	}
	a := &App{ctx: ctx, tabs: tabs}
	a.shell = newShell(a)
	a.wireServices()
	return a
}
