package control

// sessionStore owns where this conversation persists and the act of persisting
// it: the session directory, the currently active file, and the auto-save that
// runs after every turn so a crash loses at most one in-flight prompt.
//
// Split out of control.Controller in Plan 07. It deliberately does *not* own the
// message log — that lives on the agent's Session, single-writer by design (only
// the run-loop goroutine appends). This type owns the *file*, and reads the log
// through Snapshot() whenever it needs a consistent copy.
//
// Changing the active path is a two-part operation the Controller sequences:
// this store repoints the file, and the checkpoint service rebinds to the new
// session. Neither knows about the other.

import (
	"log/slog"
	"sync"

	"reasonix/internal/agent"
	"reasonix/internal/provider"
)

type sessionStore struct {
	// dir is where new session files land; "" disables persistence entirely.
	dir string
	// exec is the agent whose Session is being persisted; nil in headless tests.
	exec *agent.Agent

	mu   sync.Mutex
	path string
}

func newSessionStore(dir, path string, exec *agent.Agent) *sessionStore {
	return &sessionStore{dir: dir, path: path, exec: exec}
}

// SetPath pins where auto-save lands. The caller is responsible for rebinding
// checkpoints to the same session.
func (s *sessionStore) SetPath(p string) {
	s.mu.Lock()
	s.path = p
	s.mu.Unlock()
}

// Adopt seeds the session from a loaded transcript and pins the active file to
// its path so auto-save keeps appending there.
func (s *sessionStore) Adopt(sess *agent.Session, path string) {
	if s.exec != nil {
		s.exec.SetSession(sess)
	}
	s.SetPath(path)
}

// Snapshot writes the executor's conversation to the active session file. No-op
// when persistence is unavailable or the session has never been used (no user
// interaction). Called after every turn so a crash loses at most one in-flight
// prompt.
func (s *sessionStore) Save() error {
	return s.save(false)
}

// SnapshotActivity writes the active conversation and marks the session as
// recently active. Use it only after a real user/model turn changes the
// transcript; switch/close snapshots should call Snapshot so they do not reorder
// recent-session pickers.
func (s *sessionStore) SaveActivity() error {
	return s.save(true)
}

func (s *sessionStore) save(markActivity bool) error {
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()
	if s.exec == nil || path == "" {
		return nil
	}
	sess := s.exec.Session()
	if !sess.HasContent() {
		return nil
	}
	if !markActivity {
		if _, err := agent.EnsureBranchMeta(path); err != nil {
			return err
		}
	}
	if err := sess.Save(path); err != nil {
		return err
	}
	if markActivity {
		return agent.TouchBranchMeta(path)
	}
	return nil
}

func (s *sessionStore) MessageCount() int {
	if s.exec == nil {
		return 0
	}
	return len(s.exec.Session().Snapshot())
}

func (s *sessionStore) SaveActivityIfChanged(startMessages int) {
	if s.MessageCount() <= startMessages {
		return
	}
	if err := s.SaveActivity(); err != nil {
		slog.Warn("controller: activity snapshot", "err", err)
	}
}

// SessionDir reports the directory new session files land in ("" disables
// persistence), so the caller can decide whether to mint a path.
func (s *sessionStore) Dir() string { return s.dir }

// SessionPath reports the file the current conversation auto-saves to ("" when
// persistence is disabled), so a history view can mark the active session.
func (s *sessionStore) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// History returns the executor's current message log (for repopulating a
// resumed frontend's view).
func (s *sessionStore) History() []provider.Message {
	if s.exec == nil {
		return nil
	}
	return s.exec.Session().Snapshot() // copy — a turn may be appending concurrently
}
