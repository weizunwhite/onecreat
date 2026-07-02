// Package checkpoint is reasonix's snapshot-based edit safety net. Before a writer
// tool changes a file, the agent records the file's pre-edit content here, keyed
// to the current user turn; a frontend can then rewind the workspace (and, via the
// controller, the conversation) to an earlier turn.
//
// It is deliberately git-free (like Claude Code's rewind): snapshots live beside
// the session, never touch the user's git, and work in a non-git directory. Only
// edit-tool changes are tracked — bash side effects are not (a shell command's
// targets can't be known in advance), which is why the capture hook only fires for
// tools that can Preview their change.
package checkpoint

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"reasonix/internal/diff"
)

// FileSnap is one file's state at the moment it was first touched in a turn.
// Content == nil means the file did not exist then, so a restore deletes it.
type FileSnap struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
	// B64 marks Content as base64-encoded raw bytes. Set when the original content
	// was not valid UTF-8 (e.g. a binary file an edit tool read raw): json.Marshal
	// replaces invalid UTF-8 with U+FFFD, so persisting such content as a plain JSON
	// string would corrupt it on the persist→resume→restore round-trip. Base64 keeps
	// the bytes intact. Absent (false) on older checkpoints, whose content was UTF-8.
	B64 bool `json:"b64,omitempty"`
}

// Checkpoint anchors the pre-edit state of every distinct file touched during one
// user turn. MsgIndex is len(Session.Messages) at the turn's start — the
// conversation-rewind boundary — persisted so a resumed session can rewind the
// conversation and fork, not just the code.
type Checkpoint struct {
	Turn     int        `json:"turn"`
	Time     time.Time  `json:"time"`
	Prompt   string     `json:"prompt"`
	MsgIndex int        `json:"msgIndex"`
	Files    []FileSnap `json:"files"`
}

// Meta is the picker-facing summary of a checkpoint (no file contents).
type Meta struct {
	Turn   int
	Time   time.Time
	Prompt string
	Paths  []string
}

// Store holds a session's checkpoints in memory and, when dir is set, persists one
// JSON file per turn under it (cheap delete, corruption-isolated). All methods are
// safe for concurrent use — the agent snapshots from tool goroutines.
type Store struct {
	dir  string // <session>.ckpt/, or "" for in-memory only
	root string // workspace root, for restore path-escape guards

	mu   sync.Mutex
	done []*Checkpoint   // finalized turns
	cur  *Checkpoint     // the active turn's checkpoint
	seen map[string]bool // paths already snapshotted this turn (dedup)
	// boundsMin is the lowest turn whose persisted conversation-rewind boundary
	// (MsgIndex) can still be trusted. Compaction rewrites the message log in place,
	// so every pre-compaction checkpoint's MsgIndex goes stale; it is bumped (and
	// persisted) so a resumed session doesn't silently rewind to the wrong offset.
	// 0 means nothing has invalidated boundaries yet.
	boundsMin int
}

// New returns a store for the given checkpoint dir and workspace root, loading any
// checkpoints already persisted under dir. A "" dir disables persistence (the
// store still works in memory for the session).
func New(dir, root string) *Store {
	s := &Store{dir: dir, root: root, seen: map[string]bool{}}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("checkpoint: create dir failed, persistence disabled", "dir", dir, "err", err)
		}
		s.load()
	}
	return s
}

func (s *Store) load() {
	if b, err := os.ReadFile(filepath.Join(s.dir, boundsMinFile)); err == nil {
		if n, aerr := strconv.Atoi(strings.TrimSpace(string(b))); aerr == nil {
			s.boundsMin = n
		}
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var c Checkpoint
		if json.Unmarshal(b, &c) == nil {
			s.done = append(s.done, &c)
		}
	}
	sort.Slice(s.done, func(i, j int) bool { return s.done[i].Turn < s.done[j].Turn })
}

