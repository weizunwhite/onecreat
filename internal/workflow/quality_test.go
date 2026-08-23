package workflow

import (
	"strings"
	"testing"
)

// TestAssemblyNoteThreeStates：组装说明的三态。
//
//	① docx + drawio 无 png → 需要（图还没导出来，得人来补）
//	② docx + drawio + png → 不需要（图已经有了）
//	③ 缺 docx 或缺 drawio  → 不需要
func TestAssemblyNoteThreeStates(t *testing.T) {
	cases := []struct {
		name    string
		outputs []string
		want    bool
	}{
		{"docx+drawio 无 png", []string{
			"07_竞赛提交/论文_飞鸟志.docx", "03_研发工作区/系统架构图.drawio"}, true},
		{"docx+drawio+png", []string{
			"07_竞赛提交/论文_飞鸟志.docx", "03_研发工作区/系统架构图.drawio",
			"03_研发工作区/系统架构图.png"}, false},
		{"只有 docx", []string{"07_竞赛提交/论文_飞鸟志.docx"}, false},
		{"只有 drawio", []string{"03_研发工作区/系统架构图.drawio"}, false},
		{"什么都没有", nil, false},
		{"大写扩展名也认", []string{"07_竞赛提交/论文.DOCX", "03_研发工作区/图.DRAWIO"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, ok := AssemblyNote(c.outputs)
			if ok != c.want {
				t.Fatalf("需不需要组装说明判错了：%v != %v", ok, c.want)
			}
			if !ok {
				if body != "" {
					t.Errorf("不需要时不该给正文：%q", body)
				}
				return
			}
			if !strings.Contains(body, "draw.io") || !strings.Contains(body, "PNG") {
				t.Errorf("正文要写清怎么导出：\n%s", body)
			}
		})
	}
}

// TestAssemblyNoteListsFileNames：正文要逐个列出 docx 与 drawio 的文件名，
// 让老师照着做，而不是"自己去项目里找找"。
func TestAssemblyNoteListsFileNames(t *testing.T) {
	body, ok := AssemblyNote([]string{
		"07_竞赛提交/论文_飞鸟志.docx",
		"07_竞赛提交/研究方案_飞鸟志.docx",
		"03_研发工作区/系统架构图.drawio",
	})
	if !ok {
		t.Fatal("这组产物应该需要组装说明")
	}
	for _, want := range []string{"论文_飞鸟志.docx", "研究方案_飞鸟志.docx", "系统架构图.drawio"} {
		if !strings.Contains(body, want) {
			t.Errorf("正文缺少 %q：\n%s", want, body)
		}
	}
	// 列的是文件名不是全路径——说明是给人看的，路径前缀只会碍事。
	if strings.Contains(body, "07_竞赛提交/论文_飞鸟志.docx") {
		t.Errorf("应只列文件名：\n%s", body)
	}
}
