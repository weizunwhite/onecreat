package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// A3 回归:RemovePluginFromFile 只删目标 [[plugins]] 块,项目文件其余内容(default_model、
// 别的插件)原样保留,且【绝不】注入 Default 的 provider/default_model 等无关键(那是 1b
// 全量快照遮蔽的根因)。
func TestRemovePluginFromFileSurgical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onecreat.toml")
	src := `default_model = "deepseek-pro"

[[plugins]]
name    = "foo"
command = "foo-server"

[[plugins]]
name    = "bar"
command = "bar-server"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemovePluginFromFile(path, "foo")
	if err != nil {
		t.Fatalf("RemovePluginFromFile: %v", err)
	}
	if !removed {
		t.Fatal("removed = false, want true")
	}

	out, _ := os.ReadFile(path)
	text := string(out)
	if strings.Contains(text, `"foo"`) || strings.Contains(text, "foo-server") {
		t.Fatalf("foo 块未删干净:\n%s", text)
	}
	if !strings.Contains(text, `name    = "bar"`) {
		t.Fatalf("bar 块被误删:\n%s", text)
	}
	if !strings.Contains(text, `default_model = "deepseek-pro"`) {
		t.Fatalf("default_model 被改动:\n%s", text)
	}
	// 关键:不得多出全量快照的无关键。
	if strings.Contains(text, "[[providers]]") {
		t.Fatalf("项目文件被撑成全量快照(多出 [[providers]]):\n%s", text)
	}
	// 结果仍是合法 TOML,且只剩 bar 一个插件。
	var cfg Config
	if _, derr := toml.Decode(text, &cfg); derr != nil {
		t.Fatalf("删除后不是合法 TOML: %v", derr)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].Name != "bar" {
		t.Fatalf("剩余插件 = %+v, want 仅 bar", cfg.Plugins)
	}
}

// [plugins.env] 子表属于其上的 [[plugins]] 块,删除时必须一并删掉,不能把子表留成孤儿。
func TestRemovePluginFromFileWithSubTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onecreat.toml")
	src := `[[plugins]]
name = "foo"
command = "foo-server"

[plugins.env]
TOKEN = "x"

[[plugins]]
name = "bar"
command = "bar-server"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemovePluginFromFile(path, "foo")
	if err != nil || !removed {
		t.Fatalf("remove foo: removed=%v err=%v", removed, err)
	}
	out, _ := os.ReadFile(path)
	if strings.Contains(string(out), "[plugins.env]") || strings.Contains(string(out), "TOKEN") {
		t.Fatalf("foo 的 [plugins.env] 子表未一并删除:\n%s", string(out))
	}
	var cfg Config
	if _, derr := toml.Decode(string(out), &cfg); derr != nil {
		t.Fatalf("删除后不是合法 TOML: %v", derr)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].Name != "bar" {
		t.Fatalf("剩余插件 = %+v, want 仅 bar", cfg.Plugins)
	}
}

// 名字不存在 → (false,nil) 且文件一字不改。文件不存在也 → (false,nil)。
func TestRemovePluginFromFileAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onecreat.toml")
	src := "[[plugins]]\nname = \"foo\"\ncommand = \"foo-server\"\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemovePluginFromFile(path, "nope")
	if err != nil || removed {
		t.Fatalf("absent name: removed=%v err=%v, want false,nil", removed, err)
	}
	out, _ := os.ReadFile(path)
	if string(out) != src {
		t.Fatalf("文件被改动,应原样:\n%s", string(out))
	}
	// 文件不存在。
	removed, err = RemovePluginFromFile(filepath.Join(dir, "missing.toml"), "foo")
	if err != nil || removed {
		t.Fatalf("missing file: removed=%v err=%v, want false,nil", removed, err)
	}
}
