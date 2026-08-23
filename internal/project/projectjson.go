package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// FileName 是项目根下唯一允许的散文件名。
const FileName = "project.json"

// stage 的合法闭区间。口径同 NASApp `web/main.py:6369 _project_stage_int`：
// 必须是 1–8 的**数学整数**，`3.9` 判非法而不是截断成 3。
const (
	StageMin = 1
	StageMax = 8
)

// stage_source 的枚举（01_M0决策记录 §3.2）。
const (
	SourceKanbanMigration = "kanban-migration" // M5 冻结日一次性迁移
	SourceOneCreat        = "onecreat"         // 决策收件箱拨段位
	SourceSyncInit        = "sync-init"        // 历史同步/建档写的初值
	SourceManual          = "manual"           // 人手改
)

var stageSources = map[string]bool{
	SourceKanbanMigration: true,
	SourceOneCreat:        true,
	SourceSyncInit:        true,
	SourceManual:          true,
}

// 字段名。集中在这里，免得散落成字符串字面量。
const (
	keyCode             = "code"
	keyShortName        = "short_name"
	keyFullName         = "full_name"
	keyStudent          = "student"
	keyLine             = "line"
	keyAssignee         = "assignee"
	keyNeedsOutsourcing = "needs_outsourcing"
	keyStage            = "stage"
	keyStageUpdatedAt   = "stage_updated_at"
	keyStageSource      = "stage_source"
)

// ---------------------------------------------------------------------------
// 有序 JSON 表示
// ---------------------------------------------------------------------------

// kind 是一个 JSON 值的类型。
type kind int

const (
	kObject kind = iota
	kArray
	kString
	kNumber
	kBool
	kNull
)

// value 是一个保序的 JSON 值。
//
// 为什么不用 map[string]any + encoding/json：Go 的 struct 按字段顺序输出、map 按
// 键名排序输出，而全库 27 份 project.json 存在 **4 种键序**（三条 Python 写入路径
// 各自 append 形成）。任何"规范化键序"的写法都会把其中至少 16 份改脏，
// 于是这里自己留一份键的原始顺序。
type value struct {
	kind kind
	// kObject：keys/vals 平行数组，保留原始键序（允许重键，原样保留）
	keys []string
	vals []*value
	// kArray
	items []*value
	// kString
	str string
	// kNumber：**原样保留字面量**，不经 float64 往返，免得 3 变成 3.0
	num string
	// kBool
	b bool
}

// get 按键取值；不存在返回 nil。
func (v *value) get(key string) *value {
	if v == nil || v.kind != kObject {
		return nil
	}
	for i, k := range v.keys {
		if k == key {
			return v.vals[i]
		}
	}
	return nil
}

// has 报告键是否存在（区别于"存在但是 null"）。
func (v *value) has(key string) bool {
	return v.get(key) != nil
}

// set 写一个键：**已存在就原位更新**（不动键序），不存在就 append 到末尾。
// append 到末尾与三个外部 Python 脚本的行为一致（01_M0决策记录 §3.2 约束 1）。
func (v *value) set(key string, nv *value) {
	for i, k := range v.keys {
		if k == key {
			v.vals[i] = nv
			return
		}
	}
	v.keys = append(v.keys, key)
	v.vals = append(v.vals, nv)
}

// Doc 是一份 project.json 的保序表示，外加"原文件有没有尾换行"这一位。
//
// 尾换行必须记住：27 份真实文件里 26 份有、P26C-028 没有，写回时要原样保留
// （05_夹具与基线 §1）。
type Doc struct {
	root            *value
	trailingNewline bool
}

// ParseDoc 解析一份 project.json 正文。
func ParseDoc(data []byte) (*Doc, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// 数字一律按字面量拿，绝不经 float64 —— 否则 stage=3 会被写回成 3 或 3.0 看运气。
	dec.UseNumber()
	root, err := parseValue(dec)
	if err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	if root.kind != kObject {
		return nil, errors.New("project.json 顶层必须是对象")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("project.json 顶层对象之后还有多余内容")
	}
	return &Doc{root: root, trailingNewline: bytes.HasSuffix(data, []byte("\n"))}, nil
}

func parseValue(dec *json.Decoder) (*value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return parseFrom(dec, tok)
}

