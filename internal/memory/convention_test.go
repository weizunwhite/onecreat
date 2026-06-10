package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMdDiscovered(t *testing.T) {
	proj := t.TempDir()
	mustMkdir(t, filepath.Join(proj, ".git"))
	mustWrite(t, filepath.Join(proj, "CLAUDE.md"), "Rule from CLAUDE.md")

	set := Load(Options{CWD: proj})
	if !strings.Contains(set.Block(), "Rule from CLAUDE.md") {
		t.Fatalf("CLAUDE.md should be discovered and folded in:\n%s", set.Block())
	}
}

func TestSymlinkedAgentAndClaudeDocsComposeOnce(t *testing.T) {
	proj := t.TempDir()
	mustMkdir(t, filepath.Join(proj, ".git"))
	mustWrite(t, filepath.Join(proj, "CLAUDE.md"), "Shared symlink guidance")
	if err := os.Symlink("CLAUDE.md", filepath.Join(proj, "AGENTS.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	prompt := Compose("BASE", Load(Options{CWD: proj}))
	if got := strings.Count(prompt, "Shared symlink guidance"); got != 1 {
		t.Fatalf("symlinked memory should be composed once, got %d occurrences:\n%s", got, prompt)
	}
}

func TestDocPathDefaultsToOnecreat(t *testing.T) {
	proj := t.TempDir()
	set := Load(Options{CWD: proj})
	// 产品对外只有 OneCreat:新项目的记忆文件默认建成 ONECREAT.md。
	if got := set.DocPath(ScopeProject); filepath.Base(got) != "ONECREAT.md" {
		t.Errorf("fresh project should default to ONECREAT.md, got %s", got)
	}
	if got := set.DocPath(ScopeLocal); filepath.Base(got) != "ONECREAT.local.md" {
		t.Errorf("fresh local should default to ONECREAT.local.md, got %s", got)
	}
}

// ONECREAT.md 会被发现且作为既有写入目标;旧名 REASONIX.md 仍兼容(另有用例)。
func TestOnecreatMdDiscoveredAndPreferred(t *testing.T) {
	proj := t.TempDir()
	mustMkdir(t, filepath.Join(proj, ".git"))
	mustWrite(t, filepath.Join(proj, "ONECREAT.md"), "Rule from ONECREAT.md")

	set := Load(Options{CWD: proj})
	if !strings.Contains(set.Block(), "Rule from ONECREAT.md") {
		t.Fatalf("ONECREAT.md should be discovered:\n%s", set.Block())
	}
	if got := set.DocPath(ScopeProject); filepath.Base(got) != "ONECREAT.md" {
		t.Errorf("existing ONECREAT.md should be the write target, got %s", got)
	}
}

func TestDocPathPrefersExisting(t *testing.T) {
	proj := t.TempDir()
	// An existing REASONIX.md should keep receiving notes (no split to AGENTS.md).
	if err := os.WriteFile(filepath.Join(proj, "REASONIX.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Load(Options{CWD: proj})
	if got := set.DocPath(ScopeProject); filepath.Base(got) != "REASONIX.md" {
		t.Errorf("should append to the existing REASONIX.md, got %s", got)
	}

	// With only a CLAUDE.md present, that's the target.
	proj2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj2, "CLAUDE.md"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	set2 := Load(Options{CWD: proj2})
	if got := set2.DocPath(ScopeProject); filepath.Base(got) != "CLAUDE.md" {
		t.Errorf("should append to the existing CLAUDE.md, got %s", got)
	}
}
