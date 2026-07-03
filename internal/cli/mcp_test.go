package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/plugin"
)

// C1 回归:在项目目录跑 `reasonix mcp add`/`remove` 必须落【用户级】config.toml,绝不在
// 项目根创建 onecreat.toml/reasonix.toml(那会把全量合并快照落进项目文件、永久遮蔽用户级配置)。
func TestMCPAddRemoveCLIWritesUserLevelNotProject(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", cfgDir)
	t.Chdir(t.TempDir())

	if code := mcpAddCLI([]string{"mock", "mock-mcp"}); code != 0 {
		t.Fatalf("mcpAddCLI exit = %d, want 0", code)
	}
	userPath := filepath.Join(cfgDir, "config.toml")
	if !pluginPresent(t, userPath, "mock") {
		t.Fatal("用户级 config.toml 未写入 mock 条目")
	}
	assertNoProjectToml(t)

	if code := mcpRemoveCLI([]string{"mock"}); code != 0 {
		t.Fatalf("mcpRemoveCLI exit = %d, want 0", code)
	}
	if pluginPresent(t, userPath, "mock") {
		t.Fatal("remove 后用户级 config.toml 仍有 mock 条目")
	}
	assertNoProjectToml(t)
}

func pluginPresent(t *testing.T, path, name string) bool {
	t.Helper()
	cfg := config.LoadForEdit(path)
	for _, p := range cfg.Plugins {
		if p.Name == name {
			return true
		}
	}
	return false
}

func assertNoProjectToml(t *testing.T) {
	t.Helper()
	for _, name := range []string{"onecreat.toml", "reasonix.toml"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("mcp 操作不应创建项目级 %s(会遮蔽用户级配置)", name)
		}
	}
}

func TestParseMCPAddStdio(t *testing.T) {
	e, err := parseMCPAdd([]string{"fs", "npx", "-y", "@modelcontextprotocol/server-filesystem", "."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Name != "fs" || e.Command != "npx" {
		t.Fatalf("name/command = %q/%q", e.Name, e.Command)
	}
	// The command keeps its own -flags: "-y" is an arg, not parsed as our flag.
	if want := []string{"-y", "@modelcontextprotocol/server-filesystem", "."}; !reflect.DeepEqual(e.Args, want) {
		t.Fatalf("args = %v, want %v", e.Args, want)
	}
	if e.URL != "" {
		t.Errorf("stdio entry should have no URL, got %q", e.URL)
	}
}

func TestParseMCPAddStdioEnv(t *testing.T) {
	e, err := parseMCPAdd([]string{"db", "--env", "PGHOST=localhost", "node", "server.js"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Command != "node" || !reflect.DeepEqual(e.Args, []string{"server.js"}) {
		t.Fatalf("command/args = %q/%v", e.Command, e.Args)
	}
	if e.Env["PGHOST"] != "localhost" {
		t.Errorf("env PGHOST = %q, want localhost", e.Env["PGHOST"])
	}
}

func TestParseMCPAddHTTP(t *testing.T) {
	for _, args := range [][]string{
		{"stripe", "--http", "https://mcp.stripe.com"},
		{"stripe", "--http=https://mcp.stripe.com"},
	} {
		e, err := parseMCPAdd(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if e.Type != "http" || e.URL != "https://mcp.stripe.com" {
			t.Errorf("%v -> type/url = %q/%q", args, e.Type, e.URL)
		}
		if e.Command != "" {
			t.Errorf("%v -> remote entry should have no command, got %q", args, e.Command)
		}
	}
}

func TestParseMCPAddHTTPHeader(t *testing.T) {
	e, err := parseMCPAdd([]string{"x", "--http", "https://x", "--header", "Authorization=Bearer abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("header = %q, want %q", e.Headers["Authorization"], "Bearer abc")
	}
}

func TestParseMCPAddErrors(t *testing.T) {
	cases := map[string][]string{
		"no name":           {},
		"name is a flag":    {"--http", "https://x"},
		"no command/url":    {"fs"},
		"command and url":   {"x", "--http", "https://x", "node"},
		"unknown flag":      {"x", "--bogus", "y", "cmd"},
		"env without value": {"x", "--env"},
	}
	for name, args := range cases {
		if _, err := parseMCPAdd(args); err == nil {
			t.Errorf("%s: expected an error for %v", name, args)
		}
	}
}

func TestTokenizeArgs(t *testing.T) {
	got := tokenizeArgs(`/mcp add s --header "Authorization=Bearer abc" --http https://x`)
	want := []string{"/mcp", "add", "s", "--header", "Authorization=Bearer abc", "--http", "https://x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenizeArgs = %v, want %v", got, want)
	}
	// Single quotes work too, and surrounding whitespace collapses.
	if got := tokenizeArgs("  a  'b c'  d "); !reflect.DeepEqual(got, []string{"a", "b c", "d"}) {
		t.Fatalf("tokenizeArgs single-quote = %v", got)
	}
}

func TestRenderMCPStatusGroupsAndCompactsResources(t *testing.T) {
	longURI := "file:///Users/example/project/docs/really/deep/path/with/a/very/long/resource-name.md"
	got := renderMCPStatus(110,
		[]plugin.ServerStatus{{Name: "docs", Transport: "stdio", Tools: 2}},
		[]plugin.Prompt{{Server: "docs", Name: "mcp__docs__summarize", Description: "Summarize a selected document for review"}},
		[]plugin.Resource{{Server: "docs", URI: longURI, Name: "Resource manual", MimeType: "text/markdown"}},
		nil,
	)
	for _, want := range []string{
		"MCP servers (1)",
		"docs",
		"prompts",
		"/mcp__docs__summarize",
		"resources",
		"@docs:file:///",
		"…",
		"Resource manual [text/markdown]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered MCP status missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, longURI) {
		t.Fatalf("long resource URI should be compacted:\n%s", got)
	}
}

func TestRenderMCPStatusCapsLongSections(t *testing.T) {
	var resources []plugin.Resource
	for i := 0; i < mcpMaxItemsPerSection+2; i++ {
		resources = append(resources, plugin.Resource{Server: "fs", URI: "file:///tmp/resource.md"})
	}
	got := renderMCPStatus(80,
		[]plugin.ServerStatus{{Name: "fs", Transport: "stdio"}},
		nil,
		resources,
		nil,
	)
	if !strings.Contains(got, "+2 more resources") {
		t.Fatalf("rendered MCP status should cap long resource sections:\n%s", got)
	}
}

func TestRenderMCPStatusShowsFailures(t *testing.T) {
	got := renderMCPStatus(90,
		nil,
		nil,
		nil,
		[]plugin.Failure{{Name: "broken", Transport: "stdio", Error: "npm error ENOENT"}},
	)
	for _, want := range []string{"MCP servers (0)", "broken", "npm error ENOENT"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered MCP status missing %q:\n%s", want, got)
		}
	}
}
