// Package session owns what OneCreat knows about a conversation, as opposed to
// what the engine that ran it knows.
//
// The split matters, and Plan 11 exists to make it explicit (A15). An engine
// owns its own raw transcript — for the native engine that is a JSONL file of
// provider messages, and it is the only thing that should ever parse it. What
// OneCreat owns is the *session*: its identity, which project it ran in, which
// engine ran it, what the user called it, when it was created and last touched,
// and a reference to wherever the engine put the transcript.
//
// Before this package there was no such owner. Identity was a filename
// (`<timestamp>-<model>.jsonl`), and the metadata lived in four parallel sidecar
// maps that only the desktop wrote — `.titles.json`, `.display.json`,
// `.cwds.json`, `.kinds.json` — each keyed by that filename, each with its own
// load/modify/save code. Deleting one session meant four read-modify-writes; a
// CLI or ACP session had no metadata at all; and nothing recorded which engine
// produced a transcript, so a second engine could only have been bolted on by
// keeping a second store and syncing the two. That is precisely the shape the
// roadmap forbids:
//
//	Go 保存一份 transcript + dsh 再保存一份 transcript + 两边长期双向同步
//
// So: one index, one record per session, one owner. Store is an opaque
// reference the registry never interprets — a path today, whatever a future
// engine needs tomorrow. The conversation itself has exactly one source of
// truth, and it is the engine's.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"reasonix/internal/fileutil"
)

// IndexFile is the registry's on-disk index, in the session directory.
const IndexFile = ".sessions.json"

// Engine names the runtime that produced a session's transcript. It is recorded
// rather than inferred so a store reference is never guessed at.
const (
	EngineNative = "native"
)

