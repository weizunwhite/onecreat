// 质量四件套里的两件半（调研 01 §4.2 / `pipeline.ts:148-226`）：
//
//	① 假成功检测   —— 在 job.go 里（要用注册表的落桶与扩展名，跟作业状态机贴着走）
//	② 失败重试 1 次 —— 在 job.go 里
//	③ 看图质检     —— 本文件只留 hook 点（VisionChecker 接口），完整实现按 06 §4 后置
//	④ 图表组装说明 —— 本文件的 AssemblyNote
package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// AssemblyNoteFile 是组装说明的文件名（写在材料目标桶下）。
const AssemblyNoteFile = "图表组装说明.txt"

// VisionChecker 是"AI 看图质检"的接线点：产物里有 PNG 时，把图交给视觉模型看一眼，
// 返回一段**只报告不改**的评语。
//
// 本期（M2）不实现，Deps.Vision 可以为 nil——nil 时作业会在台账里如实记一条
// quality_note 说明跳过原因，而不是假装质检过了。
type VisionChecker interface {
	Check(ctx context.Context, files []string) (string, error)
}

// AssemblyNote 这个函数做什么：判断这批产物需不需要一份人工组装指引，需要就把正文给出来。
//
// 触发条件照 NASApp `pipeline.ts:200-222` 的意图：**有 .docx、也有 .drawio、但一张
// .png 都没有**——说明图还停在 drawio 源文件阶段，没导成能插进文档的位图，机器这一步
// 干不了（导出要开 draw.io），所以留一份指引让人补完，而不是让老师拿到一份缺图的文档还不知道。
//
// 三种情况：缺 drawio 或缺 docx → 不需要；已经有 png → 不需要；docx+drawio 无 png → 需要。
func AssemblyNote(outputs []string) (string, bool) {
	var docx, drawio []string
	hasPNG := false
	for _, p := range outputs {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".docx":
			docx = append(docx, filepath.Base(p))
		case ".drawio":
			drawio = append(drawio, filepath.Base(p))
		case ".png":
			hasPNG = true
		}
	}
	if hasPNG || len(docx) == 0 || len(drawio) == 0 {
		return "", false
	}
	sort.Strings(docx)
	sort.Strings(drawio)

	var b strings.Builder
	b.WriteString("图表组装说明（自动生成，供人工补完）\n")
	b.WriteString("========================================\n\n")
	b.WriteString("本次生成的产物里有 Word 文档、也有 drawio 图源，但没有任何 PNG 图片。\n")
	b.WriteString("也就是说图还停在源文件阶段，没有插进文档——这一步需要人来做。\n\n")
	b.WriteString("涉及的 Word 文档：\n")
	for _, f := range docx {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("\n涉及的 drawio 图源：\n")
	for _, f := range drawio {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("\n请按下面三步补完：\n")
	b.WriteString("  1. 用 draw.io（或 VS Code 的 Draw.io Integration 插件）逐个打开上面的 .drawio 文件；\n")
	b.WriteString("  2. 文件 → 导出为 → PNG，勾选「透明背景」关闭、缩放 2x，导出到与图源同一个目录；\n")
	b.WriteString("  3. 打开对应的 Word 文档，找到写着「（此处插入 XXX 图）」或图题占位的位置，\n")
	b.WriteString("     插入刚导出的 PNG，并核对图题编号与正文引用一致。\n\n")
	b.WriteString("补完后可以删掉本文件。\n")
	return b.String(), true
}
