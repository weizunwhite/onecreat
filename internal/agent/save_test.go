package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/provider"
)

// touch sets a file's mtime to t. Used by the listing-order test so it
// doesn't have to sleep between Saves.
func touch(path string, t time.Time) error {
	return os.Chtimes(path, t, t)
}

// TestSaveLoadRoundTrip is the contract `reasonix chat --resume` depends on: a
// session written to disk reloads byte-for-byte, including tool calls and
// reasoning content (which the model wants to keep across resumes for cache
// hits on thinking-mode providers).
func TestSaveLoadRoundTrip(t *testing.T) {
	s := NewSession("you are reasonix")
	s.Add(provider.Message{Role: provider.RoleUser, Content: "find the bug"})
	s.Add(provider.Message{
		Role:             provider.RoleAssistant,
		Content:          "Let me check.",
		ReasoningContent: "I should look at main.go first.",
		ToolCalls: []provider.ToolCall{{
			ID: "call_1", Name: "read_file", Arguments: `{"path":"main.go"}`,
		}},
	})
	s.Add(provider.Message{
		Role: provider.RoleTool, Name: "read_file", ToolCallID: "call_1",
		Content: "package main\nfunc main() {}\n",
	})
	s.Add(provider.Message{Role: provider.RoleAssistant, Content: "It's fine."})

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got, want := len(loaded.Messages), len(s.Messages); got != want {
		t.Fatalf("message count after round-trip = %d, want %d", got, want)
	}
	for i, m := range s.Messages {
		if loaded.Messages[i].Role != m.Role {
			t.Errorf("message %d role mismatch", i)
		}
		if loaded.Messages[i].Content != m.Content {
			t.Errorf("message %d content mismatch", i)
		}
		if loaded.Messages[i].ReasoningContent != m.ReasoningContent {
			t.Errorf("message %d reasoning mismatch", i)
		}
		if len(loaded.Messages[i].ToolCalls) != len(m.ToolCalls) {
			t.Errorf("message %d tool_calls count mismatch", i)
		}
	}
}

func TestSaveLoadLargeMessage(t *testing.T) {
	s := NewSession("sys")
	s.Add(provider.Message{Role: provider.RoleUser, Content: "run it"})
	// A bash result can exceed any line-buffer cap; Save must round-trip it.
	big := strings.Repeat("x", 5*1024*1024)
	s.Add(provider.Message{Role: provider.RoleTool, Name: "bash", ToolCallID: "c1", Content: big})

	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession of a session with a >4MiB message: %v", err)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(loaded.Messages))
	}
	if loaded.Messages[2].Content != big {
		t.Errorf("large content not round-tripped (got %d bytes, want %d)", len(loaded.Messages[2].Content), len(big))
	}
}

// TestListSessionsOrdersByMTime makes sure the picker shows the most
// recently used conversation first — that's what users reach for when they
// hit `reasonix chat --continue`.
func TestListSessionsOrdersByMTime(t *testing.T) {
	dir := t.TempDir()
	// Write two sessions with explicit mtimes so the order is deterministic.
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		s := NewSession("")
		s.Add(provider.Message{Role: provider.RoleUser, Content: "preview for " + name})
		if err := s.Save(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	oldT := time.Now().Add(-1 * time.Hour)
	newT := time.Now()
	if err := touch(filepath.Join(dir, "a.jsonl"), oldT); err != nil {
		t.Fatal(err)
	}
	if err := touch(filepath.Join(dir, "b.jsonl"), newT); err != nil {
		t.Fatal(err)
	}

	got, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if !strings.HasSuffix(got[0].Path, "b.jsonl") {
		t.Errorf("first entry = %s, want the newer 'b.jsonl'", got[0].Path)
	}
	if got[0].Turns != 1 || got[0].Preview != "preview for b.jsonl" {
		t.Errorf("preview/turns wrong on newest: turns=%d preview=%q", got[0].Turns, got[0].Preview)
	}
}

// TestPreviewStripsPlanModeMarker 钉死「(空会话)」回归:计划模式把一整段
// "[Plan mode — …]" 标记注入用户消息开头,预览只取前 80 字会全被标记占满,
// 侧栏显示成空会话。预览必须剥掉标记、露出用户真正的问题。
func TestPreviewStripsPlanModeMarker(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("")
	marker := "[Plan mode — read-only. Explore the codebase first (read_file, ls, grep, glob, web_fetch, task are available; writers are refused by the harness), then present a LAYERED plan as your reply and stop.]"
	s.Add(provider.Message{Role: provider.RoleUser, Content: marker + "\n\n讲讲这个代码库的架构"})
	path := filepath.Join(dir, "plan.jsonl")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	preview, turns := previewSession(path)
	if turns != 1 {
		t.Fatalf("turns = %d, want 1", turns)
	}
	if preview != "讲讲这个代码库的架构" {
		t.Fatalf("preview = %q, want 用户真实问题(标记应被剥掉)", preview)
	}
}

func TestListSessionsOrdersByLastActivityMeta(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	bPath := filepath.Join(dir, "b.jsonl")
	for _, path := range []string{aPath, bPath} {
		s := NewSession("")
		s.Add(provider.Message{Role: provider.RoleUser, Content: "preview for " + filepath.Base(path)})
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	olderActivity := now.Add(-2 * time.Hour)
	newerActivity := now.Add(-1 * time.Hour)
	writeBranchMeta(t, aPath, now.Add(-24*time.Hour), newerActivity)
	writeBranchMeta(t, bPath, now.Add(-24*time.Hour), olderActivity)
	if err := touch(aPath, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := touch(bPath, now); err != nil {
		t.Fatal(err)
	}

	got, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Path != aPath {
		t.Fatalf("first entry = %s, want activity-newer a.jsonl despite older file mtime", got[0].Path)
	}
	if !got[0].LastActivityAt.Equal(newerActivity) || !got[0].ModTime.Equal(newerActivity) {
		t.Fatalf("activity fields = %s / %s, want %s", got[0].LastActivityAt, got[0].ModTime, newerActivity)
	}
}

func writeBranchMeta(t *testing.T, path string, createdAt, updatedAt time.Time) {
	t.Helper()
	meta := BranchMeta{
		ID:        BranchID(path),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BranchMetaPath(path), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestListSessionsMissingDir returns nil + no error so callers can fall
// through to a fresh session without special-casing.
func TestListSessionsMissingDir(t *testing.T) {
	got, err := ListSessions(filepath.Join(t.TempDir(), "never-created"))
	if err != nil || got != nil {
		t.Errorf("missing dir = %v / %v, want nil/nil", got, err)
	}
}
