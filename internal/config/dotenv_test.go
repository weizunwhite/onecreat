package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDotEnvFallsBackToHome proves the unified-key behaviour: the working
// directory's .env wins, but a key only present in ~/.env is still picked up —
// so a key set once in the home .env (the desktop app writes there) reaches the
// CLI run from any project directory. Existing env vars beat both files.
//
// 断言点从 os.Getenv 换成了返回的叠加层:C1 之后 `.env` 不再写进进程环境,叠加层
// 才是这些值的去处。优先级语义(cwd 胜 home、进程环境胜两者)一个字没变。
func TestLoadDotEnvFallsBackToHome(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()

	if err := os.WriteFile(filepath.Join(cwd, ".env"), []byte("KEY_CWD=from_cwd\nKEY_SHARED=cwd_wins\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("KEY_HOME=from_home\nKEY_SHARED=home_loses\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cwd)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows.

	// Start clean so the file values are what land (Setenv auto-restores).
	t.Setenv("KEY_CWD", "")
	os.Unsetenv("KEY_CWD")
	t.Setenv("KEY_HOME", "")
	os.Unsetenv("KEY_HOME")
	t.Setenv("KEY_SHARED", "")
	os.Unsetenv("KEY_SHARED")

	env := loadDotEnv()

	if got := env.Get("KEY_CWD"); got != "from_cwd" {
		t.Errorf("cwd-only key not loaded: KEY_CWD=%q", got)
	}
	if got := env.Get("KEY_HOME"); got != "from_home" {
		t.Errorf("~/.env fallback failed: KEY_HOME=%q want from_home", got)
	}
	if got := env.Get("KEY_SHARED"); got != "cwd_wins" {
		t.Errorf("cwd .env should take precedence over ~/.env: KEY_SHARED=%q want cwd_wins", got)
	}
	// 而且它们**没有**污染进程环境 —— 这正是两个工作区能隔离的前提。
	for _, k := range []string{"KEY_CWD", "KEY_HOME", "KEY_SHARED"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf(".env 写进了进程环境:%s=%q", k, v)
		}
	}
}

// TestLoadDotEnvDoesNotOverrideEnv confirms an already-set environment variable
// beats both .env files (the documented first-wins contract).
func TestLoadDotEnvDoesNotOverrideEnv(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env"), []byte("PINNED=from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PINNED", "from_env")

	env := loadDotEnv()

	if got := env.Get("PINNED"); got != "from_env" {
		t.Errorf("env var must win over .env: PINNED=%q want from_env", got)
	}
	// 子进程看到的也必须是进程环境那份。
	if got := lookupIn(env.Environ(), "PINNED"); got != "from_env" {
		t.Errorf("子进程环境里 PINNED=%q want from_env", got)
	}
}
