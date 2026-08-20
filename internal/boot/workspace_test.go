package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/plugin"
	"reasonix/internal/workspace"
)

// projectConfig is a minimal, self-contained project that Build can assemble
// without a network or an API key. rule becomes the project memory line and
// prompt the base system prompt, so both are observable in the built
// controller's cached system message.
func projectConfig(t *testing.T, dir, prompt, rule string) {
	t.Helper()
	writeFile(t, dir, "onecreat.toml", `
default_model = "test-model"

[codegraph]
enabled = false

[lsp]
enabled = false

[agent]
system_prompt = "`+prompt+`"

[[providers]]
name = "test-model"
kind = "openai"
base_url = "https://example.invalid"
model = "x"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`)
	writeFile(t, dir, "REASONIX.md", rule)
}

// TestBuildIsolatesTwoWorkspaces is Plan 01's end-to-end acceptance: two
// runtimes, two projects, alive at the same time in one process — with the
// process working directory pointing at neither of them. Everything
// workspace-scoped (project config, project memory, the checkpoint/confinement
// root) must come from each runtime's own root.
func TestBuildIsolatesTwoWorkspaces(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	projectConfig(t, a, "PROMPT A", "Project rule: this is workspace A.")
	projectConfig(t, b, "PROMPT B", "Project rule: this is workspace B.")

	// The process stands in a third directory that is neither project. Before
	// workspaces were explicit this alone would have broken both runtimes.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	wsA, err := workspace.New(a)
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := workspace.New(b)
	if err != nil {
		t.Fatal(err)
	}

	ctrlA, err := Build(context.Background(), Options{Workspace: wsA})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	defer ctrlA.Close()
	// B is built while A is still open: the two must coexist, not take turns.
	ctrlB, err := Build(context.Background(), Options{Workspace: wsB})
	if err != nil {
		t.Fatalf("Build(B): %v", err)
	}
	defer ctrlB.Close()

	sysA := systemMessage(ctrlA.History())
	sysB := systemMessage(ctrlB.History())

	// Project config came from each workspace's own onecreat.toml.
	if !strings.Contains(sysA, "PROMPT A") || strings.Contains(sysA, "PROMPT B") {
		t.Errorf("A's system prompt is not A's project config:\n%s", sysA)
	}
	if !strings.Contains(sysB, "PROMPT B") || strings.Contains(sysB, "PROMPT A") {
		t.Errorf("B's system prompt is not B's project config:\n%s", sysB)
	}
	// Project memory came from each workspace's own REASONIX.md.
	if !strings.Contains(sysA, "this is workspace A") || strings.Contains(sysA, "this is workspace B") {
		t.Errorf("A's memory is not A's REASONIX.md:\n%s", sysA)
	}
	if !strings.Contains(sysB, "this is workspace B") || strings.Contains(sysB, "this is workspace A") {
		t.Errorf("B's memory is not B's REASONIX.md:\n%s", sysB)
	}
}

// TestBuildWorkspaceSurvivesLaterChdir pins the switch-the-active-project case:
// once a runtime is built, moving the process working directory afterwards — the
// old way of "switching workspace" — must not change what that runtime reads or
// writes.
func TestBuildWorkspaceSurvivesLaterChdir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	projectConfig(t, a, "PROMPT A", "Project rule: this is workspace A.")
	projectConfig(t, b, "PROMPT B", "Project rule: this is workspace B.")
	t.Chdir(t.TempDir())

	wsA, err := workspace.New(a)
	if err != nil {
		t.Fatal(err)
	}
	ctrlA, err := Build(context.Background(), Options{Workspace: wsA})
	if err != nil {
		t.Fatalf("Build(A): %v", err)
	}
	defer ctrlA.Close()

	// The frontend "switches project" the old way.
	t.Chdir(b)

	sysA := systemMessage(ctrlA.History())
	if strings.Contains(sysA, "PROMPT B") || strings.Contains(sysA, "this is workspace B") {
		t.Fatalf("A's runtime followed the process cwd into B:\n%s", sysA)
	}
	if !strings.Contains(sysA, "PROMPT A") {
		t.Fatalf("A's runtime lost its own project config:\n%s", sysA)
	}
}

