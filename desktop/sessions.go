package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"reasonix/internal/fileutil"
)

// errActiveSession is returned when a delete targets the session in use.
var errActiveSession = errors.New("can't delete the session you're in — start a new one first")

// sidecarMu 串行化三个侧车文件(.titles/.display/.cwds)的「读全量→改→原子写回」。
// 原子 rename 只保证文件不损坏,不防丢更新:多标签几乎同时 SubmitDisplay 时,后写会覆盖
// 先写、丢一条 display/cwd 映射。每个读改写整段持锁即可(纯读函数不持锁,靠原子读)(A10)。
var sidecarMu sync.Mutex

// sessions.go holds the desktop-only session-management state that the shared
// kernel doesn't model: custom display titles. A session on disk is just a JSONL
// transcript named by timestamp+model, with no title slot — so the history panel
// stores user-chosen names in a sidecar map (basename → title) next to the .jsonl
// files. The preview (first user message) is the default name; a title overrides
// it. Deleting a session also drops its title entry.

const sessionTitlesFile = ".titles.json"
const sessionDisplayFile = ".display.json"
const sessionCwdsFile = ".cwds.json"   // basename -> 这条会话创建时的 workspace 路径,用于侧栏按文件夹分组
const sessionKindsFile = ".kinds.json" // basename -> 会话类型(如 "hardware"),用于历史侧栏区分垂直

func sessionTitlesPath(dir string) string  { return filepath.Join(dir, sessionTitlesFile) }
func sessionDisplayPath(dir string) string { return filepath.Join(dir, sessionDisplayFile) }
func sessionCwdsPath(dir string) string    { return filepath.Join(dir, sessionCwdsFile) }
func sessionKindsPath(dir string) string   { return filepath.Join(dir, sessionKindsFile) }

// loadSessionCwds 读取 basename→cwd 的映射(类似 loadSessionTitles)。
func loadSessionCwds(dir string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(sessionCwdsPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func saveSessionCwds(dir string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cwds.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, sessionCwdsPath(dir))
}

// rememberSessionCwd 在该 session 首次记录 cwd 时落盘,后续不覆盖
// (会话归属哪个文件夹应当稳定,即便用户中途切换 workspace)。
func rememberSessionCwd(dir, sessionPath, cwd string) error {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(cwd) == "" {
		return nil
	}
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	m := loadSessionCwds(dir)
	key := filepath.Base(sessionPath)
	if _, ok := m[key]; ok {
		return nil // 已有记录,保持不变
	}
	m[key] = cwd
	return saveSessionCwds(dir, m)
}

// loadSessionKinds 读取 basename→kind 的映射(如 "hardware")。会话默认是普通对话(无条目),
// 只有真正用过某垂直定制表面(如硬件工作台跑了编译/烧录/生成代码)才打标,供历史侧栏区分
// "硬件项目 / 普通对话"。打开硬件视图是切 mainView(同一 tab),tab.kind 仍是 chat,所以
// 必须由前端在真正干活时显式 MarkSessionKind,不能靠 tab 类型推断。
func loadSessionKinds(dir string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(sessionKindsPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func saveSessionKinds(dir string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kinds.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, sessionKindsPath(dir))
}

// rememberSessionKind 给某会话打类型标(写入一次即定,后续不覆盖)。
func rememberSessionKind(dir, sessionPath, kind string) error {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(kind) == "" {
		return nil
	}
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	m := loadSessionKinds(dir)
	key := filepath.Base(sessionPath)
	if _, ok := m[key]; ok {
		return nil // 已有记录,保持不变
	}
	m[key] = kind
	return saveSessionKinds(dir, m)
}

// loadSessionTitles reads the basename→title map (missing/corrupt → empty).
func loadSessionTitles(dir string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(sessionTitlesPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

// saveSessionTitles writes the map atomically (temp file + rename).
func saveSessionTitles(dir string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".titles.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, sessionTitlesPath(dir))
}

// setSessionTitle sets (or, with an empty title, clears) a session's custom name.
func setSessionTitle(dir, sessionPath, title string) error {
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	m := loadSessionTitles(dir)
	key := filepath.Base(sessionPath)
	if strings.TrimSpace(title) == "" {
		delete(m, key)
	} else {
		m[key] = strings.TrimSpace(title)
	}
	return saveSessionTitles(dir, m)
}

// deleteSessionFile removes a session's .jsonl and its title entry.
func deleteSessionFile(dir, sessionPath string) error {
	if err := os.Remove(sessionPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	m := loadSessionTitles(dir)
	if _, ok := m[filepath.Base(sessionPath)]; ok {
		delete(m, filepath.Base(sessionPath))
		if err := saveSessionTitles(dir, m); err != nil {
			return err
		}
	}
	if dm := loadSessionDisplays(dir); dm[filepath.Base(sessionPath)] != nil {
		delete(dm, filepath.Base(sessionPath))
		if err := saveSessionDisplays(dir, dm); err != nil {
			return err
		}
	}
	if cm := loadSessionCwds(dir); cm[filepath.Base(sessionPath)] != "" {
		delete(cm, filepath.Base(sessionPath))
		if err := saveSessionCwds(dir, cm); err != nil {
			return err
		}
	}
	if km := loadSessionKinds(dir); km[filepath.Base(sessionPath)] != "" {
		delete(km, filepath.Base(sessionPath))
		if err := saveSessionKinds(dir, km); err != nil {
			return err
		}
	}
	return nil
}

type sessionDisplayMap map[string]map[string]string

func messageDisplayKey(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum[:])
}

func loadSessionDisplays(dir string) sessionDisplayMap {
	m := sessionDisplayMap{}
	b, err := os.ReadFile(sessionDisplayPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func saveSessionDisplays(dir string, m sessionDisplayMap) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".display.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, sessionDisplayPath(dir))
}

func recordSessionDisplay(dir, sessionPath, content, display string) error {
	if strings.TrimSpace(sessionPath) == "" || content == display || strings.TrimSpace(display) == "" {
		return nil
	}
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	m := loadSessionDisplays(dir)
	key := filepath.Base(sessionPath)
	if m[key] == nil {
		m[key] = map[string]string{}
	}
	m[key][messageDisplayKey(content)] = display
	return saveSessionDisplays(dir, m)
}

// sessionDisplayResolver loads the sidecar once and returns a per-message
// resolver, so a transcript of N messages doesn't re-read .display.json N times.
func sessionDisplayResolver(dir, sessionPath string) func(content string) string {
	byHash := loadSessionDisplays(dir)[filepath.Base(sessionPath)]
	return func(content string) string {
		if byHash != nil {
			if display := byHash[messageDisplayKey(content)]; strings.TrimSpace(display) != "" {
				return display
			}
		}
		return content
	}
}

func resolveSessionDisplay(dir, sessionPath, content string) string {
	return sessionDisplayResolver(dir, sessionPath)(content)
}
