package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

// fakeProject 造一个临时项目目录：七目录齐全 + project.json + 若干假产物文件。
// files 的键是项目根下的相对路径（用 / 分隔），值是文件内容（内容不参与判定，随便写）。
func fakeProject(t *testing.T, files map[string]string) *project.Project {
	t.Helper()
	root := filepath.Join(t.TempDir(), "P26C-020_飞鸟志")
	for _, b := range project.Buckets {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	meta := `{
  "code": "P26C-020",
  "short_name": "飞鸟志",
  "full_name": "校园鸟类声纹多样性监测站",
  "line": "C",
  "stage": 3
}`
	if err := os.WriteFile(filepath.Join(root, project.FileName), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func statusOf(t *testing.T, files map[string]string) *Status {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := ProjectStatus(reg, fakeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func materialOf(t *testing.T, st *Status, typ string) MaterialStatus {
	t.Helper()
	for _, m := range st.Materials {
		if m.Type == typ {
			return m
		}
	}
	t.Fatalf("材料 %s 不在 Status.Materials 里", typ)
	return MaterialStatus{}
}

// TestProjectStatusBasics 断言基本信息、桶计数与段名。
func TestProjectStatusBasics(t *testing.T) {
	st := statusOf(t, map[string]string{
		"02_平台材料/技术方案_简易版.md": "x",
		"02_平台材料/教案_第1课.md":   "x",
	})
	if st.Code != "P26C-020" || st.ShortName != "飞鸟志" || st.Line != "C" {
		t.Errorf("基本信息不对：%+v", st)
	}
	if !st.StageOK || st.Stage != 3 {
		t.Errorf("stage 应读出 3，实际 %d(ok=%v)", st.Stage, st.StageOK)
	}
	if st.StageName != "生产" {
		t.Errorf("stage 3 的段名应是「生产」，实际 %q", st.StageName)
	}
	if got := st.BucketFiles[project.BucketPlatform]; got != 2 {
		t.Errorf("02_平台材料 文件数应为 2，实际 %d", got)
	}
	if len(st.MissingBuckets) != 0 {
		t.Errorf("七目录都建了，MissingBuckets 应为空，实际 %v", st.MissingBuckets)
	}
}

// TestProjectStatusPresentAndMissing 是判定表的正反例。
func TestProjectStatusPresentAndMissing(t *testing.T) {
	st := statusOf(t, map[string]string{
		// 桶根直落，文件名带材料名后缀。
		"02_平台材料/技术方案_简易版.md": "x",
		// 机制目录 App产物/ 下再嵌一层，必须递归才找得到。
		"01_立项定题/App产物/项目简要说明/项目简要说明.md": "x",
		// 英文脚手架名，靠 materialRules 的关键词表命中。
		"02_平台材料/research_log_scaffold_飞鸟志.json": "x",
		// 课件走 outputs.dir 的子目录 课件/specs。
		"02_平台材料/课件/specs/lesson1.json": "x",
		// 07 桶：答辩 PPT。
		"07_竞赛提交/飞鸟志_答辩.pptx": "x",
		// 扩展名不对（论文注册的是 .docx），必须判缺失。
		"07_竞赛提交/论文.md": "x",
		// 桶不对（材料清单的正本桶是 01_立项定题），必须判缺失。
		"03_研发工作区/docs/材料采购清单_预研版.xlsx": "x",
	})

	present := []string{"技术方案", "项目简要说明", "研究日志框架", "课件", "答辩PPT"}
	for _, typ := range present {
		if m := materialOf(t, st, typ); !m.Present {
			t.Errorf("材料 %s 应判为已有，实际缺失（规则：%s）", typ, m.Rule)
		}
	}
	absent := []string{"论文", "材料清单", "教案", "辅导手册", "图表", "金鹏视频", "匿名化检查"}
	for _, typ := range absent {
		if m := materialOf(t, st, typ); m.Present {
			t.Errorf("材料 %s 应判为缺失，实际命中 %v", typ, m.Paths)
		}
	}

	// 命中路径必须是项目根下的相对路径，且指向真实那份文件。
	m := materialOf(t, st, "项目简要说明")
	if len(m.Paths) != 1 || m.Paths[0] != "01_立项定题/App产物/项目简要说明/项目简要说明.md" {
		t.Errorf("项目简要说明 命中路径不对：%v", m.Paths)
	}
}

// TestProjectStatusUnknown 断言判不了的材料进 Unknown 而不是被当成缺失。
func TestProjectStatusUnknown(t *testing.T) {
	st := statusOf(t, nil)
	want := map[string]bool{"快速产题": true, "深度产题": true, "单题发散": true, "快速评题": true, "深度评题": true}
	got := map[string]bool{}
	for _, typ := range st.Unknown {
		got[typ] = true
	}
	for typ := range want {
		if !got[typ] {
			t.Errorf("材料 %s 的正本不在项目目录里，应进 Unknown，实际没有", typ)
		}
	}
	for typ := range got {
		if !want[typ] {
			t.Errorf("材料 %s 不该进 Unknown", typ)
		}
	}
	// Materials + Unknown 必须刚好覆盖注册表全部材料，一个都不许漏。
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Materials)+len(st.Unknown) != len(reg.Materials) {
		t.Errorf("材料条数 %d + Unknown %d ≠ 注册表 %d",
			len(st.Materials), len(st.Unknown), len(reg.Materials))
	}
}

// TestProjectStatusEvidenceLedger 断言证据台账的探测。
func TestProjectStatusEvidenceLedger(t *testing.T) {
	if st := statusOf(t, nil); st.EvidenceLedger {
		t.Error("空项目不该报有证据台账")
	}
	st := statusOf(t, map[string]string{
		"02_平台材料/飞鸟志_evidence_ledger.json": "{}",
	})
	if !st.EvidenceLedger || len(st.EvidenceLedgerPaths) != 1 {
		t.Errorf("应探测到 1 份证据台账，实际 %v", st.EvidenceLedgerPaths)
	}
}

// TestProjectStatusIgnoresHiddenFiles 断言隐藏文件不算产物、不进计数。
func TestProjectStatusIgnoresHiddenFiles(t *testing.T) {
	st := statusOf(t, map[string]string{
		"02_平台材料/.DS_Store":            "x",
		"02_平台材料/.onecreat/技术方案_影子.md": "x",
	})
	if got := st.BucketFiles[project.BucketPlatform]; got != 0 {
		t.Errorf("隐藏文件不该进计数，实际 %d", got)
	}
	if m := materialOf(t, st, "技术方案"); m.Present {
		t.Errorf("隐藏目录里的文件不该算产物，实际命中 %v", m.Paths)
	}
}

// TestProjectStatusMissingBuckets 断言桶缺失被记下来而不是让整轮失败。
func TestProjectStatusMissingBuckets(t *testing.T) {
	p := fakeProject(t, nil)
	if err := os.Remove(filepath.Join(p.Dir, project.BucketContest)); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := ProjectStatus(reg, p)
	if err != nil {
		t.Fatalf("缺一个桶不该让判定失败：%v", err)
	}
	if len(st.MissingBuckets) != 1 || st.MissingBuckets[0] != project.BucketContest {
		t.Errorf("MissingBuckets 应只有 07_竞赛提交，实际 %v", st.MissingBuckets)
	}
}

// TestStageName 覆盖八段与越界。
func TestStageName(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[int]string{1: "孵化", 2: "立项", 3: "生产", 8: "回流归档", 0: "", 9: ""}
	for no, want := range cases {
		if got := StageName(reg, no); got != want {
			t.Errorf("StageName(%d) = %q，应为 %q", no, got, want)
		}
	}
}
