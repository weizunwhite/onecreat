package control

// checkpointService owns snapshot-based rewind: the per-session checkpoint store,
// the monotonic turn counter, and the turn→message-index boundaries that make a
// conversation rewind (or fork, or summarize-at) able to say *where* a turn began.
//
// Split out of control.Controller in Plan 07. The state moved as a unit because
// it is only ever consistent as a unit — a boundary is meaningless without the
// turn number it was recorded under, and both are invalidated together whenever
// something rewrites the message log.
//
// Two invariants live here and nowhere else:
//
//   - The turn counter is **monotonic**. Compaction invalidates boundaries but
//     must not renumber, or a new checkpoint would collide with one already on
//     disk under the same number (B1).
//   - A boundary that cannot be trusted is **dropped, not guessed**. After a
//     compaction rewrites the log in place, the old MsgIndex values point at the
//     wrong messages; callers then honestly report "unavailable for this turn"
//     instead of silently truncating to the wrong place (B3/B4). The file
//     snapshots stay valid, so code rewind still works.

import (
	"strings"
	"sync"

	"reasonix/internal/checkpoint"
	"reasonix/internal/diff"
)

type checkpointService struct {
	// wsRoot is the session's workspace root: it confines checkpoint restore
	// writes to the project.
	wsRoot string

	mu    sync.Mutex
	store *checkpoint.Store
	turn  int
	bound map[int]int
}

func newCheckpointService(wsRoot string) *checkpointService {
	return &checkpointService{wsRoot: wsRoot, bound: map[int]int{}}
}

// ckptDir derives a session's checkpoint directory from its file path
// (…/<id>.jsonl → …/<id>.ckpt). Empty path → empty (in-memory checkpoints).
func ckptDir(sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	return strings.TrimSuffix(sessionPath, ".jsonl") + ".ckpt"
}

// Rebind points the store at the (possibly new) session, loading any checkpoints
// already on disk, and resets the turn boundaries. Called on construction and
// whenever the session path changes (NewSession/Resume/SetSessionPath).
func (s *checkpointService) Rebind(sessionPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = checkpoint.New(ckptDir(sessionPath), s.wsRoot)
	s.turn = s.store.NextTurn() // continue numbering past any checkpoints on disk
	s.bound = s.store.Bounds()  // rebuilt from persisted checkpoints so a resumed
	if s.bound == nil {         // session can still rewind conversation / fork
		s.bound = map[int]int{}
	}
	// 丢弃被压缩失效的陈旧边界:压缩原地重写了日志,压缩前 turn 的 MsgIndex 已失真。这些
	// checkpoint 的文件快照(代码 rewind)仍有效,只是对话边界不可信——所以只剔 MsgIndex,
	// 不删 checkpoint。剔掉后这些 turn 的对话 rewind 会诚实报"不可用",而非静默切错位置。
	if min := s.store.MinValidBoundTurn(); min > 0 {
		for t := range s.bound {
			if t < min {
				delete(s.bound, t)
			}
		}
	}
}

// Available reports whether a store is bound at all.
func (s *checkpointService) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store != nil
}

// Invalidate 清空 turn→消息下标的映射。任何压缩 / 摘要重写了日志后调用它,使「仅对话」
// rewind 与 fork 退化为「不可用」而不是用陈旧下标静默切错;新的 turn 会重建边界。
// turn 计数保持单调,避免与磁盘上的 checkpoint 编号冲突(B1)。
func (s *checkpointService) Invalidate() {
	s.mu.Lock()
	s.bound = map[int]int{}
	turn := s.turn // 只有 turn>=当前计数(压缩后新起的)边界才可信;之前的 MsgIndex 都陈旧
	store := s.store
	s.mu.Unlock()
	// 把"边界失效"持久化,让它跨 resume 生效:否则 Rebind 会从磁盘 checkpoint 的陈旧
	// MsgIndex 无条件重填边界,resume 后对压缩前 turn 做对话 rewind 会静默截到错误位置。
	if store != nil {
		store.InvalidateBounds(turn)
	}
}

// Begin opens a checkpoint for the turn about to run, recording the current
// message count as the conversation-rewind boundary. Called at the top of runTurn,
// before the user message is appended.
func (s *checkpointService) Begin(input string, msgIndex int) {
	s.mu.Lock()
	store := s.store
	if store == nil {
		s.mu.Unlock()
		return
	}
	turn := s.turn
	s.turn++
	s.bound[turn] = msgIndex
	s.mu.Unlock()
	store.Begin(turn, input, msgIndex)
}

// Snapshot records a writer tool's pre-edit content into the open checkpoint.
func (s *checkpointService) Snapshot(ch diff.Change) {
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store != nil {
		store.Snapshot(ch)
	}
}

// List returns the session's rewind points (one per user turn), oldest first.
//
// 锁内捕获 store 后解锁再用(store.List 自带内部锁,不必持这把):Rebind 会替换
// store,裸读指针与之构成数据竞争(桌面端「打开 rewind 抽屉」与「恢复历史会话」是
// 不同 goroutine)。
func (s *checkpointService) List() []checkpoint.Meta {
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.List()
}

// Bound returns the message-log boundary recorded at the start of turn, and
// whether it is still trustworthy.
func (s *checkpointService) Bound(turn int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bound[turn]
	return b, ok
}

// RestoreCode reverts every file that turn (or a later one) changed to its
// pre-turn content, returning the files written and removed.
func (s *checkpointService) RestoreCode(turn int) ([]string, []string, error) {
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store == nil {
		return nil, nil, nil
	}
	return store.RestoreCode(turn)
}

// TruncateFrom drops everything at or after turn: it renumbers future turns from
// there, forgets their boundaries, and prunes their checkpoints.
//
// 删掉被回退掉的 turn(>=turn)的废弃 checkpoint(内存 + 磁盘)是必须的。否则复用 turn 号后
// RestoreCode / Bounds 会取到废弃时间线的同号快照——按旧内容覆盖,甚至(废弃快照记录
// 的是"文件新建",Content==nil)静默删除当前时间线合法存在的文件。
func (s *checkpointService) TruncateFrom(turn int) {
	s.mu.Lock()
	s.turn = turn // renumber future turns from here; later turns are gone
	for k := range s.bound {
		if k >= turn {
			delete(s.bound, k)
		}
	}
	store := s.store
	s.mu.Unlock()
	if store != nil {
		store.Prune(turn)
	}
}
