package main

// sessionService 是「会话」那一片的后端:历史会话列表、改名 / 删除 / 恢复 / 预览,
// 以及每回合结束后的自动落盘。
//
// 自动落盘是按标签单飞的:后台标签跑完一轮也各存各的 session 文件,不串到活动标签;
// 重叠的请求合并成一次尾随写。这份单飞状态过去挂在 App 上,现在归它自己。
//
// 它唯一的依赖是标签注册表 —— 会话属于标签,「活动会话」就是活动标签的会话。

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"reasonix/internal/control"
	"sync"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/provider"
)

type sessionService struct {
	tabs *tabManager

	// 按 tab 单飞的自动落盘状态。
	saveMu    sync.Mutex
	saving    map[string]bool
	saveAgain map[string]bool
}

func newSessionService(tabs *tabManager) *sessionService {
	return &sessionService{tabs: tabs, saving: map[string]bool{}, saveAgain: map[string]bool{}}
}

// SessionMeta summarises one saved session for the history panel.
type SessionMeta struct {
	Path           string `json:"path"`
	Preview        string `json:"preview"`         // first user message
	Title          string `json:"title,omitempty"` // user-chosen name, when set (overrides preview)
	Turns          int    `json:"turns"`
	CreatedAt      int64  `json:"createdAt"`      // unix milliseconds
	LastActivityAt int64  `json:"lastActivityAt"` // unix milliseconds
	ModTime        int64  `json:"modTime"`        // compatibility alias for lastActivityAt
	Current        bool   `json:"current"`
	Cwd            string `json:"cwd,omitempty"`  // workspace path at session creation, for sidebar grouping
	Kind           string `json:"kind,omitempty"` // 会话类型(如 "hardware");空=普通对话。历史侧栏据此区分垂直
}

// ListSessions returns the saved sessions newest-first for the history panel,
// marking the one the current conversation is writing to and attaching any
// user-chosen titles.
func (s *sessionService) List() []SessionMeta {
	dir := config.SessionDir()
	infos, err := agent.ListSessions(dir)
	if err != nil {
		return []SessionMeta{}
	}
	titles := loadSessionTitles(dir)
	cwds := loadSessionCwds(dir)
	kinds := loadSessionKinds(dir)
	ctrl := s.tabs.Ctrl("")
	cur := ""
	if ctrl != nil {
		cur = ctrl.SessionPath()
	}
	out := make([]SessionMeta, 0, len(infos))
	for _, s := range infos {
		out = append(out, SessionMeta{
			Path:           s.Path,
			Preview:        s.Preview,
			Title:          titles[filepath.Base(s.Path)],
			Turns:          s.Turns,
			CreatedAt:      s.CreatedAt.UnixMilli(),
			LastActivityAt: s.LastActivityAt.UnixMilli(),
			ModTime:        s.LastActivityAt.UnixMilli(),
			Current:        s.Path == cur,
			Cwd:            cwds[filepath.Base(s.Path)],
			Kind:           kinds[filepath.Base(s.Path)],
		})
	}
	return out
}

// MarkSessionKind 给当前活动会话打类型标(写入一次即定),供历史侧栏区分垂直。前端在真正
// 进入某垂直定制流程时调用(如硬件面板跑编译/烧录/生成代码)——硬件视图是切 mainView、
// 不换 tab,所以只能由前端显式标记,后端无法从 tab 类型推断(Phase 1 收尾)。
func (s *sessionService) MarkKind(kind string) {
	ctrl := s.tabs.Ctrl("")
	if ctrl == nil {
		return
	}
	_ = rememberSessionKind(config.SessionDir(), ctrl.SessionPath(), kind)
}

// sessionPathsInUse 返回所有标签 controller 当前正在写的 session 路径 → 标签 id 映射;
// exclude 跳过指定标签(空串=不跳过)。先在锁内取出 controller 列表再调 SessionPath(),
// 避免持 a.mu 时回调进 controller 锁。用于跨标签防双写同一 session 文件(A4)。
func (s *sessionService) pathsInUse(exclude string) map[string]string {
	type pair struct {
		id   string
		ctrl *control.Controller
	}
	tabs := s.tabs.List()
	pairs := make([]pair, 0, len(tabs))
	for _, t := range tabs {
		if t.ID == exclude {
			continue
		}
		if ctrl := s.tabs.Ctrl(t.ID); ctrl != nil {
			pairs = append(pairs, pair{t.ID, ctrl})
		}
	}
	out := map[string]string{}
	for _, p := range pairs {
		if sp := p.ctrl.SessionPath(); sp != "" {
			out[sp] = p.id
		}
	}
	return out
}

