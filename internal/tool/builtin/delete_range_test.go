package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteRangeBasic(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.txt")
	body := "line1\nline2\nline3\nline4\nline5\n"
	os.WriteFile(f, []byte(body), 0o644)

	out := runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "line4",
	})
	if !strings.Contains(out, "---") || !strings.Contains(out, "+++") {
		t.Errorf("expected unified diff output, got: %s", out)
	}
	got, _ := os.ReadFile(f)
	want := "line1\nline5\n"
	if string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestDeleteRangeInclusive(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(f, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)
	runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "line4", "inclusive": true,
	})
	got, _ := os.ReadFile(f)
	if string(got) != "line1\nline5\n" {
		t.Errorf("inclusive=true: got %q, want %q", got, "line1\\nline5\\n")
	}

	f2 := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(f2, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)
	runTool(t, deleteRange{}, map[string]any{
		"path": f2, "start_anchor": "line2", "end_anchor": "line4", "inclusive": false,
	})
	got2, _ := os.ReadFile(f2)
	if string(got2) != "line1\nline2\nline4\nline5\n" {
		t.Errorf("inclusive=false: got %q, want %q", got2, "line1\\nline2\\nline4\\nline5\\n")
	}
}

func TestDeleteRangeDuplicateAnchor(t *testing.T) {
	f := filepath.Join(t.TempDir(), "dup.txt")
	body := "line1\nline2\nline3\nline2\nline5\n"
	os.WriteFile(f, []byte(body), 0o644)

	args := argsJSON(t, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "line5",
	})
	_, err := (deleteRange{}).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected duplicate anchor error")
	}
	if !strings.Contains(err.Error(), "not unique") {
		t.Errorf("error should mention 'not unique': %v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != body {
		t.Errorf("file modified despite error: %q", got)
	}
}

func TestDeleteRangeMissingAnchor(t *testing.T) {
	f := filepath.Join(t.TempDir(), "missing.txt")
	body := "line1\nline2\nline3\n"
	os.WriteFile(f, []byte(body), 0o644)

	args := argsJSON(t, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "no_such_line",
	})
	_, err := (deleteRange{}).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected missing anchor error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestDeleteRangeReversed(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rev.txt")
	body := "line1\nline2\nline3\nline4\nline5\n"
	os.WriteFile(f, []byte(body), 0o644)

	args := argsJSON(t, map[string]any{
		"path": f, "start_anchor": "line4", "end_anchor": "line2",
	})
	_, err := (deleteRange{}).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected reversed anchor error")
	}
	if !strings.Contains(err.Error(), "after") {
		t.Errorf("error should mention ordering: %v", err)
	}
}

func TestDeleteRangeCRLF(t *testing.T) {
	f := filepath.Join(t.TempDir(), "crlf.txt")
	body := "line1\r\nline2\r\nline3\r\nline4\r\nline5\r\n"
	os.WriteFile(f, []byte(body), 0o644)

	runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "line4",
	})
	got, _ := os.ReadFile(f)
	want := "line1\r\nline5\r\n"
	if string(got) != want {
		t.Errorf("CRLF file: got %q, want %q", got, want)
	}
}

// C2:行内的裸 \r(字符串字面量、旧 Mac 片段)不能被删除,被删区间外每行字节不变。
func TestDeleteRangePreservesInlineCR(t *testing.T) {
	f := filepath.Join(t.TempDir(), "inline_cr.txt")
	// 第二行含一个行内裸 \r(模拟字符串字面量 "a\rb")。
	body := "keep1\nfoo := \"a\rb\"\nDELETE_ME\nkeep2\n"
	os.WriteFile(f, []byte(body), 0o644)

	runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "DELETE_ME", "end_anchor": "DELETE_ME",
	})
	got, _ := os.ReadFile(f)
	want := "keep1\nfoo := \"a\rb\"\nkeep2\n"
	if string(got) != want {
		t.Errorf("inline \\r got %q, want %q (in-line CR must be preserved)", got, want)
	}
}

// C2:混合行尾文件,被删区间外的行不能被统一行尾。
func TestDeleteRangePreservesMixedLineEndings(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mixed.txt")
	body := "line1\r\nline2\nDELETE\nline4\r\n"
	os.WriteFile(f, []byte(body), 0o644)

	runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "DELETE", "end_anchor": "DELETE",
	})
	got, _ := os.ReadFile(f)
	want := "line1\r\nline2\nline4\r\n" // 各行原始行尾保持不变
	if string(got) != want {
		t.Errorf("mixed endings got %q, want %q (endings outside range must not be normalized)", got, want)
	}
}

func TestDeleteRangeWholeNewlineTerminatedFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "whole.txt")
	os.WriteFile(f, []byte("line1\n"), 0o644)

	runTool(t, deleteRange{}, map[string]any{
		"path": f, "start_anchor": "line1", "end_anchor": "line1",
	})
	got, _ := os.ReadFile(f)
	if string(got) != "" {
		t.Errorf("whole-file delete left content %q, want empty", got)
	}
}

func TestDeleteRangePreview(t *testing.T) {
	f := filepath.Join(t.TempDir(), "preview.txt")
	body := "line1\nline2\nline3\nline4\nline5\n"
	os.WriteFile(f, []byte(body), 0o644)

	change, err := deleteRange{}.Preview(argsJSON(t, map[string]any{
		"path": f, "start_anchor": "line2", "end_anchor": "line4",
	}))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}

	got, _ := os.ReadFile(f)
	if string(got) != body {
		t.Errorf("Preview mutated the file: %q", got)
	}

	if change.Kind != "modify" {
		t.Errorf("kind = %q, want modify", change.Kind)
	}
	if change.OldText != body {
		t.Errorf("OldText = %q, want %q", change.OldText, body)
	}
}