// Record is everything OneCreat owns about one session.
type Record struct {
	// ID is the stable identity. It is independent of the store: renaming or
	// relocating a transcript does not create a different session.
	ID string `json:"id"`
	// Engine is which runtime produced the transcript ("native" today).
	Engine string `json:"engine"`
	// Store is the engine's reference to the raw transcript — a file path for
	// the native engine. The registry never parses it; only the engine does.
	Store string `json:"store"`
	// Workspace is the project root the session ran in, recorded once at
	// creation: which project a conversation belongs to should stay stable even
	// if the user later switches folders.
	Workspace string `json:"workspace,omitempty"`
	// Title is the user's chosen name ("" = fall back to a preview of the
	// transcript, which is the engine's business).
	Title string `json:"title,omitempty"`
	// Ephemeral marks a session whose transcript OneCreat cannot read: the engine
	// keeps the conversation in its own process, so there is no file here to
	// reopen. The record still exists — it carries the identity, the project and
	// the real engine name — but a frontend must show it as ephemeral and must not
	// offer history/resume/fork on it (复核 AR-R03)。
	//
	// 不把这类会话直接从索引里抹掉:那样会连「这条会话属于哪个引擎、哪个项目」都丢掉,
	// 而复核要的是 Registry 记录**真实**引擎与稳定 ID,只是别谎称本地有转录文本。
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Kind marks a vertical surface the session actually used ("hardware"),
	// for grouping in the history sidebar. Empty = an ordinary conversation.
	Kind      string    `json:"kind,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Display maps a message's content hash to the text to show instead — the
	// desktop rewrites a pasted attachment's raw payload into a short label.
	// It is per-session presentation state, so it belongs to the record.
	Display map[string]string `json:"display,omitempty"`
}

// Registry is the single source of truth for session identity and metadata.
//
// It serialises every read-modify-write: an atomic rename keeps the file from
// being corrupted, but it does not prevent a lost update — two tabs recording a
// display mapping at the same time would otherwise have the later write clobber
// the earlier one's entry.
type Registry struct {
	dir string

	mu    sync.Mutex
	index map[string]Record // by ID
	// byStore resolves an engine's reference back to the session. The desktop's
	// RPC surface still speaks in transcript paths, so this is the bridge from
	// the old handle to the new identity.
	byStore map[string]string
}

// Open returns the registry for a session directory. Nothing is read until the
// first operation, so constructing one is cheap and cannot fail.
func Open(dir string) *Registry { return &Registry{dir: dir} }

func (r *Registry) path() string { return filepath.Join(r.dir, IndexFile) }

// sync re-reads the index from disk, importing legacy sidecars if this is the
// first run after the upgrade. Caller holds mu.
//
// 它**每次操作前都读**,而不是读一次就缓存住。原因是一个进程里必然存在不止一个
// Registry:desktop 的 sessionService 一个,每个标签的 control.sessionStore 又各有
// 一个。save() 写的是**整份**内存快照,所以只要谁的快照是旧的,它那一次写回就会把
// 别人新加的记录抹掉 —— 这不是竞态,是必然(TestSecondOwnerDoesNotClobberTheFirst)。
//
// 代价是每次操作多一次几 KB 的 ReadFile。这些操作都是用户触发的低频动作(新建会话、
// 改标题、刷新历史面板),拿这点开销换掉一整类「会话记录莫名消失」,是划算的。
//
// 那个残留窗口(两个 owner 的「读—改—写」交错,各自的 mu 管不到对方)已经由索引目录
// 下的 lockfile 关掉,见 lock.go —— 每一次「读—改—写」整体在跨进程锁内进行,所以
// 这里读到的快照在写回之前不会被别人换掉。
func (r *Registry) sync() {
	r.index = map[string]Record{}
	r.byStore = map[string]string{}

	b, err := os.ReadFile(r.path())
	if err == nil {
		var recs []Record
		if json.Unmarshal(b, &recs) == nil {
			for _, rec := range recs {
				r.put(rec)
			}
			return
		}
		// A corrupt index is not a reason to lose the user's titles: fall through
		// and rebuild from the legacy sidecars if they are still there.
	}
	r.importLegacy()
}

func (r *Registry) put(rec Record) {
	r.index[rec.ID] = rec
	if rec.Store != "" {
		r.byStore[storeKey(rec.Store)] = rec.ID
	}
}

// storeKey normalises a store reference so a session is found whether the caller
// passes an absolute path or just the file name. The legacy sidecars were keyed
// by base name, and the desktop passes full paths.
func storeKey(store string) string { return filepath.Base(store) }

// save writes the index atomically. Caller holds mu.
func (r *Registry) save() error {
	recs := make([]Record, 0, len(r.index))
	for _, rec := range r.index {
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dir, ".sessions.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, r.path())
}

// Ensure returns the record for a store reference, creating it on first sight.
// workspace and engine are only applied at creation — which project and which
// runtime a conversation belongs to is decided once.
func (r *Registry) Ensure(store, workspace, engine string) (Record, error) {
	return r.ensure(store, workspace, engine, false)
}

// EnsureEphemeral is Ensure for an engine that keeps the transcript in its own
// process: the record carries identity, project and engine, but is marked
// Ephemeral so nothing offers to reopen a file that was never written
// (复核 AR-R03)。
func (r *Registry) EnsureEphemeral(store, workspace, engine string) (Record, error) {
	return r.ensure(store, workspace, engine, true)
}

func (r *Registry) ensure(store, workspace, engine string, ephemeral bool) (Record, error) {
	if strings.TrimSpace(store) == "" {
		return Record{}, fmt.Errorf("session: empty store reference")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out Record
	err := r.withFileLock(func() error {
		rec, err := r.ensureLocked(store, workspace, engine, ephemeral)
		out = rec
		return err
	})
	return out, err
}

// ensureLocked 是 Ensure 的实体。调用方持有 r.mu **和**索引文件锁 —— 「读整份 →
// 改一条 → 写回整份」必须整体互斥,否则并发的另一个 owner 会拿旧快照覆盖回来。
func (r *Registry) ensureLocked(store, workspace, engine string, ephemeral bool) (Record, error) {
	r.sync()
	if id, ok := r.byStore[storeKey(store)]; ok {
		rec := r.index[id]
		// A record imported from the legacy sidecars has no engine or store yet.
		changed := false
		if rec.Store == "" {
			rec.Store, changed = store, true
		}
		if rec.Engine == "" {
			rec.Engine, changed = orNative(engine), true
		}
		if rec.Ephemeral != ephemeral {
			rec.Ephemeral, changed = ephemeral, true
		}
		if !changed {
			return rec, nil
		}
		r.put(rec)
		return rec, r.save()
	}
	now := time.Now().UTC()
	rec := Record{
		ID: newID(store), Engine: orNative(engine), Store: store,
		Ephemeral: ephemeral,
		Workspace: workspace, CreatedAt: now, UpdatedAt: now,
	}
	r.put(rec)
	return rec, r.save()
}

func orNative(engine string) string {
	if strings.TrimSpace(engine) == "" {
		return EngineNative
	}
	return engine
}

// newID derives a stable id from the store reference at creation time. It is a
// *derivation*, not an alias: the id is stored, so it survives the store moving.
func newID(store string) string {
	return strings.TrimSuffix(filepath.Base(store), filepath.Ext(store))
}

// ByStore looks a session up by its engine store reference.
func (r *Registry) ByStore(store string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sync()
	id, ok := r.byStore[storeKey(store)]
	if !ok {
		return Record{}, false
	}
	rec, ok := r.index[id]
	return rec, ok
}

// List returns every known session, newest activity first.
func (r *Registry) List() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sync()
	out := make([]Record, 0, len(r.index))
	for _, rec := range r.index {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// update applies fn to the record for store, creating nothing: an unknown store
// is a no-op, because metadata for a session that was never started is meaningless.
func (r *Registry) update(store string, fn func(*Record)) error {
	if strings.TrimSpace(store) == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withFileLock(func() error { return r.updateLocked(store, fn) })
}

// updateLocked 是 update 的实体。调用方持有 r.mu 和索引文件锁。
func (r *Registry) updateLocked(store string, fn func(*Record)) error {
	r.sync()
	id, ok := r.byStore[storeKey(store)]
	if !ok {
		return nil
	}
	rec := r.index[id]
	fn(&rec)
	rec.UpdatedAt = time.Now().UTC()
	r.put(rec)
	return r.save()
}

// SetTitle sets (or, with an empty title, clears) a session's user-chosen name.
func (r *Registry) SetTitle(store, title string) error {
	return r.update(store, func(rec *Record) { rec.Title = strings.TrimSpace(title) })
}

// SetKind marks the vertical surface a session used. Write-once: which kind a
// conversation is should not flip because a later turn touched something else.
func (r *Registry) SetKind(store, kind string) error {
	if strings.TrimSpace(kind) == "" {
		return nil
	}
	return r.update(store, func(rec *Record) {
		if rec.Kind == "" {
			rec.Kind = kind
		}
	})
}

// SetWorkspace records the project a session belongs to, write-once for the same
// reason: a conversation's home folder should stay stable even if the user
// switches projects mid-session.
func (r *Registry) SetWorkspace(store, workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	return r.update(store, func(rec *Record) {
		if rec.Workspace == "" {
			rec.Workspace = workspace
		}
	})
}

// RecordDisplay remembers the text to show in place of a message's raw content.
func (r *Registry) RecordDisplay(store, key, display string) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(display) == "" {
		return nil
	}
	return r.update(store, func(rec *Record) {
		if rec.Display == nil {
			rec.Display = map[string]string{}
		}
		rec.Display[key] = display
	})
}

// Touch marks a session as recently active.
func (r *Registry) Touch(store string) error { return r.update(store, func(*Record) {}) }

// Forget drops a session's record. The engine's transcript is the caller's to
// delete — the registry never touches the store it does not own.
func (r *Registry) Forget(store string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withFileLock(func() error { return r.forgetLocked(store) })
}

// forgetLocked 是 Forget 的实体。调用方持有 r.mu 和索引文件锁。
func (r *Registry) forgetLocked(store string) error {
	r.sync()
	key := storeKey(store)
	id, ok := r.byStore[key]
	if !ok {
		return nil
	}
	delete(r.byStore, key)
	delete(r.index, id)
	return r.save()
}
