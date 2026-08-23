// 项目运行台账 `.onecreat/workflow.jsonl`。
//
// append-only、一行一个 JSON 事件，作用是"这个项目跑过哪些材料作业、结果如何"
// 可以被重放（status / UI / 对账都读它）。
//
// **落点是本地私有的，NAS 总库里永远不会有它**：`project_sync.py`
// `collect_project_files:588-590` 整树排除 dot 路径。这是 05_夹具与基线 §6 第 2 条
// 的三选一里选定的 (a) 方案——接受本地不镜像，别指望 NAS 有它。
package workflow

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 台账目录与文件名。
const (
	journalDirName  = ".onecreat"
	journalFileName = "workflow.jsonl"
)

// 事件类型。新增类型要同时加进 journalKinds。
const (
	KindMaterialStarted   = "material_started"
	KindMaterialSucceeded = "material_succeeded"
	KindMaterialFailed    = "material_failed"
	KindMaterialSkipped   = "material_skipped"
	KindQualityNote       = "quality_note"
)

// journalKinds 是合法事件类型集合。写入时校验——台账是给人和程序回放的，
// 拼错的 kind 静默写进去比报错难查得多。
var journalKinds = map[string]bool{
	KindMaterialStarted:   true,
	KindMaterialSucceeded: true,
	KindMaterialFailed:    true,
	KindMaterialSkipped:   true,
	KindQualityNote:       true,
}

// Event 是台账里的一行。
//
// SessionHint 与 ReceiptsSummary 由**装配层**填（会话 id、evidence receipts 摘要），
// 领域层拿不到这两样，一律留空——留空即 omitempty，不会在台账里落出空字段。
type Event struct {
	TS              time.Time `json:"ts"`
	Kind            string    `json:"kind"`
	Material        string    `json:"material,omitempty"`
	SessionHint     string    `json:"session_hint,omitempty"`
	Outputs         []string  `json:"outputs,omitempty"`
	ReceiptsSummary string    `json:"receipts_summary,omitempty"`
	Retries         int       `json:"retries,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

// JournalPath 这个函数做什么：给出一个项目的台账文件绝对路径。
func JournalPath(projDir string) string {
	return filepath.Join(projDir, journalDirName, journalFileName)
}

// Append 这个函数做什么：往项目台账尾部追加一个事件。
//
// O_APPEND 单次写一整行，`.onecreat/` 不存在则建。TS 为零值时补当前时间。
func Append(projDir string, ev Event) error {
	if projDir == "" {
		return fmt.Errorf("项目目录为空")
	}
	if !journalKinds[ev.Kind] {
		return fmt.Errorf("非法台账事件类型 %q", ev.Kind)
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("台账事件序列化失败：%w", err)
	}
	dir := filepath.Join(projDir, journalDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建台账目录失败：%w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, journalFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开台账失败：%w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("写台账失败：%w", err)
	}
	return nil
}

// Replay 这个函数做什么：把项目台账按写入顺序读回来。
//
// 台账文件不存在返回空切片而不是错误——没跑过作业不是错。坏行（半行、
// 手工编辑坏掉的 JSON）**跳过**，不 panic 也不让整份台账作废；跳了几行用
// ReplaySkipped 拿。
func Replay(projDir string) ([]Event, error) {
	evs, _, err := ReplaySkipped(projDir)
	return evs, err
}

// ReplaySkipped 与 Replay 相同，另外返回被跳过的坏行数。
func ReplaySkipped(projDir string) ([]Event, int, error) {
	f, err := os.Open(JournalPath(projDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("打开台账失败：%w", err)
	}
	defer f.Close()

	var out []Event
	skipped := 0
	sc := bufio.NewScanner(f)
	// 单行上限调大：Outputs 可能是一长串路径。
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Kind == "" {
			skipped++
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return out, skipped, fmt.Errorf("读台账失败：%w", err)
	}
	return out, skipped, nil
}
