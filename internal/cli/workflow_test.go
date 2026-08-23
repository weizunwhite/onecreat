package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/project"
)

// fakeProjects 在临时目录里造几个只有 project.json 的项目，用来测寻址逻辑。
func fakeProjects(t *testing.T, codes map[string]string) []*project.Project {
	t.Helper()
	root := t.TempDir()
	var out []*project.Project
	for code, short := range codes {
		dir := filepath.Join(root, code+"_"+short)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{
  "code": "` + code + `",
  "short_name": "` + short + `",
  "stage": 3
}`
		if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := project.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestFindProject(t *testing.T) {
	ps := fakeProjects(t, map[string]string{
		"P26C-020": "飞鸟志",
		"P26C-019": "三端联调",
		"P26C-031": "安卓联调",
	})

	// 编号寻址（大小写不敏感）与短名精确寻址。
	for _, q := range []string{"P26C-020", "p26c-020", "飞鸟志"} {
		p, err := findProject(ps, q)
		if err != nil {
			t.Fatalf("查 %q 应该找得到：%v", q, err)
		}
		if p.Code() != "P26C-020" {
			t.Errorf("查 %q 找到的是 %s", q, p.Code())
		}
	}

	// 唯一子串匹配：「飞鸟」只对得上一个。
	if p, err := findProject(ps, "飞鸟"); err != nil || p.Code() != "P26C-020" {
		t.Errorf("子串「飞鸟」应唯一命中飞鸟志，实际 %v / %v", p, err)
	}

	// 匹配到多个必须报错并列候选，绝不替人猜一个。
	_, err := findProject(ps, "联调")
	if err == nil {
		t.Fatal("「联调」匹配两个项目，应该报错")
	}
	if !strings.Contains(err.Error(), "P26C-019") || !strings.Contains(err.Error(), "P26C-031") {
		t.Errorf("歧义报错应列出候选，实际：%v", err)
	}

	// 查不到与空串。
	if _, err := findProject(ps, "没有这个题"); err == nil {
		t.Error("查不到应该报错")
	}
	if _, err := findProject(ps, "  "); err == nil {
		t.Error("空查询应该报错")
	}
}
