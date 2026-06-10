// Package agent wires a Provider, a tool Registry, and a Session into the
// harness loop that drives a coding task to completion.
package agent

import (
	"sync"

	"reasonix/internal/provider"
)

// Session holds the conversation history for one task. The run loop (one turn at
// a time) is the only writer, but a frontend can read History/Save from another
// goroutine while a turn appends, so mu guards Messages. Direct Messages reads on
// the run-loop goroutine stay lock-free (serial with its own writes); cross-
// goroutine access goes through Snapshot.
type Session struct {
	mu       sync.RWMutex
	Messages []provider.Message
}

// NewSession initializes a session with an optional system prompt.
func NewSession(system string) *Session {
	s := &Session{}
	if system != "" {
		s.Messages = append(s.Messages, provider.Message{Role: provider.RoleSystem, Content: system})
	}
	return s
}

// Add appends a message.
func (s *Session) Add(m provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, m)
}

// Replace swaps the whole message log — used by compaction, which rewrites the
// middle of the history.
func (s *Session) Replace(msgs []provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = msgs
}

// Truncate cuts the message log back to boundary (keeping msgs[:boundary]) under
// the lock, so a rewind from a serve/HTTP request goroutine doesn't race History
// reads. It returns false WITHOUT modifying anything when boundary is out of range
// (e.g. a stale checkpoint boundary after a compaction) — the caller then reports a
// failed rewind instead of silently cutting at the wrong place.
func (s *Session) Truncate(boundary int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if boundary < 0 || boundary > len(s.Messages) {
		return false
	}
	s.Messages = s.Messages[:boundary]
	return true
}

// Snapshot returns a copy of the messages, safe to read from another goroutine
// while a turn appends. Frontends (History, Save) use it instead of touching the
// live slice.
func (s *Session) Snapshot() []provider.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]provider.Message(nil), s.Messages...)
}

// HasContent returns true when the session carries at least one user,
// assistant, or tool message — i.e. more than just a system prompt. An
// "empty" conversation that has never been used should not be persisted.
func (s *Session) HasContent() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.Messages {
		if m.Role != provider.RoleSystem {
			return true
		}
	}
	return false
}