// TestBuildZeroWorkspaceUsesProcessCwd pins the compatibility promise for the
// CLI, which genuinely is process-cwd scoped and passes no workspace.
func TestBuildZeroWorkspaceUsesProcessCwd(t *testing.T) {
	dir := t.TempDir()
	projectConfig(t, dir, "PROMPT CWD", "Project rule: process cwd project.")
	t.Chdir(dir)

	ctrl, err := Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	sys := systemMessage(ctrl.History())
	if !strings.Contains(sys, "PROMPT CWD") || !strings.Contains(sys, "process cwd project") {
		t.Fatalf("zero workspace did not read the process cwd project:\n%s", sys)
	}
}

// TestBuildWorkspaceConfinesWritesToItsOwnRoot proves the sandbox write root
// follows the workspace rather than the process working directory: a config
// with no explicit sandbox.workspace_root must confine to its own project.
func TestBuildWorkspaceConfinesWritesToItsOwnRoot(t *testing.T) {
	a := t.TempDir()
	projectConfig(t, a, "PROMPT A", "Project rule: this is workspace A.")
	other := t.TempDir()
	t.Chdir(other)

	wsA, err := workspace.New(a)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadIn(wsA)
	if err != nil {
		t.Fatal(err)
	}
	roots := cfg.WriteRoots()
	if len(roots) == 0 {
		t.Fatal("WriteRoots is empty — confinement would be off")
	}
	if !samePathTest(roots[0], a) {
		t.Fatalf("WriteRoots[0] = %q, want workspace root %q", roots[0], a)
	}
	if samePathTest(roots[0], other) {
		t.Fatal("WriteRoots defaulted to the process working directory")
	}
}

func samePathTest(got, want string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return resolve(got) == resolve(want)
}

// TestHostProvidesCodeIntelSkipsWorkspaceDaemons pins the one option that
// changes which services a session starts. An editor host (ACP) already runs its
// own language servers and symbol index; starting a second CodeGraph daemon and
// LSP manager inside the agent costs memory and CPU for capabilities the host
// already has.
//
// It is asserted through the tool surface: the LSP tools are registered only
// when the manager is built.
func TestHostProvidesCodeIntelSkipsWorkspaceDaemons(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "onecreat.toml", `
default_model = "test-model"

[codegraph]
enabled = false

[lsp]
enabled = true

[agent]
system_prompt = "BASE"

[[providers]]
name = "test-model"
kind = "openai"
base_url = "https://example.invalid"
model = "x"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`)
	t.Chdir(dir)
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	withLSP, err := Build(context.Background(), Options{Workspace: ws})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer withLSP.Close()

	withoutLSP, err := Build(context.Background(), Options{Workspace: ws, HostProvidesCodeIntel: true})
	if err != nil {
		t.Fatalf("Build(HostProvidesCodeIntel): %v", err)
	}
	defer withoutLSP.Close()

	if !hasLSPTool(t, withLSP) {
		t.Fatal("lsp.enabled=true should register LSP tools")
	}
	if hasLSPTool(t, withoutLSP) {
		t.Fatal("HostProvidesCodeIntel should suppress the LSP manager — the host provides it")
	}
}

// hasLSPTool reports whether any LSP tool made it into the session's tool set.
func hasLSPTool(t *testing.T, ctrl *control.Controller) bool {
	t.Helper()
	for _, name := range ctrl.ToolNames() {
		if strings.HasPrefix(name, "lsp_") {
			return true
		}
	}
	return false
}

// TestExtraPluginsStartWithTheSession proves the other new option: MCP servers a
// host declares for one session (the ACP client's `session/new` servers) start
// eagerly alongside the configured ones, so their tools exist on the first turn.
func TestExtraPluginsStartWithTheSession(t *testing.T) {
	isolateConfigHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, dir, "onecreat.toml", `
default_model = "test-model"

[codegraph]
enabled = false

[lsp]
enabled = false

[agent]
system_prompt = "BASE"

[[providers]]
name = "test-model"
kind = "openai"
base_url = "https://example.invalid"
model = "x"
api_key_env = "REASONIX_TEST_KEY_UNSET"
`)
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctrl, err := Build(ctx, Options{
		Workspace: ws,
		ExtraPlugins: []plugin.Spec{{
			Name:    "hostmock",
			Command: os.Args[0],
			Args:    []string{"-test.run=TestHelperProcess", "--"},
			Env:     map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	names := ctrl.Host().ServerNames()
	found := false
	for _, n := range names {
		if n == "hostmock" {
			found = true
		}
	}
	if !found {
		t.Fatalf("caller-supplied MCP server missing from Host.ServerNames() = %v", names)
	}
	if got := ctrl.Host().Failures(); len(got) != 0 {
		t.Fatalf("Host.Failures() = %+v, want empty", got)
	}
}