func parseFrom(dec *json.Decoder, tok json.Token) (*value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &value{kind: kObject}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("对象的键不是字符串: %v", kt)
				}
				child, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.keys = append(v.keys, key)
				v.vals = append(v.vals, child)
			}
			if _, err := dec.Token(); err != nil { // 吃掉 '}'
				return nil, err
			}
			return v, nil
		case '[':
			v := &value{kind: kArray}
			for dec.More() {
				child, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.items = append(v.items, child)
			}
			if _, err := dec.Token(); err != nil { // 吃掉 ']'
				return nil, err
			}
			return v, nil
		default:
			return nil, fmt.Errorf("意外的定界符 %q", t)
		}
	case string:
		return &value{kind: kString, str: t}, nil
	case json.Number:
		return &value{kind: kNumber, num: t.String()}, nil
	case bool:
		return &value{kind: kBool, b: t}, nil
	case nil:
		return &value{kind: kNull}, nil
	default:
		return nil, fmt.Errorf("无法识别的 JSON 词法单元 %T", tok)
	}
}

// Bytes 渲染成字节。
//
// 输出必须与 Python `json.dumps(obj, ensure_ascii=False, indent=2)`（+ 尾换行）
// **逐字节一致**：2 空格缩进、`": "` / `",\n"` 分隔、中文明文（等价于
// SetEscapeHTML(false)）、LF 行尾、尾换行按原文件。
func (d *Doc) Bytes() []byte {
	var buf bytes.Buffer
	writeValue(&buf, d.root, 0)
	if d.trailingNewline {
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func writeIndent(buf *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		buf.WriteString("  ")
	}
}

func writeValue(buf *bytes.Buffer, v *value, depth int) {
	switch v.kind {
	case kObject:
		if len(v.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, k := range v.keys {
			writeIndent(buf, depth+1)
			writeJSONString(buf, k)
			buf.WriteString(": ")
			writeValue(buf, v.vals[i], depth+1)
			if i < len(v.keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeIndent(buf, depth)
		buf.WriteByte('}')
	case kArray:
		if len(v.items) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, item := range v.items {
			writeIndent(buf, depth+1)
			writeValue(buf, item, depth+1)
			if i < len(v.items)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeIndent(buf, depth)
		buf.WriteByte(']')
	case kString:
		writeJSONString(buf, v.str)
	case kNumber:
		buf.WriteString(v.num)
	case kBool:
		if v.b {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case kNull:
		buf.WriteString("null")
	}
}

// writeJSONString 按 Python `ensure_ascii=False` 的口径写字符串：
// 只转义 `"`、`\` 与控制字符，非 ASCII 一律明文输出。
//
// 与 Go `json.Encoder` + SetEscapeHTML(false) 的唯一差别是 U+2028 / U+2029：
// Go 无论如何都会把它们转义，Python 不会。这里跟 Python 走——文件的另一个写入方
// 是那三个 Python 脚本，跟它对齐才不会两边来回改格式。
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

// Assignee 是项目负责人/机构。三键顺序固定 type / id / name。
type Assignee struct {
	Type string // student | org
	ID   string
	Name string
}

// Project 是一个已建档的科创项目。Dir 是项目根目录（`P26X-NNN_短名/`）。
type Project struct {
	Dir string
	doc *Doc
}

// Load 读 <dir>/project.json。
func Load(dir string) (*Project, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := ParseDoc(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Project{Dir: dir, doc: doc}, nil
}

// Doc 暴露底层保序文档，供只读的字节回环测试使用。
func (p *Project) Doc() *Doc { return p.doc }

func (p *Project) str(key string) string {
	v := p.doc.root.get(key)
	if v == nil || v.kind != kString {
		return ""
	}
	return v.str
}

// Code 是项目编号 P26X-NNN，三端身份证。
func (p *Project) Code() string { return p.str(keyCode) }

// ShortName 是项目短名（目录名的后半段）。
func (p *Project) ShortName() string { return p.str(keyShortName) }

// FullName 是课题全名。
func (p *Project) FullName() string { return p.str(keyFullName) }

// Line 是业务线 C / B / G。
func (p *Project) Line() string { return p.str(keyLine) }

// Student 是学生（或 `00_未指派`）。
func (p *Project) Student() string { return p.str(keyStudent) }

// NeedsOutsourcing 报告是否需要外协；键缺失或类型不对时第二个返回值为 false。
func (p *Project) NeedsOutsourcing() (bool, bool) {
	v := p.doc.root.get(keyNeedsOutsourcing)
	if v == nil || v.kind != kBool {
		return false, false
	}
	return v.b, true
}

// Stage 返回段位 1–8。
//
// 必须是 1–8 的**数学整数**：`3.9` 判非法（返回 false）而不是截断成 3，`3.0`
// 是数学整数按 3 接受，布尔与越界值判非法 —— 这几条与 NASApp
// `web/main.py:6369 _project_stage_int` 逐条对齐。
//
// 唯一**故意更严**的一处：字符串 `"3"` 这里判非法，而 `_project_stage_int` 会
// `float(raw)` 把它收下。理由是那个函数是 P 卡侧的容错入口，而管着 project.json
// 的是 `project_sync.py:517 validate_project_meta` —— 它要求严格的 JSON 整数，
// 一个 `"3"` 就会让全部 27 个项目的同步整轮中止。宽进这里等于把炸弹递给同步。
func (p *Project) Stage() (int, bool) {
	v := p.doc.root.get(keyStage)
	if v == nil || v.kind != kNumber {
		return 0, false
	}
	return stageFromLiteral(v.num)
}

func stageFromLiteral(lit string) (int, bool) {
	n, err := strconv.Atoi(lit)
	if err != nil {
		f, ferr := strconv.ParseFloat(lit, 64)
		if ferr != nil || f != float64(int64(f)) {
			return 0, false // 3.9 判非法，绝不截断
		}
		n = int(int64(f))
	}
	if n < StageMin || n > StageMax {
		return 0, false
	}
	return n, true
}

// StageUpdatedAt 是最后一次 stage 变更时间（RFC3339）。
// 键缺失时返回空串 —— 按 01_M0决策记录 §3.2 约束 3，缺失即"未知"。
func (p *Project) StageUpdatedAt() string { return p.str(keyStageUpdatedAt) }

// StageSource 是这一次 stage 是谁写的。
// 键缺失时返回 SourceSyncInit —— 同上，缺失一律视为历史同步写的初值。
func (p *Project) StageSource() string {
	if !p.doc.root.has(keyStageSource) {
		return SourceSyncInit
	}
	return p.str(keyStageSource)
}

// Assignee 的三态：
//
//	(nil, false) —— 键整个不存在（27 份里 11 份如此）
//	(nil, true)  —— 键存在但是 null（未分配，5 份）
//	(&a,  true)  —— 键存在且是对象（11 份）
//
// 这三态必须分得开：否则"未分配"会被写成"键缺失"，反之亦然。
func (p *Project) Assignee() (*Assignee, bool) {
	v := p.doc.root.get(keyAssignee)
	if v == nil {
		return nil, false
	}
	if v.kind != kObject {
		return nil, true // null（或任何非对象）都按"存在但未分配"处理
	}
	a := &Assignee{}
	if t := v.get("type"); t != nil && t.kind == kString {
		a.Type = t.str
	}
	if id := v.get("id"); id != nil && id.kind == kString {
		a.ID = id.str
	}
	if n := v.get("name"); n != nil && n.kind == kString {
		a.Name = n.str
	}
	return a, true
}

// SetStage 拨段位。
//
// 写法遵守 01_M0决策记录 §3.2 的兼容性约束：`stage` 已存在则**原位更新**（不动
// 键序），`stage_updated_at` / `stage_source` 不存在时 **append 到文件末尾**、
// 存在时原位更新。不删、不改名、不重排任何现有字段。
//
// 只改内存，落盘要再调 Save。
func (p *Project) SetStage(n int, source string, now time.Time) error {
	if n < StageMin || n > StageMax {
		return fmt.Errorf("stage 必须是 %d–%d 的整数，实际 %d", StageMin, StageMax, n)
	}
	if !stageSources[source] {
		return fmt.Errorf("stage_source 取值非法: %q（合法值：%s / %s / %s / %s）",
			source, SourceKanbanMigration, SourceOneCreat, SourceSyncInit, SourceManual)
	}
	p.doc.root.set(keyStage, &value{kind: kNumber, num: strconv.Itoa(n)})
	p.doc.root.set(keyStageUpdatedAt, &value{kind: kString, str: now.Format(time.RFC3339)})
	p.doc.root.set(keyStageSource, &value{kind: kString, str: source})
	return nil
}

// Save 原子写回 <Dir>/project.json（同目录 tmp + rename）。
func (p *Project) Save() error {
	path := filepath.Join(p.Dir, FileName)
	data := p.doc.Bytes()

	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm() // 原文件权限原样保留
	}

	tmp, err := os.CreateTemp(p.Dir, ".project.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是 no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