// boundsMinFile persists Store.boundsMin next to the per-turn checkpoints.
const boundsMinFile = "bounds-min.txt"

// Begin opens a checkpoint for a new user turn, finalizing the previous one. The
// prompt labels it in the picker; msgIndex is the conversation-rewind boundary.
func (s *Store) Begin(turn int, prompt string, msgIndex int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		s.done = append(s.done, s.cur)
	}
	s.cur = &Checkpoint{Turn: turn, Time: time.Now(), Prompt: prompt, MsgIndex: msgIndex}
	s.seen = map[string]bool{}
	s.persist(s.cur)
}

// Bounds returns turn → MsgIndex over all checkpoints (persisted + current), so
// the controller can rebuild its conversation-rewind boundaries after loading a
// resumed session's checkpoints from disk.
func (s *Store) Bounds() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[int]int, len(s.done)+1)
	for _, c := range s.done {
		m[c.Turn] = c.MsgIndex
	}
	if s.cur != nil {
		m[s.cur.Turn] = s.cur.MsgIndex
	}
	return m
}

// Snapshot records the pre-edit state of the file a writer is about to change.
// Only the first touch of a path in the current turn is kept (that is its
// turn-start content). A no-op before the first Begin.
func (s *Store) Snapshot(ch diff.Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || ch.Path == "" || s.seen[ch.Path] {
		return
	}
	s.seen[ch.Path] = true
	snap := FileSnap{Path: ch.Path}
	if ch.Kind != diff.Create { // create == file didn't exist → leave nil (restore deletes)
		old := ch.OldText
		if !utf8.ValidString(old) {
			// 非法 UTF-8(如二进制文件被原样读入):base64 存,避免 json.Marshal 把非法字节
			// 换成 U+FFFD 而在 resume 后 rewind 时写回损坏内容。
			enc := base64.StdEncoding.EncodeToString([]byte(old))
			snap.Content = &enc
			snap.B64 = true
		} else {
			snap.Content = &old
		}
	}
	s.cur.Files = append(s.cur.Files, snap)
	s.persist(s.cur)
}

func (s *Store) persist(c *Checkpoint) {
	if s.dir == "" {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(s.dir, fmt.Sprintf("turn-%d.json", c.Turn)), b, 0o644); err != nil {
		slog.Warn("checkpoint: persist failed", "turn", c.Turn, "err", err)
	}
}

// Prune drops every checkpoint for turn >= fromTurn, in memory and on disk. A
// conversation rewind to turn T abandons turns >= T; without this their snapshots
// linger, and because the resumed timeline re-uses those turn numbers a later
// RestoreCode/Bounds can pick the abandoned timeline's snapshot — restoring stale
// content, or (when the abandoned snapshot recorded a file's creation, Content==nil)
// silently deleting a file the current timeline legitimately has. Called by the
// controller after a conversation rewind.
func (s *Store) Prune(fromTurn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.done[:0]
	for _, c := range s.done {
		if c.Turn >= fromTurn {
			s.removeFile(c.Turn)
			continue
		}
		kept = append(kept, c)
	}
	s.done = kept
	if s.cur != nil && s.cur.Turn >= fromTurn {
		s.removeFile(s.cur.Turn)
		s.cur = nil
		s.seen = map[string]bool{}
	}
}

// removeFile deletes a turn's persisted checkpoint. Caller holds the lock.
func (s *Store) removeFile(turn int) {
	if s.dir == "" {
		return
	}
	if err := os.Remove(filepath.Join(s.dir, fmt.Sprintf("turn-%d.json", turn))); err != nil && !os.IsNotExist(err) {
		slog.Warn("checkpoint: prune remove failed", "turn", turn, "err", err)
	}
}

