package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/diff"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two turns edit a.txt and create b.txt; rewinding restores each file to its
// state at the start of the chosen turn (b.txt being deleted when it post-dates it).
func TestRestoreToStartOfTurn(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "sub", "b.txt")
	write(t, a, "v0")
	s := New("", root)

	s.Begin(0, "first", 0)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "v0"})
	write(t, a, "v1") // the edit turn 0 made

	s.Begin(1, "second", 2)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "v1"})
	s.Snapshot(diff.Change{Path: b, Kind: diff.Create})
	write(t, a, "v2")
	write(t, b, "new")

	// Rewind to the start of turn 1: a back to v1, b gone.
	if _, _, err := s.RestoreCode(1); err != nil {
		t.Fatal(err)
	}
	if got := read(t, a); got != "v1" {
		t.Fatalf("a = %q, want v1", got)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("b should have been deleted, stat err=%v", err)
	}
}

func TestRestoreToTurnZero(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	write(t, a, "v0")
	s := New("", root)
	s.Begin(0, "first", 0)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "v0"})
	write(t, a, "v1")
	s.Begin(1, "second", 2)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "v1"})
	write(t, a, "v2")

	if _, _, err := s.RestoreCode(0); err != nil {
		t.Fatal(err)
	}
	if got := read(t, a); got != "v0" {
		t.Fatalf("a = %q, want v0 (earliest snapshot)", got)
	}
}

func TestSnapshotDedupsFirstTouchWins(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	write(t, a, "orig")
	s := New("", root)
	s.Begin(0, "p", 0)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "orig"})
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "edited-once"}) // ignored
	write(t, a, "edited-twice")
	if _, _, err := s.RestoreCode(0); err != nil {
		t.Fatal(err)
	}
	if got := read(t, a); got != "orig" {
		t.Fatalf("a = %q, want orig (first snapshot wins)", got)
	}
}

func TestRestoreRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.txt")
	write(t, outside, "keep")
	s := New("", root)
	s.Begin(0, "p", 0)
	s.Snapshot(diff.Change{Path: outside, Kind: diff.Modify, OldText: "hacked"})
	if _, _, err := s.RestoreCode(0); err == nil {
		t.Fatal("RestoreCode should reject a path outside the workspace")
	}
	if got := read(t, outside); got != "keep" {
		t.Fatalf("outside file was modified: %q", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "sess.ckpt")
	a := filepath.Join(root, "a.txt")

	s := New(dir, root)
	s.Begin(0, "hello", 1)
	s.Snapshot(diff.Change{Path: a, Kind: diff.Modify, OldText: "v0"})
	s.Begin(1, "world", 5)

	// A fresh store over the same dir must see both turns and their boundaries.
	s2 := New(dir, root)
	metas := s2.List()
	if len(metas) != 2 {
		t.Fatalf("loaded %d checkpoints, want 2", len(metas))
	}
	if metas[0].Prompt != "hello" || metas[1].Prompt != "world" {
		t.Fatalf("prompts = %q, %q", metas[0].Prompt, metas[1].Prompt)
	}
	// Boundaries must survive the round-trip so a resumed session can rewind/fork.
	b := s2.Bounds()
	if b[0] != 1 || b[1] != 5 {
		t.Fatalf("bounds = %v, want {0:1, 1:5}", b)
	}
	if s2.NextTurn() != 2 {
		t.Fatalf("NextTurn = %d, want 2", s2.NextTurn())
	}
}

// Prune 回归(F1):对话 rewind 到 turn T 会 Prune(T),丢弃废弃 turn(>=T)的 checkpoint。
// 演示最严重的一种数据丢失:废弃时间线里 turn 1 是"新建文件"(快照 Content==nil),不 Prune
// 的话 RestoreCode 会取到这份废弃快照,把当前时间线里合法存在的同名文件静默删掉。
func TestPrunePreventsAbandonedTimelineDataLoss(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "s.ckpt")
	f := filepath.Join(root, "f.txt")

	s := New(dir, root)
	// 废弃时间线:turn 0 占位,turn 1 新建 f(快照 Content==nil = "turn 1 开始时 f 不存在")。
	s.Begin(0, "t0", 0)
	s.Begin(1, "t1-abandoned", 1)
	s.Snapshot(diff.Change{Path: f, Kind: diff.Create})

	// 对话 rewind 到 turn 1 → 废弃 turn>=1。
	s.Prune(1)

	// 新时间线:turn 1 改动一个【当前合法存在】的 f。
	write(t, f, "current")
	s.Begin(1, "t1-new", 1)
	s.Snapshot(diff.Change{Path: f, Kind: diff.Modify, OldText: "current"})
	write(t, f, "current-edited")

	// 不再有重复 turn 号。
	if metas := s.List(); len(metas) != 2 || metas[1].Turn != 1 || metas[1].Prompt != "t1-new" {
		t.Fatalf("Prune 后 List 应为 [turn0, turn1-new],got %+v", metas)
	}
	// RestoreCode(1) 应还原成新时间线的 "current",绝不按废弃快照删掉 f。
	_, deleted, err := s.RestoreCode(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("Prune 后不应删除任何文件,却删了 %v(废弃时间线的 Create 快照泄漏 = 数据丢失)", deleted)
	}
	if got := read(t, f); got != "current" {
		t.Fatalf("f = %q, want current(新时间线 turn 1 的起始内容)", got)
	}
}

// InvalidateBounds 回归(F2):压缩失效边界后写盘的 threshold 必须跨重载(resume)恢复,且单调。
func TestInvalidateBoundsPersistsAcrossReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s.ckpt")
	s := New(dir, t.TempDir())
	s.Begin(0, "t0", 5)
	s.InvalidateBounds(3)
	if got := s.MinValidBoundTurn(); got != 3 {
		t.Fatalf("MinValidBoundTurn = %d, want 3", got)
	}
	// 重新加载(模拟 resume):marker 从磁盘恢复。
	s2 := New(dir, t.TempDir())
	if got := s2.MinValidBoundTurn(); got != 3 {
		t.Fatalf("reload 后 MinValidBoundTurn = %d, want 3", got)
	}
	// 单调:更小的 threshold 不覆盖已记录的更高值。
	s2.InvalidateBounds(2)
	if got := s2.MinValidBoundTurn(); got != 3 {
		t.Fatalf("InvalidateBounds 必须单调,got %d want 3", got)
	}
}
