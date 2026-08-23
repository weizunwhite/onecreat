package project

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const baselineDir = "testdata/project_json_baseline"

// baselineFiles 列出 8 份格式基线夹具。
func baselineFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(baselineDir)
	if err != nil {
		t.Fatalf("读夹具目录: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, e.Name())
		}
	}
	if len(out) != 8 {
		t.Fatalf("夹具应有 8 份，实际 %d 份", len(out))
	}
	return out
}

// stageProject 把一份夹具复制成临时目录里的 project.json，返回目录。
func stageProject(t *testing.T, fixture string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(baselineDir, fixture))
	if err != nil {
		t.Fatalf("读夹具 %s: %v", fixture, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), src, 0o644); err != nil {
		t.Fatalf("写临时 project.json: %v", err)
	}
	return dir
}

// TestBaselineRoundTripsByteForByte 是黄金测试：8 份夹具逐一 Load → Save，
// 结果必须与原文件**逐字节**相同。键序、2 空格缩进、中文明文、尾换行
// （P26C-028 没有尾换行）任何一处走样都会红。
func TestBaselineRoundTripsByteForByte(t *testing.T) {
	for _, name := range baselineFiles(t) {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(baselineDir, name))
			if err != nil {
				t.Fatal(err)
			}
			dir := stageProject(t, name)
			p, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := p.Save(); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, FileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("round-trip 不是逐字节相同\n--- want ---\n%q\n--- got ---\n%q", want, got)
			}
		})
	}
}

