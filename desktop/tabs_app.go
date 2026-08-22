package main

// 标签运行时与项目切换的 transport facade:实现在 tabRuntimeService 里
// (见 tab_runtime.go)。下面几个非导出方法保留原名,是包内其它文件的调用点。

import (
	"reasonix/internal/workspace"
)

// workspace 是当前选中的项目文件夹。需要解析 UI 路径的调用方(文件浏览、知识库、
// 硬件面板)用它,而不是 os.Getwd:进程工作目录在启动时就固定了,不再跟着用户
// 打开的项目走。
func (a *App) workspace() workspace.Context { return a.rt.Workspace() }

// workspaceRoot 是 workspace().Root(),带进程 cwd 兜底,给需要一个具体目录的调用点。
func (a *App) workspaceRoot() string { return a.rt.Root() }

// tabUpdate 按 id 写回一个标签的运行时,标签已被关闭时返回 false。
func (a *App) tabUpdate(id string, fn func(rt *tabRuntime)) bool { return a.rt.update(id, fn) }

// buildTab 为一个标签装配它自己的 controller。
func (a *App) buildTab(tabID string) { a.rt.BuildTab(tabID) }

// rebuildTabByID 用当前 config + 环境重建指定标签的 controller。
func (a *App) rebuildTabByID(tabID string) { a.rt.RebuildTab(tabID) }

// rebuildAllTabs 重建每一个标签的 controller(切档 / 登录 / 登出后必须全量重建)。
func (a *App) rebuildAllTabs() { a.rt.RebuildAllTabs() }

// anyTabRunning 报告是否有任何标签正在跑回合。
func (a *App) anyTabRunning() bool { return a.rt.AnyTabRunning() }

// SetModel 换掉活动标签的模型并带过历史。
func (a *App) SetModel(name string) error { return a.rt.SetModel(name) }

// SwitchWorkspace 把活动标签切到另一个项目文件夹。
func (a *App) SwitchWorkspace(dir string) (string, error) { return a.rt.SwitchWorkspace(dir) }

// PickWorkspace 弹出原生文件夹选择框。
func (a *App) PickWorkspace() (string, error) { return a.rt.PickWorkspace() }

// ListWorkspaces 列出最近打开过的项目文件夹。
func (a *App) ListWorkspaces() []WorkspaceMeta { return a.rt.ListWorkspaces() }
