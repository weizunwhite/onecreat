package numbering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RegistryFileName 是登记簿文件名,与 NAS 侧 PROJECT_CODES_FILE 同名(main.py:118)。
const RegistryFileName = "project_codes.json"

const (
	// lockSuffix 跨进程互斥用的锁文件后缀。
	lockSuffix = ".lock"
	// lockWait 抢锁最长等待时间。
	lockWait = 5 * time.Second
	// lockPoll 抢锁重试间隔。
	lockPoll = 20 * time.Millisecond
	// lockStale 锁文件超过这个岁数就认为是崩溃残留,可以抢占。
	// 没有这一条的话,一次进程崩溃会把登记簿永久锁死。
	lockStale = 60 * time.Second
)

// Entry 是登记簿里一个编号的登记信息。
//
// 结构核对状态:已与 NAS 上的真实文件
// /share/CACHEDEV1_DATA/.appdata/topic_board/project_codes.json 逐字段比对过
// (2026-08-23 只读拉取,28 条,next=32,字段集恰为 {short_name, student, line})。
// 写入端口径见 main.py:4740-4752(codes[code] = {"short_name","student","line"})
// 与 main.py:4700-4706(手建 P 卡发号,student 留空)。
// **历史号 P26-001~P26-018 没有 line 键**,所以 Line 为空时不补这个键,保持原样。
type Entry struct {
	ShortName string
	Student   string
	Line      string

	// extra 保留读进来时遇到的未知字段,写回时原样带出去。
	// 登记簿是不可再生资产:NAS 侧哪天加了字段,OneCreat 不能因为不认识就把它抹掉。
	extra map[string]json.RawMessage
}

// UnmarshalJSON 读一条登记,已知字段落到结构体,其余原样存进 extra。
func (e *Entry) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	pick := func(key string) (string, error) {
		v, ok := raw[key]
		if !ok {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", fmt.Errorf("字段 %s 不是字符串", key)
		}
		delete(raw, key)
		return s, nil
	}
	var err error
	if e.ShortName, err = pick("short_name"); err != nil {
		return err
	}
	if e.Student, err = pick("student"); err != nil {
		return err
	}
	if e.Line, err = pick("line"); err != nil {
		return err
	}
	e.extra = raw
	return nil
}

// MarshalJSON 写一条登记:未知字段先铺底,已知字段再覆盖上去。
func (e Entry) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(e.extra)+3)
	for k, v := range e.extra {
		out[k] = v
	}
	out["short_name"] = e.ShortName
	out["student"] = e.Student
	if e.Line != "" { // 历史号本来就没这个键,别替它补
		out["line"] = e.Line
	}
	return json.Marshal(out)
}

// Registry 是整本登记簿:{"next": N, "codes": {...}}。
type Registry struct {
	Next  int              `json:"next"`
	Codes map[string]Entry `json:"codes"`
}

// Local 是 M5 冻结日之后的发号权威:直接读写本地登记簿文件。
//
// 现在就实现好是为了 M5 那天只改配置不改代码(01_M0决策记录 §1.1 M1 约束:
// 同期可写 Local,但**只用于单测与离线自检**,不许接真实立项链)。
//
// 并发口径:进程内一把 mutex,进程间一个锁文件;写用「临时文件 + rename」原子落盘,
// 与 NAS 侧 _save_project_codes(main.py:4563)同一做法。
type Local struct {
	path string
	mu   sync.Mutex
}

var _ Issuer = (*Local)(nil)

// NewLocal 构造本地发号器。dir 是登记簿所在目录(M5 后为 课题/.onecreat/),
// 文件名固定 project_codes.json。
func NewLocal(dir string) *Local {
	return &Local{path: filepath.Join(dir, RegistryFileName)}
}

// Path 返回登记簿文件的绝对/相对路径,便于 M5 对账脚本引用。
func (l *Local) Path() string { return l.path }

// Preview 只读预告下一编号,不占号、不落盘。
func (l *Local) Preview(_ context.Context, line string) (string, error) {
	clean, err := NormalizeLine(line)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	release, err := acquireLock(l.path + lockSuffix)
	if err != nil {
		return "", err
	}
	defer release()

	reg, err := l.load()
	if err != nil {
		return "", err
	}
	return previewLocked(reg, clean), nil
}

