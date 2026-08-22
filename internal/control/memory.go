package control

// memoryService owns the session's project/user memory: the loaded snapshot and
// the notes queued to ride the *next* turn.
//
// Split out of control.Controller in Plan 07. The pairing is the whole point, and
// it is a DeepSeek-prefix-cache decision rather than a filing one:
//
//	The snapshot folds into the **cache-stable system prefix** at build time.
//	A memory written mid-session therefore cannot go there — rewriting the prefix
//	would reset the prompt cache for the rest of the session. So every write also
//	queues a turn-tail note, which Compose drains onto the next outgoing turn.
//	The change applies immediately, costs one turn's tokens, and folds into the
//	prefix for free on the next session.
//
// set is treated as an immutable snapshot guarded by mu: reads take the lock and
// return the pointer; writes mutate disk then swap in a freshly discovered
// snapshot. All of these are no-ops returning "" when memory is disabled.

import (
	"fmt"
	"strings"
	"sync"

	"reasonix/internal/memory"
)

type memoryService struct {
	mu  sync.Mutex
	set *memory.Set
	// pending holds memory notes added mid-session (via "#" quick-add, a memory
	// edit, or the model's remember/forget tool) that haven't yet been folded into
	// a turn. Compose drains it onto the next outgoing turn — never into the
	// cache-stable system prefix.
	pending []string
}

func newMemoryService(set *memory.Set) *memoryService { return &memoryService{set: set} }

// DrainPending returns the queued turn-tail notes and clears them. Compose calls
// it once per outgoing turn.
func (m *memoryService) DrainPending() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	notes := m.pending
	m.pending = nil
	return notes
}

// QuickAdd appends a one-line note to the doc-memory file for scope (project
// REASONIX.md by default) — the write side of "#<note>". Returns the file written.
func (m *memoryService) QuickAdd(scope memory.Scope, note string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set == nil {
		return "", nil
	}
	path := m.set.DocPath(scope)
	if path == "" {
		return "", fmt.Errorf("no target file for memory scope %q", scope)
	}
	if err := memory.AppendDoc(path, note); err != nil {
		return "", err
	}
	m.pending = append(m.pending, note)
	m.refreshLocked()
	return path, nil
}

// SaveDoc overwrites a recognized memory doc with body — the save side of the
// desktop panel's in-place editor. Returns the file written.
func (m *memoryService) SaveDoc(path, body string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set == nil {
		return "", nil
	}
	written, err := m.set.WriteDoc(path, body)
	if err != nil {
		return "", err
	}
	// Inject the new content once on the next turn: the cached prefix still holds
	// the pre-edit version this session, so handing the model the current text
	// avoids a stale-guidance gap until the next session re-folds it into the
	// prefix. Trimmed to a single tail note (drained by Compose), not per-turn.
	m.pending = append(m.pending,
		"Memory file "+written+" was just edited. Its current contents:\n"+strings.TrimSpace(body))
	m.refreshLocked()
	return written, nil
}

// ForgetMemory deletes a saved auto-memory by name — the panel/TUI delete action,
// the manual counterpart to the model's `forget` tool. It queues a turn-tail note
// so the deletion applies this session (the cached prefix still lists the fact
// until the next session re-folds the index).
func (m *memoryService) Forget(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set == nil {
		return nil
	}
	if err := m.set.Store.Delete(name); err != nil {
		return err
	}
	m.pending = append(m.pending,
		"Deleted memory \""+name+"\" — disregard its line still shown in the saved-memories index until next session.")
	m.refreshLocked()
	return nil
}

// QueueMemory implements memory.Queue: when the model runs the remember/forget
// tool, the tool calls this with a note that rides the next turn so the change
// applies this session without touching the cache-stable prefix. It also
// refreshes the snapshot a memory panel reads.
func (m *memoryService) QueueMemory(note string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, note)
	m.refreshLocked()
}

// Memory returns the loaded memory snapshot (nil when memory is disabled), for
// frontends that surface a memory panel or the /memory command. The returned
// *Set is immutable — mutations go through QuickAdd / SaveDoc.
func (m *memoryService) Set() *memory.Set {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set
}

// refreshMemoryLocked re-discovers memory from disk so a later Memory() reflects
// a just-applied write. Caller holds m.mu.
func (m *memoryService) refreshLocked() {
	if m.set == nil {
		return
	}
	m.set = memory.Load(memory.Options{CWD: m.set.CWD, UserDir: m.set.UserDir})
}
