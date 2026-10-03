package numbering

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// 夹具 = 04_D09三方计数对账 的现状快照:27 条,next=31,序号 021–023 是空洞。
// 前 18 条是历史号(无 line 键),后 9 条带 line —— 与 NAS 真实文件的两种形态一致。
const d09Registry = `{
  "next": 31,
  "codes": {
    "P26-001": {"short_name": "吸尘黑板擦", "student": "学生002"},
    "P26-002": {"short_name": "路灯防虫", "student": "学生003"},
    "P26-003": {"short_name": "双电梯调度", "student": "学生004"},
    "P26-004": {"short_name": "微观摄影箱", "student": "学生005"},
    "P26-005": {"short_name": "标本转看台", "student": "学生006"},
    "P26-006": {"short_name": "草坪小管家", "student": "学生008"},
    "P26-007": {"short_name": "还书机", "student": "学生009"},
    "P26-008": {"short_name": "垃圾清运调度", "student": "学生106"},
    "P26-009": {"short_name": "纹影观测仪", "student": "学生104"},
    "P26-010": {"short_name": "智能浑仪", "student": "学生010"},
    "P26-011": {"short_name": "轮椅康复装置", "student": "学生101"},
    "P26-012": {"short_name": "智能眼保健操", "student": "学生011"},
    "P26-013": {"short_name": "松果湿度装置", "student": "学生012"},
    "P26-014": {"short_name": "平衡训练站", "student": "学生103"},
    "P26-015": {"short_name": "运河监测船", "student": "学生108"},
    "P26-016": {"short_name": "Mac散热监控", "student": "00_机构自研"},
    "P26-017": {"short_name": "眼控轮椅", "student": "学生107"},
    "P26-018": {"short_name": "动态疏散", "student": "学生105"},
    "P26C-019": {"short_name": "三端联调", "student": "00_待分配", "line": "C"},
    "P26C-020": {"short_name": "飞鸟志", "student": "学生102", "line": "C"},
    "P26B-024": {"short_name": "种子活力", "student": "机构甲", "line": "B"},
    "P26B-025": {"short_name": "震颤仪", "student": "机构甲", "line": "B"},
    "P26B-026": {"short_name": "配餐台", "student": "00_待分配", "line": "B"},
    "P26B-027": {"short_name": "导航鞋", "student": "00_待分配", "line": "B"},
    "P26C-028": {"short_name": "v16联调2", "student": "00_待分配", "line": "C"},
    "P26C-029": {"short_name": "天蓝极值", "student": "00_待分配", "line": "C"},
    "P26B-030": {"short_name": "追光清洁板", "student": "机构乙", "line": "B"}
  }
}`

// newLocalWith 在临时目录里铺一本登记簿并返回发号器。
func newLocalWith(t *testing.T, body string) *Local {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RegistryFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	return NewLocal(dir)
}

func readRegistry(t *testing.T, l *Local) Registry {
	t.Helper()
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("读回登记簿失败: %v", err)
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("登记簿不是合法 JSON: %v", err)
	}
	return reg
}

// 从 D09 现状接管:下一号是 031,021–023 的空洞不回填。
func TestLocalPreviewTakesOverD09StateWithoutBackfill(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	for _, line := range Lines {
		got, err := l.Preview(context.Background(), line)
		if err != nil {
			t.Fatalf("Preview(%s) 失败: %v", line, err)
		}
		if want := "P26" + line + "-031"; got != want {
			t.Fatalf("Preview(%s) = %s，想要 %s(空洞 021–023 不该被回填)", line, got, want)
		}
	}
	// 只读:预告不许动文件。
	if reg := readRegistry(t, l); reg.Next != 31 || len(reg.Codes) != 27 {
		t.Fatalf("Preview 改动了登记簿: next=%d codes=%d", reg.Next, len(reg.Codes))
	}
}

