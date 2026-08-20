package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func storePath(dir, name string) string { return filepath.Join(dir, name) }

// TestEnsureIsIdempotentAndStable: the same store reference always resolves to
// the same session. Identity is recorded, not re-derived on each call.
func TestEnsureIsIdempotentAndStable(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	store := storePath(dir, "20260101-000000.000000000-flash.jsonl")

	a, err := r.Ensure(store, "/proj", EngineNative)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Ensure(store, "/other", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same store produced two identities: %q vs %q", a.ID, b.ID)
	}
	if b.Workspace != "/proj" || b.Engine != EngineNative {
		t.Errorf("workspace/engine must be decided once, got %+v", b)
	}
	// A fresh registry over the same directory sees the same record.
	if rec, ok := Open(dir).ByStore(store); !ok || rec.ID != a.ID {
		t.Fatalf("identity did not survive a reload: %+v", rec)
	}
}

// TestMetadataIsWriteOnceWhereItShouldBe: which project a conversation belongs
// to, and which vertical it is, should not flip mid-session; the title should.
func TestMetadataIsWriteOnceWhereItShouldBe(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	store := storePath(dir, "s.jsonl")
	if _, err := r.Ensure(store, "/proj-a", EngineNative); err != nil {
		t.Fatal(err)
	}

	if err := r.SetWorkspace(store, "/proj-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetKind(store, "hardware"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetKind(store, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetTitle(store, "first"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetTitle(store, "second"); err != nil {
		t.Fatal(err)
	}

	rec, _ := r.ByStore(store)
	if rec.Workspace != "/proj-a" {
		t.Errorf("workspace should stay where the session started, got %q", rec.Workspace)
	}
	if rec.Kind != "hardware" {
		t.Errorf("kind should be write-once, got %q", rec.Kind)
	}
	if rec.Title != "second" {
		t.Errorf("title should be freely renameable, got %q", rec.Title)
	}
	if err := r.SetTitle(store, "  "); err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.ByStore(store); rec.Title != "" {
		t.Errorf("an empty title should clear the name, got %q", rec.Title)
	}
}

// TestForgetLeavesTheTranscriptAlone: the registry owns the record, the engine
// owns the transcript. Deleting metadata must never touch the store.
func TestForgetLeavesTheTranscriptAlone(t *testing.T) {
	dir := t.TempDir()
	store := storePath(dir, "s.jsonl")
	if err := os.WriteFile(store, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Open(dir)
	if _, err := r.Ensure(store, "/proj", EngineNative); err != nil {
		t.Fatal(err)
	}
	if err := r.Forget(store); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ByStore(store); ok {
		t.Error("record should be gone")
	}
	if _, err := os.Stat(store); err != nil {
		t.Errorf("the registry deleted a transcript it does not own: %v", err)
	}
}

// TestLegacySidecarsAreImportedOnce is the migration guarantee. Losing someone's
// named conversations to a refactor would be a bad trade for tidiness, so the
// four old sidecars are read on first open — and left on disk afterwards.
func TestLegacySidecarsAreImportedOnce(t *testing.T) {
	dir := t.TempDir()
	base := "20260101-000000.000000000-flash.jsonl"
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(legacyTitles, map[string]string{base: "my chat"})
	write(legacyCwds, map[string]string{base: "/proj"})
	write(legacyKinds, map[string]string{base: "hardware"})
	write(legacyDisplays, map[string]map[string]string{base: {"hash": "[image]"}})

	r := Open(dir)
	rec, ok := r.ByStore(base)
	if !ok {
		t.Fatal("legacy session was not imported")
	}
	if rec.Title != "my chat" || rec.Workspace != "/proj" || rec.Kind != "hardware" {
		t.Fatalf("imported record lost fields: %+v", rec)
	}
	if rec.Display["hash"] != "[image]" {
		t.Errorf("display mapping lost: %+v", rec.Display)
	}
	// The sidecars stay, so a downgrade still finds them.
	for _, name := range []string{legacyTitles, legacyCwds, legacyKinds, legacyDisplays} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("legacy sidecar %s was deleted: %v", name, err)
		}
	}
	// And the index now exists, so the next open reads it rather than re-importing.
	if _, err := os.Stat(filepath.Join(dir, IndexFile)); err != nil {
		t.Fatalf("index was not written: %v", err)
	}
	if err := Open(dir).SetTitle(base, "renamed"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := Open(dir).ByStore(base); rec.Title != "renamed" {
		t.Errorf("the index should win after import, got %q", rec.Title)
	}
}

// TestCorruptIndexFallsBackToLegacy: a truncated index must not silently erase
// the user's titles when the sidecars are still there.
func TestCorruptIndexFallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	base := "s.jsonl"
	b, _ := json.Marshal(map[string]string{base: "kept"})
	if err := os.WriteFile(filepath.Join(dir, legacyTitles), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IndexFile), []byte("{ truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, ok := Open(dir).ByStore(base); !ok || rec.Title != "kept" {
		t.Fatalf("a corrupt index should fall back to the sidecars, got %+v ok=%v", rec, ok)
	}
}

// TestConcurrentUpdatesDoNotLoseEntries: an atomic rename keeps the file intact
// but does not prevent a lost update. Two tabs recording display mappings at the
// same time must both survive.
func TestConcurrentUpdatesDoNotLoseEntries(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	store := storePath(dir, "s.jsonl")
	if _, err := r.Ensure(store, "/proj", EngineNative); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.RecordDisplay(store, string(rune('a'+i)), "shown")
		}(i)
	}
	wg.Wait()
	rec, _ := r.ByStore(store)
	if len(rec.Display) != n {
		t.Fatalf("lost updates: %d display entries, want %d", len(rec.Display), n)
	}
}

// TestUnknownStoreIsANoOp: metadata for a session that was never started is
// meaningless, so it must not conjure a record.
func TestUnknownStoreIsANoOp(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	if err := r.SetTitle(storePath(dir, "never.jsonl"), "x"); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 0 {
		t.Fatalf("SetTitle created a record out of nothing: %+v", r.List())
	}
}
