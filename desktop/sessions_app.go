package main

// 会话面板的 transport facade:实现在 sessionService 里(见 sessions_service.go)。

// ListSessions 列出历史会话。
func (a *App) ListSessions() []SessionMeta { return a.sessions.List() }

// MarkSessionKind 给当前会话打上类型标记(chat / 任务等)。
func (a *App) MarkSessionKind(kind string) { a.sessions.MarkKind(kind) }

// DeleteSession 删除一个历史会话文件。
func (a *App) DeleteSession(path string) error { return a.sessions.Delete(path) }

// RenameSession 给一个历史会话改标题。
func (a *App) RenameSession(path, title string) error { return a.sessions.Rename(path, title) }

// ResumeSession 把一个历史会话装回活动标签,返回它的消息记录。
func (a *App) ResumeSession(path string) ([]HistoryMessage, error) {
	return a.sessions.Resume(path)
}

// PreviewSession 只读地预览一个历史会话的消息记录。
func (a *App) PreviewSession(path string) ([]HistoryMessage, error) {
	return a.sessions.Preview(path)
}

// History 返回活动标签当前会话的消息记录。
func (a *App) History() []HistoryMessage { return a.sessions.History() }
