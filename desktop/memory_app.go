package main

// 记忆面板的 transport facade:实现在 memoryService 里(见 memory_service.go)。

// Memory 返回记忆文档与事实的视图。
func (a *App) Memory() MemoryView { return a.memory.View() }

// Remember 往指定作用域写一条事实。
func (a *App) Remember(scope, note string) (string, error) { return a.memory.Remember(scope, note) }

// Forget 删掉一条已记住的事实。
func (a *App) Forget(name string) error { return a.memory.Forget(name) }

// SaveDoc 直接保存整篇记忆文档。
func (a *App) SaveDoc(path, body string) (string, error) { return a.memory.SaveDoc(path, body) }