// InvalidateBounds records that only turns >= minValidTurn have trustworthy
// conversation-rewind boundaries — called when compaction rewrites the log in
// place, invalidating every earlier checkpoint's MsgIndex. Persisted so the mark
// survives a resume. Monotonic: a later (higher) compaction threshold wins.
func (s *Store) InvalidateBounds(minValidTurn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if minValidTurn <= s.boundsMin {
		return
	}
	s.boundsMin = minValidTurn
	if s.dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(s.dir, boundsMinFile), []byte(strconv.Itoa(s.boundsMin)), 0o644); err != nil {
		slog.Warn("checkpoint: persist bounds-min failed", "err", err)
	}
}

// MinValidBoundTurn returns the lowest turn whose persisted conversation boundary
// can still be trusted (0 when no compaction has invalidated any). Bounds for
// turns below it were made stale by an in-place compaction and must be ignored.
func (s *Store) MinValidBoundTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundsMin
}

// NextTurn returns the turn number a new checkpoint should take: one past the
// highest existing turn (0 when empty), so a resumed session keeps numbering
// without colliding with checkpoints loaded from disk.
func (s *Store) NextTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := 0
	for _, c := range s.done {
		if c.Turn >= next {
			next = c.Turn + 1
		}
	}
	if s.cur != nil && s.cur.Turn >= next {
		next = s.cur.Turn + 1
	}
	return next
}

// List returns every checkpoint's metadata, oldest turn first.
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.done)+1)
	for _, c := range s.all() {
		paths := make([]string, len(c.Files))
		for i, f := range c.Files {
			paths[i] = f.Path
		}
		out = append(out, Meta{Turn: c.Turn, Time: c.Time, Prompt: c.Prompt, Paths: paths})
	}
	return out
}

// all returns done + cur in turn order. Caller holds the lock.
func (s *Store) all() []*Checkpoint {
	cps := append([]*Checkpoint(nil), s.done...)
	if s.cur != nil {
		cps = append(cps, s.cur)
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].Turn < cps[j].Turn })
	return cps
}

// RestoreCode reverts the workspace to its state at the start of turn `fromTurn`:
// for every file touched in turn fromTurn or later, it writes back that file's
// earliest recorded content (or deletes it when the earliest snapshot was nil).
// Returns the paths written and deleted.
func (s *Store) RestoreCode(fromTurn int) (written, deleted []string, err error) {
	s.mu.Lock()
	// earliest snapshot per path across checkpoints >= fromTurn (turn order → first wins).
	earliest := map[string]FileSnap{}
	seen := map[string]bool{}
	order := []string{}
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		for _, f := range c.Files {
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			earliest[f.Path] = f
			order = append(order, f.Path)
		}
	}
	root := s.root
	s.mu.Unlock()

	for _, p := range order {
		abs, gerr := safePath(root, p)
		if gerr != nil {
			err = gerr
			continue
		}
		f := earliest[p]
		if f.Content == nil {
			if rmErr := os.Remove(abs); rmErr == nil {
				deleted = append(deleted, p)
			} else if !os.IsNotExist(rmErr) {
				err = rmErr
			}
			continue
		}
		data := []byte(*f.Content)
		if f.B64 { // 二进制快照:还原原始字节(见 FileSnap.B64)
			decoded, derr := base64.StdEncoding.DecodeString(*f.Content)
			if derr != nil {
				err = derr
				continue
			}
			data = decoded
		}
		if mkErr := os.MkdirAll(filepath.Dir(abs), 0o755); mkErr != nil {
			err = mkErr
			continue
		}
		if wErr := os.WriteFile(abs, data, 0o644); wErr != nil {
			err = wErr
			continue
		}
		written = append(written, p)
	}
	return written, deleted, err
}

// safePath resolves p against root and rejects anything escaping it — restore
// must never write outside the workspace, even if a snapshot path is hostile or
// the project moved since it was taken.
func safePath(root, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	if root != "" {
		r := filepath.Clean(root)
		if abs != r && !strings.HasPrefix(abs, r+string(os.PathSeparator)) {
			return "", fmt.Errorf("checkpoint path %q escapes workspace %q", p, root)
		}
	}
	return abs, nil
}
