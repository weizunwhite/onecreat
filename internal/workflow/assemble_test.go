package workflow

import (
	"errors"
	"strings"
	"testing"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

func loadReg(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestAssemblePromptSections：段落齐全且顺序固定。
func TestAssemblePromptSections(t *testing.T) {
	reg := loadReg(t)
	p := fakeProject(t, nil)
	got, err := AssemblePrompt(reg, p, "教案", "【lesson-plan-generator 技能正文】",
		map[string][]byte{"技术方案": []byte("主控用 ESP32。")}, "偏重低年级")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"使用 lesson-plan-generator 技能",
		"## 项目信息",
		"P26C-020",
		"校园鸟类声纹多样性监测站",
		"## 本项目已有的「技术方案」（完整内容，请以它为主要依据）",
		"主控用 ESP32。",
		"技能正文",
		"## 运行环境与产物落点",
		"## 本次生成的额外要求",
		"偏重低年级",
		"不要问我任何问题",
	}
	at := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("prompt 缺少 %q：\n%s", want, got)
		}
		if i < at {
			t.Errorf("段落顺序不对，%q 出现得太早：\n%s", want, got)
		}
		at = i
	}
}

// TestAssemblePromptEnvironmentNote：七目录说明要逐字列出七个桶、点名目标桶与扩展名。
func TestAssemblePromptEnvironmentNote(t *testing.T) {
	got, err := AssemblePrompt(loadReg(t), fakeProject(t, nil), "课件", "正文", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range project.Buckets {
		if !strings.Contains(got, b) {
			t.Errorf("环境说明缺桶名 %s", b)
		}
	}
	// 课件的 outputs.dir 是 02_平台材料/课件/specs，桶是 02_平台材料。
	if !strings.Contains(got, "必须**写进 `02_平台材料/`") {
		t.Errorf("没点名目标桶：\n%s", got)
	}
	if !strings.Contains(got, ".json") {
		t.Errorf("没点名扩展名：\n%s", got)
	}
	if !strings.Contains(got, "真的落盘文件") {
		t.Errorf("没强调交付物必须落盘：\n%s", got)
	}
}

// TestAssemblePromptOmitsEmptySections：没有依赖、没有 note、没有 skill 正文时，
// 对应段落整段不出现（空标题会让模型以为有东西没给它）。
func TestAssemblePromptOmitsEmptySections(t *testing.T) {
	got, err := AssemblePrompt(loadReg(t), fakeProject(t, nil), "技术方案", "",
		map[string][]byte{"立项": []byte("   \n")}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"本项目已有的", "额外要求", "技能正文"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("空内容不该留下 %q 段：\n%s", unwanted, got)
		}
	}
}

// TestAssemblePromptDepOrderIsStable：依赖注入顺序固定为
// 硬依赖（注册表序）→ 软依赖（注册表序）→ 其余（字典序）。
func TestAssemblePromptDepOrderIsStable(t *testing.T) {
	// 论文：deps=[论文骨架]、soft_deps=[研究日志框架, 原始资料]。
	got, err := AssemblePrompt(loadReg(t), fakeProject(t, nil), "论文", "",
		map[string][]byte{
			"原始资料":   []byte("原始资料正文"),
			"论文骨架":   []byte("论文骨架正文"),
			"技术方案":   []byte("技术方案正文"), // 既不是硬依赖也不是软依赖 → 排最后
			"研究日志框架": []byte("研究日志框架正文"),
		}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"论文骨架正文", "研究日志框架正文", "原始资料正文", "技术方案正文"}
	at := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Fatalf("prompt 缺少 %q", w)
		}
		if i < at {
			t.Fatalf("依赖顺序不对，%q 排早了：\n%s", w, got)
		}
		at = i
	}
}

// TestAssemblePromptAcceptsAliasKeys：deps 的键写成旧别名也要能注入。
func TestAssemblePromptAcceptsAliasKeys(t *testing.T) {
	got, err := AssemblePrompt(loadReg(t), fakeProject(t, nil), "论文", "",
		map[string][]byte{"研究日志": []byte("旧名键的正文")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "旧名键的正文") {
		t.Errorf("别名键的依赖没注入：\n%s", got)
	}
}

// TestAssemblePromptRefusesUnknownAndConversationalOutputs：
// 未注册材料、以及产物不落七目录的材料，装配阶段就拒绝。
func TestAssemblePromptRefusesUnknownAndConversationalOutputs(t *testing.T) {
	reg := loadReg(t)
	p := fakeProject(t, nil)
	if _, err := AssemblePrompt(reg, p, "毕业论文", "", nil, ""); !errors.Is(err, project.ErrUnknownMaterial) {
		t.Errorf("未注册材料应拒绝，实际 %v", err)
	}
	// 深度产题的 outputs.dir 是题库位置 00_产题评题/产题，不在项目七目录里。
	if _, err := AssemblePrompt(reg, p, "深度产题", "", nil, ""); err == nil {
		t.Errorf("产物不落七目录的材料应拒绝")
	}
	if _, err := AssemblePrompt(nil, p, "教案", "", nil, ""); err == nil {
		t.Errorf("注册表为空应拒绝")
	}
	if _, err := AssemblePrompt(reg, nil, "教案", "", nil, ""); err == nil {
		t.Errorf("项目为空应拒绝")
	}
}
