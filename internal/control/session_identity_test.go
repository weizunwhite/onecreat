package control

import (
	"path/filepath"
	"testing"

	"reasonix/internal/session"
)

// TestEverySessionGetsAnIdentity is Plan 11's acceptance seen from the shared
// composition layer: a session built by *any* frontend — desktop, CLI, ACP —
// gets a record with an identity, a workspace and a named engine.
//
// Before, only the desktop knew anything about a session: identity was the
// transcript's file name and the metadata lived in desktop-only sidecars, so a
// CLI or editor session had none at all.
func TestEverySessionGetsAnIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260101-000000.000000000-flash.jsonl")
	c := New(Options{SessionDir: dir, SessionPath: path, WorkspaceRoot: "/proj"})

	rec, ok := c.SessionRecord()
	if !ok {
		t.Fatal("a persisted session must have a record")
	}
	if rec.ID == "" {
		t.Error("identity must be a recorded field, not a file name")
	}
	if rec.Workspace != "/proj" {
		t.Errorf("workspace = %q, want /proj", rec.Workspace)
	}
	if rec.Engine != session.EngineNative {
		t.Errorf("engine = %q, want %q — which runtime owns a transcript is recorded, never guessed", rec.Engine, session.EngineNative)
	}
	if rec.Store != path {
		t.Errorf("store = %q, want the transcript path %q", rec.Store, path)
	}
}

// TestRotatingTheSessionRegistersTheNewOne: /new and fork mint a fresh
// transcript; each is its own session, not a rename of the old one.
func TestRotatingTheSessionRegistersTheNewOne(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	c := New(Options{SessionDir: dir, SessionPath: first, WorkspaceRoot: "/proj"})
	before, _ := c.SessionRecord()

	c.SetSessionPath(second)
	after, ok := c.SessionRecord()
	if !ok {
		t.Fatal("the new session was not registered")
	}
	if after.ID == before.ID {
		t.Error("a rotated session must be a new identity, not the old record repointed")
	}
	if len(session.Open(dir).List()) != 2 {
		t.Error("both sessions should be known")
	}
}

// TestPersistenceOffMeansNoRecord: a headless run with no session directory
// writes nothing and claims no identity.
func TestPersistenceOffMeansNoRecord(t *testing.T) {
	c := New(Options{})
	if _, ok := c.SessionRecord(); ok {
		t.Error("a session with no persistence should have no record")
	}
}