// Issue 正式占号:算号 → 回验 ExpectedCode → 登记 → 落盘。整段在锁内完成。
func (l *Local) Issue(_ context.Context, req Request) (string, error) {
	line, err := validateRequest(req)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	release, err := acquireLock(l.path + lockSuffix)
	if err != nil {
		return "", err
	}
	defer release()

	reg, err := l.load()
	if err != nil {
		return "", err
	}
	code := previewLocked(reg, line)
	if req.ExpectedCode != "" && req.ExpectedCode != code {
		return "", fmt.Errorf("%w(预告 %s，当前 %s)", ErrCodeMoved, req.ExpectedCode, code)
	}
	seq, ok := SequenceOf(code)
	if !ok { // previewLocked 自己拼的号,拼不出序号只能是代码坏了
		return "", fmt.Errorf("%w:内部算出的编号 %q 不可解析", ErrBadCode, code)
	}
	// student 留空:Request 里没有这个字段,与 NAS 侧「手建 P 卡即发号」(main.py:4704)
	// 同口径 —— 分配学生是另一件事,由 assign_project 一侧回写。
	reg.Codes[code] = Entry{ShortName: req.ShortName, Student: "", Line: line}
	reg.Next = seq + 1
	if err := l.save(reg); err != nil {
		return "", err
	}
	return code, nil
}

// previewLocked 与 main.py:4659 _preview_project_code_locked 逐行对齐:
// 从 next 起往上找第一个没被占用的序号。**三条业务线共用同一个序号池**,
// 所以只要后三位被任何一条线占了就跳过(main.py:4650 _project_sequence_taken)。
//
// 注意这个算法天然**不回填空洞**:它从 next 起步,next 以下的空号(现状 021–023)
// 永远不会被再走到 —— 与 NAS 行为一致,不要"优化"成从 1 开始扫。
func previewLocked(reg *Registry, line string) string {
	seq := max(1, reg.Next)
	for sequenceTaken(reg.Codes, seq) {
		seq++
	}
	return formatCode(line, seq)
}

// sequenceTaken 判断某个后三位序号是否已被任何一条业务线占用。
func sequenceTaken(codes map[string]Entry, seq int) bool {
	for code := range codes {
		if got, ok := SequenceOf(code); ok && got == seq {
			return true
		}
	}
	return false
}

// load 读登记簿。文件不存在或损坏一律 fail-closed:发号是"唯一权威"型数据,
// 凭空造一本空簿子会从 001 重新发号,直接撞掉全部存量项目(D-1 列的不可逆后果)。
func (l *Local) load() (*Registry, error) {
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w:%s(M5 冻结日须先从 NAS 全量导入)", ErrNoRegistry, l.path)
	}
	if err != nil {
		return nil, fmt.Errorf("读项目编号登记簿失败: %w", err)
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("%w:%v", ErrRegistryBroken, err)
	}
	if reg.Codes == nil {
		return nil, fmt.Errorf("%w:codes 不是对象", ErrRegistryBroken)
	}
	if reg.Next < 1 { // 对齐 main.py 的 int(data.get("next") or 1)
		reg.Next = 1
	}
	return &reg, nil
}

// save 原子写回:同目录临时文件 + rename(与 main.py:4563 _save_project_codes 同做法)。
func (l *Local) save(reg *Registry) error {
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化项目编号登记簿失败: %w", err)
	}
	body = append(body, '\n')
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("写项目编号登记簿临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘项目编号登记簿失败: %w", err)
	}
	return nil
}

// acquireLock 用 O_CREATE|O_EXCL 抢一个锁文件,拿不到就退避重试。
// 返回的 release 必须被调用(defer),否则锁要等 lockStale 才会被别人抢占。
func acquireLock(path string) (func(), error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "pid=%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("抢项目编号登记簿锁失败: %w", err)
		}
		// 崩溃残留的锁要能被抢占,否则登记簿会被永久锁死。
		if st, statErr := os.Stat(path); statErr == nil && time.Since(st.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等项目编号登记簿锁超时(%s),疑似有别的进程正在发号", lockWait)
		}
		time.Sleep(lockPoll)
	}
}