// DeleteSession removes a saved session (and its title). It refuses any session a
// tab is currently writing to — the active one (auto-save would recreate it) and
// any background tab's session too, so a background task can't be deleted mid-run (A4).
func (s *sessionService) Delete(path string) error {
	active := s.tabs.ActiveID()
	if tab := s.pathsInUse("")[path]; tab != "" {
		if tab == active {
			return errActiveSession
		}
		return fmt.Errorf("该会话正在另一个任务标签中使用,无法删除;请先在那个标签里新建会话")
	}
	return deleteSessionFile(config.SessionDir(), path)
}

// RenameSession sets a custom display name for a session (empty clears it back to
// the preview). It only affects the history panel; the file on disk is unchanged.
func (s *sessionService) Rename(path, title string) error {
	return setSessionTitle(config.SessionDir(), path, title)
}

// ResumeSession snapshots the current conversation, then loads the session at
// path and continues it — auto-save keeps appending to that file. The model and
// working folder are unchanged (same controller); only the transcript is swapped.
// Returns the resumed messages for the frontend to render.
func (s *sessionService) Resume(path string) ([]HistoryMessage, error) {
	v, _ := s.tabs.View("")
	ctrl, active := v.ctrl, v.id
	if ctrl == nil {
		return []HistoryMessage{}, nil
	}
	// 目标 session 若正被「其它标签」写入,拒绝 resume——两个 controller 各自整文件快照
	// 同一个 .jsonl 会后写覆盖先写、互丢对话轮次(A4)。
	if tab := s.pathsInUse(active)[path]; tab != "" {
		return nil, fmt.Errorf("该会话已在另一个任务标签中打开;请切到那个标签继续,而不是在此重复打开")
	}
	loaded, err := agent.LoadSession(path)
	if err != nil {
		return nil, err
	}
	_ = ctrl.Snapshot() // persist the current session before switching away
	ctrl.Resume(loaded, path)
	return s.History(), nil
}

// PreviewSession reads a saved session for display only. It does not snapshot or
// swap the active controller, so the history drawer can call it while a turn runs.
func (s *sessionService) Preview(path string) ([]HistoryMessage, error) {
	return previewSessionMessages(config.SessionDir(), path)
}

// HistoryMessage is one prior turn, for the frontend to repopulate its transcript
// after a reload.
type HistoryMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning,omitempty"`
}

// History returns the session's message log.
func (s *sessionService) History() []HistoryMessage {
	ctrl := s.tabs.Ctrl("")
	if ctrl == nil {
		return nil
	}
	msgs := ctrl.History()
	return historyMessages(msgs, sessionDisplayResolver(config.SessionDir(), ctrl.SessionPath()))
}

func historyMessages(msgs []provider.Message, resolveUserContent func(string) string) []HistoryMessage {
	out := make([]HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if m.Role == provider.RoleUser {
			content = resolveUserContent(m.Content)
		}
		reasoning := ""
		if m.Role == provider.RoleAssistant {
			reasoning = m.ReasoningContent
		}
		out = append(out, HistoryMessage{Role: string(m.Role), Content: content, Reasoning: reasoning})
	}
	return out
}

func previewSessionMessages(sessionDir, path string) ([]HistoryMessage, error) {
	loaded, err := agent.LoadSession(path)
	if err != nil {
		return nil, err
	}
	return historyMessages(loaded.Snapshot(), sessionDisplayResolver(sessionDir, path)), nil
}

// scheduleSnapshot kicks a single-flight background save of one tab's session;
// a request arriving while one runs sets a trailing pass so the final state lands.
func (s *sessionService) ScheduleSnapshot(tabID string) {
	s.saveMu.Lock()
	if s.saving[tabID] {
		s.saveAgain[tabID] = true
		s.saveMu.Unlock()
		return
	}
	s.saving[tabID] = true
	s.saveMu.Unlock()
	go s.snapshotLoop(tabID)
}

func (s *sessionService) snapshotLoop(tabID string) {
	for {
		ctrl := s.tabs.Ctrl(tabID)
		if ctrl != nil {
			if err := ctrl.Snapshot(); err != nil {
				slog.Warn("desktop: per-turn snapshot", "err", err)
			}
		}
		s.saveMu.Lock()
		if s.saveAgain[tabID] {
			s.saveAgain[tabID] = false
			s.saveMu.Unlock()
			continue
		}
		delete(s.saving, tabID)
		delete(s.saveAgain, tabID)
		s.saveMu.Unlock()
		return
	}
}