// 三线共用一个序号池:next 落在别的线已占的序号上时要往后跳。
func TestLocalPreviewSkipsSequencesTakenByOtherLines(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	// 手工把 next 拨回 24(024–030 已被 B/C 线占满),下一个可用只能是 031。
	reg := readRegistry(t, l)
	reg.Next = 24
	writeRegistry(t, l, reg)

	got, err := l.Preview(context.Background(), "G")
	if err != nil {
		t.Fatalf("Preview 失败: %v", err)
	}
	if got != "P26G-031" {
		t.Fatalf("Preview = %s，想要 P26G-031(024–030 已被其它线占用)", got)
	}
}

// 20 个 goroutine 并发占号:不能重号,序号必须连续单调,next 必须收在 051。
func TestLocalIssueConcurrentNoDuplicates(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	const n = 20

	var wg sync.WaitGroup
	codes := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], errs[i] = l.Issue(context.Background(), Request{Line: "C", ShortName: "并发题"})
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个 Issue 失败: %v", i, err)
		}
		if seen[codes[i]] {
			t.Fatalf("重号: %s", codes[i])
		}
		seen[codes[i]] = true
	}

	got := make([]string, 0, n)
	for code := range seen {
		got = append(got, code)
	}
	sort.Strings(got)
	for i, code := range got {
		if want := formatCode("C", 31+i); code != want {
			t.Fatalf("序号不连续:第 %d 个是 %s，想要 %s", i, code, want)
		}
	}

	reg := readRegistry(t, l)
	if reg.Next != 51 {
		t.Fatalf("next = %d，想要 51", reg.Next)
	}
	if len(reg.Codes) != 27+n {
		t.Fatalf("登记条数 = %d，想要 %d", len(reg.Codes), 27+n)
	}
	// 存量 27 条一条不能少,空洞也不能被谁顺手补上。
	if _, ok := reg.Codes["P26-001"]; !ok {
		t.Fatal("存量登记 P26-001 丢了")
	}
	for _, hole := range []string{"021", "022", "023"} {
		for code := range reg.Codes {
			if len(code) > 3 && code[len(code)-3:] == hole {
				t.Fatalf("空洞 %s 被回填成了 %s", hole, code)
			}
		}
	}
}

// ExpectedCode 对得上就放行,并且真的写进了登记簿。
func TestLocalIssueHonoursMatchingExpectedCode(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	preview, err := l.Preview(context.Background(), "B")
	if err != nil {
		t.Fatalf("Preview 失败: %v", err)
	}
	code, err := l.Issue(context.Background(), Request{Line: "B", ShortName: "回验题", ExpectedCode: preview})
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	if code != preview {
		t.Fatalf("Issue = %s，与预告 %s 不一致", code, preview)
	}
	reg := readRegistry(t, l)
	entry, ok := reg.Codes[code]
	if !ok {
		t.Fatalf("登记簿里没有 %s", code)
	}
	if entry.ShortName != "回验题" || entry.Line != "B" || entry.Student != "" {
		t.Fatalf("登记内容不对: %+v", entry)
	}
}

// 预告与占号之间号被别人拿走 → ErrCodeMoved,且一个字都不许写。
func TestLocalIssueRejectsMovedCode(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	preview, err := l.Preview(context.Background(), "C")
	if err != nil {
		t.Fatalf("Preview 失败: %v", err)
	}
	// 模拟"别人先占了这个号"。
	if _, err := l.Issue(context.Background(), Request{Line: "C", ShortName: "抢先题"}); err != nil {
		t.Fatalf("抢先占号失败: %v", err)
	}
	before := readRegistry(t, l)

	_, err = l.Issue(context.Background(), Request{Line: "C", ShortName: "迟到题", ExpectedCode: preview})
	if !errors.Is(err, ErrCodeMoved) {
		t.Fatalf("想要 ErrCodeMoved，得到 %v", err)
	}
	after := readRegistry(t, l)
	if after.Next != before.Next || len(after.Codes) != len(before.Codes) {
		t.Fatalf("被拒的 Issue 改了登记簿: %d/%d → %d/%d",
			before.Next, len(before.Codes), after.Next, len(after.Codes))
	}
}

