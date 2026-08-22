package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/session"
)

// 这些测试针对的是「历史面板看到的行为」,而不是存储实现 —— 存储的不变量由
// internal/session 自己的测试覆盖。Plan 11 之前这里还测四份侧车文件的 load/save
// 样板,那些函数已经不存在了。

func titleOf(t *testing.T, r *session.Registry, path string) string {
	t.Helper()
	rec, _ := r.ByStore(path)
	return rec.Title
}

func TestSetSessionTitle(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "my-session.jsonl")

	if err := setSessionTitle(r, sessionPath, "Custom Title"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := titleOf(t, r, sessionPath); got != "Custom Title" {
		t.Errorf("title = %q", got)
	}

	// Clearing with an empty string drops the custom name (the preview takes over).
	if err := setSessionTitle(r, sessionPath, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := titleOf(t, r, sessionPath); got != "" {
		t.Errorf("cleared title should be empty, got %q", got)
	}
}

func TestSetSessionTitleTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "s.jsonl")
	if err := setSessionTitle(r, sessionPath, "  trimmed  "); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := titleOf(t, r, sessionPath); got != "trimmed" {
		t.Errorf("title = %q, want trimmed", got)
	}
}

// TestDeleteSessionFile: one call removes both halves — the engine's transcript
// and OneCreat's record of it. Before Plan 11 this was four read-modify-writes
// across four sidecar files, and forgetting one left orphan metadata behind.
func TestDeleteSessionFile(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setSessionTitle(r, sessionPath, "My Title"); err != nil {
		t.Fatal(err)
	}
	if err := rememberSessionCwd(r, sessionPath, "/proj"); err != nil {
		t.Fatal(err)
	}
	if err := rememberSessionKind(r, sessionPath, "hardware"); err != nil {
		t.Fatal(err)
	}
	if err := recordSessionDisplay(r, sessionPath, "expanded prompt", "[Pasted text #1 · 5 lines]"); err != nil {
		t.Fatalf("record display: %v", err)
	}

	if err := deleteSessionFile(r, sessionPath); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Error("session file should be deleted")
	}
	if _, ok := r.ByStore(sessionPath); ok {
		t.Error("the session record should be gone too")
	}
	if got := resolveSessionDisplay(r, sessionPath, "expanded prompt"); got != "expanded prompt" {
		t.Errorf("display mapping should be gone after delete, got %q", got)
	}
}

func TestDeleteSessionFileNoMetadata(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "no-title.jsonl")
	if err := os.WriteFile(sessionPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deleteSessionFile(r, sessionPath); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Error("session file should be deleted")
	}
}

func TestDeleteSessionFileMissing(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	// Deleting a non-existent file should not error.
	if err := deleteSessionFile(r, filepath.Join(dir, "missing.jsonl")); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// TestSessionMetadataIsOneRecord: title, project and vertical used to live in
// three separate files. They are one record now, and the history panel reads all
// of it in a single lookup.
func TestSessionMetadataIsOneRecord(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "s.jsonl")
	if err := rememberSessionCwd(r, sessionPath, "/proj"); err != nil {
		t.Fatal(err)
	}
	if err := rememberSessionKind(r, sessionPath, "hardware"); err != nil {
		t.Fatal(err)
	}
	if err := setSessionTitle(r, sessionPath, "named"); err != nil {
		t.Fatal(err)
	}

	rec, ok := r.ByStore(sessionPath)
	if !ok {
		t.Fatal("no record")
	}
	if rec.Title != "named" || rec.Workspace != "/proj" || rec.Kind != "hardware" {
		t.Fatalf("record lost a field: %+v", rec)
	}
	if rec.Engine != session.EngineNative {
		t.Errorf("engine = %q, want %q — a store reference must never be guessed at", rec.Engine, session.EngineNative)
	}
	if rec.ID == "" {
		t.Error("a session must have an identity of its own, not just a filename")
	}
}

func TestErrActiveSession(t *testing.T) {
	if errActiveSession.Error() == "" {
		t.Error("errActiveSession should have a message")
	}
}

func TestSessionDisplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "s.jsonl")
	content := "prefix\n--- Begin [Pasted text #1 · 5 lines] ---\nfull text\n--- End [Pasted text #1 · 5 lines] ---"
	display := "[Pasted text #1 · 5 lines]"
	if err := recordSessionDisplay(r, sessionPath, content, display); err != nil {
		t.Fatalf("record display: %v", err)
	}
	if got := resolveSessionDisplay(r, sessionPath, content); got != display {
		t.Fatalf("display = %q, want %q", got, display)
	}
	if got := resolveSessionDisplay(r, sessionPath, "other"); got != "other" {
		t.Fatalf("unknown content should pass through, got %q", got)
	}
}

func TestRecordSessionDisplaySkipsNoop(t *testing.T) {
	dir := t.TempDir()
	r := session.Open(dir)
	sessionPath := filepath.Join(dir, "s.jsonl")
	if err := recordSessionDisplay(r, sessionPath, "same", "same"); err != nil {
		t.Fatalf("record display: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, session.IndexFile)); !os.IsNotExist(err) {
		t.Fatalf("a no-op display should not create a record, stat err = %v", err)
	}
}
