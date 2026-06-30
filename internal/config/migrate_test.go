package config

import (
	"os"
	"path/filepath"
	"testing"
)

// R3 回归:状态目录从旧名 reasonix 迁移到 onecreat。旧目录里的 config.toml / sessions 等被
// 搬进新目录;新目录里已存在的条目(如账号 session.json 先建)不被覆盖;迁移后旧条目消失。
func TestStateRootMigratesLegacyReasonixDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("REASONIX_CONFIG_DIR", "") // 用默认位置才会触发迁移

	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	oldRoot := filepath.Join(base, "reasonix")
	newRoot := filepath.Join(base, "onecreat")

	// 旧目录:有 config.toml + sessions/ 子目录。
	if err := os.MkdirAll(filepath.Join(oldRoot, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "config.toml"), []byte("default_model=\"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "sessions", "s1.jsonl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 新目录:账号 session.json 已先存在(模拟登录早于内核首次写配置)。
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "session.json"), []byte("{\"token\":\"t\"}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 触发迁移(任一走 stateRoot 的函数)。
	got := MemoryUserDir()
	if got != newRoot {
		t.Fatalf("MemoryUserDir = %q, want %q", got, newRoot)
	}

	// config.toml 与 sessions/ 已搬进新目录。
	if _, err := os.Stat(filepath.Join(newRoot, "config.toml")); err != nil {
		t.Errorf("config.toml 未迁移到新目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "sessions", "s1.jsonl")); err != nil {
		t.Errorf("sessions/ 未迁移到新目录: %v", err)
	}
	// 新目录原有的 session.json 未被动。
	if _, err := os.Stat(filepath.Join(newRoot, "session.json")); err != nil {
		t.Errorf("已存在的 session.json 不应被迁移破坏: %v", err)
	}
	// 旧目录的 config.toml 已被搬走(读旧写新:迁移而非复制)。
	if _, err := os.Stat(filepath.Join(oldRoot, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("旧目录 config.toml 应已被搬走,err=%v", err)
	}
}

// 已是 onecreat、无旧目录时:不报错、返回新根。
func TestStateRootFreshInstallNoMigration(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("REASONIX_CONFIG_DIR", "")

	base, _ := os.UserConfigDir()
	if got := MemoryUserDir(); got != filepath.Join(base, "onecreat") {
		t.Fatalf("fresh install MemoryUserDir = %q, want .../onecreat", got)
	}
}

// REASONIX_CONFIG_DIR 显式覆盖时:直接用它、不迁移。
func TestStateRootHonorsExplicitOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("REASONIX_CONFIG_DIR", tmp)
	if got := MemoryUserDir(); got != tmp {
		t.Fatalf("override MemoryUserDir = %q, want %q", got, tmp)
	}
	if got := UserConfigPath(); got != filepath.Join(tmp, "config.toml") {
		t.Fatalf("override UserConfigPath = %q", got)
	}
}