// 登记簿不存在 / 损坏一律 fail-closed —— 绝不能凭空造一本新簿子从 001 重发。
func TestLocalFailsClosedOnMissingOrBrokenRegistry(t *testing.T) {
	missing := NewLocal(t.TempDir())
	if _, err := missing.Preview(context.Background(), "C"); !errors.Is(err, ErrNoRegistry) {
		t.Fatalf("想要 ErrNoRegistry，得到 %v", err)
	}
	if _, err := missing.Issue(context.Background(), Request{Line: "C", ShortName: "题"}); !errors.Is(err, ErrNoRegistry) {
		t.Fatalf("想要 ErrNoRegistry，得到 %v", err)
	}

	for name, body := range map[string]string{
		"截断":       `{"next": 31, "codes": {`,
		"codes缺失":  `{"next": 31}`,
		"codes非对象": `{"next": 31, "codes": []}`,
	} {
		broken := newLocalWith(t, body)
		if _, err := broken.Preview(context.Background(), "C"); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("%s:想要 ErrRegistryBroken，得到 %v", name, err)
		}
	}
}

// 登记簿是不可再生资产:未知字段与历史号「没有 line 键」的形态必须原样保住。
func TestLocalPreservesUnknownFieldsAndLegacyShape(t *testing.T) {
	l := newLocalWith(t, `{"next": 31, "codes": {
		"P26-001": {"short_name": "吸尘黑板擦", "student": "学生002"},
		"P26C-030": {"short_name": "带新字段", "student": "谁", "line": "C", "future_field": {"a": 1}}
	}}`)
	if _, err := l.Issue(context.Background(), Request{Line: "G", ShortName: "新题"}); err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}

	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var probe struct {
		Codes map[string]map[string]json.RawMessage `json:"codes"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, ok := probe.Codes["P26C-030"]["future_field"]; !ok {
		t.Fatal("未知字段 future_field 被写丢了")
	}
	if _, ok := probe.Codes["P26-001"]["line"]; ok {
		t.Fatal("历史号 P26-001 被补上了 line 键，破坏了原格式")
	}
}

// 入参体检与 NAS 侧同口径。
func TestLocalRejectsBadInput(t *testing.T) {
	l := newLocalWith(t, d09Registry)
	if _, err := l.Preview(context.Background(), "X"); !errors.Is(err, ErrBadLine) {
		t.Fatalf("想要 ErrBadLine，得到 %v", err)
	}
	if _, err := l.Issue(context.Background(), Request{Line: "C", ShortName: "这个短名超过了八个字"}); !errors.Is(err, ErrBadShortName) {
		t.Fatalf("想要 ErrBadShortName，得到 %v", err)
	}
	if _, err := l.Issue(context.Background(), Request{Line: "C", ShortName: "题", ExpectedCode: "P26-031"}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("宽口径历史号不该被当作合法的 expected_code，得到 %v", err)
	}
}

func TestCodePatterns(t *testing.T) {
	for _, code := range []string{"P26C-031", "P26B-001", "P26G-999"} {
		if !ValidCode(code) || !ValidAnyCode(code) {
			t.Fatalf("%s 应当两个口径都合法", code)
		}
	}
	// 历史号:宽口径认,严口径不认。
	if ValidCode("P26-018") || !ValidAnyCode("P26-018") {
		t.Fatal("P26-018 应当只被宽口径接受")
	}
	for _, bad := range []string{"P25C-001", "P26D-001", "P26C-1", "P26C-0001", " P26C-001"} {
		if ValidAnyCode(bad) {
			t.Fatalf("%q 不该被接受", bad)
		}
	}
	if seq, ok := SequenceOf("P26B-030"); !ok || seq != 30 {
		t.Fatalf("SequenceOf = %d,%v，想要 30,true", seq, ok)
	}
}

func writeRegistry(t *testing.T, l *Local, reg Registry) {
	t.Helper()
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(l.Path(), body, 0o644); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
}
