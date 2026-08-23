package project

import (
	"os"
	"path/filepath"
	"testing"
)

// mkProject 在 root 下建一个项目目录，body 为空则不写 project.json。
func mkProject(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	ok := func(code string) string {
		return "{\n  \"code\": \"" + code + "\",\n  \"short_name\": \"短名\",\n  \"stage\": 3\n}\n"
	}
	mkProject(t, root, "P26C-020_飞鸟志", ok("P26C-020"))
	mkProject(t, root, "P26-003_双电梯调度", ok("P26-003"))   // 存量无字母编号
	mkProject(t, root, "P26B-030_追光清洁板", ok("P26B-030")) // B 线
	mkProject(t, root, "P26G-041_学校线", ok("P26G-041"))   // G 线（登记簿口径里有，实际暂无）
	mkProject(t, root, "P26C-099_缺档", "")                // 缺 project.json → Skipped
	mkProject(t, root, "P26C-098_坏档", "{ 这不是 JSON")      // 解析失败 → Skipped
	mkProject(t, root, UnestablishedDir, "")             // _未立项/ 忽略
	mkProject(t, root, "张三", ok("P26C-001"))             // 不匹配编号前缀 → 忽略
	if err := os.WriteFile(filepath.Join(root, "说明.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Projects) != 4 {
		var got []string
		for _, p := range res.Projects {
			got = append(got, filepath.Base(p.Dir))
		}
		t.Fatalf("应扫到 4 个项目，实际 %d: %v", len(res.Projects), got)
	}
	if res.Projects[0].Code() != "P26-003" {
		t.Errorf("结果应按目录名排序，第一个是 %s", res.Projects[0].Code())
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("应跳过 2 个目录，实际 %d: %+v", len(res.Skipped), res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" || s.Err == nil {
			t.Errorf("Skip 应带原因与底层错误: %+v", s)
		}
	}
}

func TestScanMissingRootErrors(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "不存在")); err == nil {
		t.Error("root 读不了时 Scan 应返回错误")
	}
}