// TestBaselineTrailingNewlineIsPreserved 单独把尾换行这一位钉死：
// 26/27 有尾换行，P26C-028 是唯一没有的那份，写回时不许顺手补上。
func TestBaselineTrailingNewlineIsPreserved(t *testing.T) {
	cases := map[string]bool{
		"P26-003.json":  true,
		"P26C-028.json": false,
	}
	for name, wantNL := range cases {
		dir := stageProject(t, name)
		p, err := Load(dir)
		if err != nil {
			t.Fatalf("%s Load: %v", name, err)
		}
		if err := p.Save(); err != nil {
			t.Fatalf("%s Save: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		if gotNL := bytes.HasSuffix(got, []byte("\n")); gotNL != wantNL {
			t.Errorf("%s 尾换行 = %v，期望 %v", name, gotNL, wantNL)
		}
	}
}

// TestSetStagePreservesKeyOrderAndAppends 验证保序写回的两半：
// stage 原位更新（不动键序），两个新字段 append 到文件末尾。
func TestSetStagePreservesKeyOrderAndAppends(t *testing.T) {
	// P26B-030 是 O4 键序（… line, assignee, needs_outsourcing, stage），
	// 拿它验证"stage 不在末尾时也原位更新"。
	dir := stageProject(t, "P26B-030.json")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := p.Stage()
	if before != 3 {
		t.Fatalf("夹具 stage 应为 3，实际 %d", before)
	}
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	// 同值改写：只加两个新字段，stage 本身不变。
	if err := p.SetStage(3, SourceKanbanMigration, now); err != nil {
		t.Fatalf("SetStage: %v", err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "code": "P26B-030",
  "short_name": "追光清洁板",
  "full_name": "会追太阳的自清洁太阳能板",
  "student": "机构A",
  "line": "B",
  "assignee": {
    "type": "org",
    "id": "card_test_03",
    "name": "机构A"
  },
  "needs_outsourcing": false,
  "stage": 3,
  "stage_updated_at": "2026-08-23T10:00:00+08:00",
  "stage_source": "kanban-migration"
}
`
	if string(got) != want {
		t.Errorf("写回结果不符\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}

	// 再 Load 一次做语义校验。
	p2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := p2.Stage(); !ok || s != 3 {
		t.Errorf("Stage() = %d, %v，期望 3, true", s, ok)
	}
	if got := p2.StageUpdatedAt(); got != "2026-08-23T10:00:00+08:00" {
		t.Errorf("StageUpdatedAt() = %q", got)
	}
	if got := p2.StageSource(); got != SourceKanbanMigration {
		t.Errorf("StageSource() = %q", got)
	}
	// 新字段不许影响既有字段。
	if p2.Code() != "P26B-030" || p2.Line() != "B" || p2.ShortName() != "追光清洁板" {
		t.Errorf("既有字段被改动了: %+v", p2)
	}
	a, present := p2.Assignee()
	if !present || a == nil || a.Type != "org" || a.ID != "card_test_03" || a.Name != "机构A" {
		t.Errorf("assignee 走样: present=%v a=%+v", present, a)
	}
}

// TestSetStageOnAllBaselines 对 8 份夹具逐一跑 SetStage → Save → Load 的语义回环。
func TestSetStageOnAllBaselines(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for _, name := range baselineFiles(t) {
		t.Run(name, func(t *testing.T) {
			dir := stageProject(t, name)
			p, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			old, ok := p.Stage()
			if !ok {
				t.Fatalf("夹具 %s 的 stage 读不出来", name)
			}
			code, full, student := p.Code(), p.FullName(), p.Student()
			wantAssignee, wantPresent := p.Assignee()

			if err := p.SetStage(5, SourceOneCreat, now); err != nil {
				t.Fatalf("SetStage: %v", err)
			}
			if err := p.Save(); err != nil {
				t.Fatal(err)
			}
			p2, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if s, ok := p2.Stage(); !ok || s != 5 {
				t.Errorf("stage = %d,%v，期望 5,true（原值 %d）", s, ok, old)
			}
			if p2.StageSource() != SourceOneCreat || p2.StageUpdatedAt() != "2026-08-23T10:00:00+08:00" {
				t.Errorf("新字段没落对: source=%q at=%q", p2.StageSource(), p2.StageUpdatedAt())
			}
			if p2.Code() != code || p2.FullName() != full || p2.Student() != student {
				t.Errorf("既有字段走样")
			}
			gotAssignee, gotPresent := p2.Assignee()
			if gotPresent != wantPresent {
				t.Errorf("assignee 三态走样: present %v → %v", wantPresent, gotPresent)
			}
			if (gotAssignee == nil) != (wantAssignee == nil) {
				t.Errorf("assignee 对象/null 走样: %+v → %+v", wantAssignee, gotAssignee)
			}
		})
	}
}

// TestAssigneeThreeStates 钉死三态：键缺失 / null / 对象。
func TestAssigneeThreeStates(t *testing.T) {
	cases := []struct {
		fixture     string
		wantPresent bool
		wantObj     bool
	}{
		{"P26-010.json", false, false}, // 键整个不存在
		{"P26B-026.json", true, false}, // 键存在但是 null
		{"P26C-020.json", true, true},  // 键存在且是对象
	}
	for _, c := range cases {
		dir := stageProject(t, c.fixture)
		p, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		a, present := p.Assignee()
		if present != c.wantPresent || (a != nil) != c.wantObj {
			t.Errorf("%s: Assignee() = %+v, %v；期望 obj=%v present=%v",
				c.fixture, a, present, c.wantObj, c.wantPresent)
		}
	}
}

// TestStageValidation 钉死 stage 的口径：1–8 的数学整数，3.9 判非法不截断。
func TestStageValidation(t *testing.T) {
	cases := []struct {
		literal string
		want    int
		ok      bool
	}{
		{`3`, 3, true},
		{`8`, 8, true},
		{`1`, 1, true},
		{`3.0`, 3, true},   // 数学整数
		{`3.9`, 0, false},  // 关键用例：判非法，绝不截断成 3
		{`0`, 0, false},    // 越界
		{`9`, 0, false},    // 越界
		{`"3"`, 0, false},  // 字符串不是 int
		{`true`, 0, false}, // bool 不是 int
		{`null`, 0, false},
	}
	for _, c := range cases {
		doc, err := ParseDoc([]byte(`{"stage": ` + c.literal + `}`))
		if err != nil {
			t.Fatalf("解析 stage=%s: %v", c.literal, err)
		}
		p := &Project{doc: doc}
		got, ok := p.Stage()
		if got != c.want || ok != c.ok {
			t.Errorf("stage=%s → %d,%v；期望 %d,%v", c.literal, got, ok, c.want, c.ok)
		}
	}
}

// TestSetStageRejectsBadInput 验证 SetStage 的 fail-closed。
func TestSetStageRejectsBadInput(t *testing.T) {
	doc, err := ParseDoc([]byte("{\n  \"stage\": 3\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Project{doc: doc}
	now := time.Now()
	if err := p.SetStage(0, SourceOneCreat, now); err == nil {
		t.Error("stage=0 应当被拒")
	}
	if err := p.SetStage(9, SourceOneCreat, now); err == nil {
		t.Error("stage=9 应当被拒")
	}
	if err := p.SetStage(3, "随手写的", now); err == nil {
		t.Error("非法 stage_source 应当被拒")
	}
}

// TestMissingStageMetaDefaults 验证 §3.2 约束 3 的读取约定：
// stage_updated_at 缺失视为未知（空串），stage_source 缺失视为 sync-init。
func TestMissingStageMetaDefaults(t *testing.T) {
	dir := stageProject(t, "P26-010.json")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.StageUpdatedAt(); got != "" {
		t.Errorf("StageUpdatedAt() = %q，期望空串", got)
	}
	if got := p.StageSource(); got != SourceSyncInit {
		t.Errorf("StageSource() = %q，期望 %q", got, SourceSyncInit)
	}
}

// TestSerializationStyleMatchesPython 把序列化风格的几个细节钉住：
// 中文明文（不是 \uXXXX）、`<`/`>`/`&` 不转义、嵌套 2 空格递增、空对象写成 {}。
func TestSerializationStyleMatchesPython(t *testing.T) {
	src := "{\n  \"a\": \"中文 <b> & 引号\\\"\",\n  \"n\": [\n    1,\n    2\n  ],\n  \"empty\": {},\n  \"deep\": {\n    \"x\": {\n      \"y\": true\n    }\n  }\n}\n"
	doc, err := ParseDoc([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(doc.Bytes()); got != src {
		t.Errorf("风格走样\n--- want ---\n%s\n--- got ---\n%s", src, got)
	}
}
