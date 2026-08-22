package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnowledgeCreateImportSearchAndPrompt(t *testing.T) {
	t.Setenv("ONECREAT_KNOWLEDGE_DIR", t.TempDir())
	app := newBareApp(nil, nil)

	base, err := app.KnowledgeCreate("客户硬件资料")
	if err != nil {
		t.Fatalf("KnowledgeCreate: %v", err)
	}
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "esp32_uart.md")
	body := "# ESP32 UART 调试\n\nESP32 和 Unihiker 之间优先使用 UART 通信，波特率建议 115200。课堂项目需要保留接线表。"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := app.knowledgeImportPaths(base.ID, []string{src})
	if err != nil {
		t.Fatalf("knowledgeImportPaths: %v", err)
	}
	if len(result.Imported) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("import result = %+v, want one imported and no skipped", result)
	}
	if result.Imported[0].Chunks == 0 {
		t.Fatalf("imported chunks = 0")
	}

	view, err := app.KnowledgeView()
	if err != nil {
		t.Fatalf("KnowledgeView: %v", err)
	}
	if view.StoreDir == "" {
		t.Fatalf("StoreDir is empty")
	}
	if _, err := os.Stat(filepath.Join(view.StoreDir, "index.json")); err != nil {
		t.Fatalf("knowledge index missing: %v", err)
	}
	if len(view.Bases) != 1 || view.Bases[0].Documents != 1 {
		t.Fatalf("KnowledgeView bases = %+v, docs = %+v", view.Bases, view.Documents)
	}

	search, err := app.KnowledgeSearch([]string{base.ID}, "ESP32 UART 波特率", 5)
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	if len(search.Matches) == 0 {
		t.Fatalf("KnowledgeSearch returned no matches")
	}
	if !strings.Contains(search.Matches[0].Text, "115200") {
		t.Fatalf("top match = %q, want UART baud context", search.Matches[0].Text)
	}

	prompt, err := app.KnowledgeBuildPrompt([]string{base.ID}, "ESP32 和 Unihiker 怎么通信？", 5)
	if err != nil {
		t.Fatalf("KnowledgeBuildPrompt: %v", err)
	}
	if len(prompt.Sources) == 0 {
		t.Fatalf("KnowledgeBuildPrompt returned no sources")
	}
	if !strings.Contains(prompt.Prompt, "本地知识库片段") || !strings.Contains(prompt.Prompt, "UART") {
		t.Fatalf("prompt = %q, want local knowledge context", prompt.Prompt)
	}
	if !strings.Contains(prompt.Prompt, "用户问题和当前对话上下文优先") {
		t.Fatalf("prompt = %q, want user intent priority guard", prompt.Prompt)
	}
	if strings.Index(prompt.Prompt, "# 用户问题") > strings.Index(prompt.Prompt, "# 本地知识库片段") {
		t.Fatalf("prompt = %q, want user question before knowledge snippets", prompt.Prompt)
	}
}

// 中文分词回归:纯中文、无空格的查询，旧实现会把整句当成一个超长 token
// 几乎匹配不到（必漏召回）；新实现按二字词切分，应能命中。
func TestKnowledgeTokensSplitChinese(t *testing.T) {
	tokens := knowledgeTokens("超声波接线")
	for _, want := range []string{"超声", "声波", "接线"} {
		if !tokensContain(tokens, want) {
			t.Fatalf("tokens %v missing bigram %q", tokens, want)
		}
	}
	// 型号 esp32 应整词保留，不被打散成单字符
	mixed := knowledgeTokens("esp32温度")
	if !tokensContain(mixed, "esp32") || !tokensContain(mixed, "温度") {
		t.Fatalf("mixed tokens = %v, want esp32 + 温度", mixed)
	}
}

func TestKnowledgeScoreMatchesPureChinese(t *testing.T) {
	text := "超声波传感器 HC-SR04 的接线方法：VCC 接 5V，触发脚接 D9。"
	// 纯中文、无空格的查询——旧分词器会整段当一个 token 而漏召回（score 0）
	if score := knowledgeScore("超声波接线", text, "wiring.md"); score <= 0 {
		t.Fatalf("pure-Chinese query scored %v, want > 0", score)
	}
}

func tokensContain(tokens []string, want string) bool {
	for _, tok := range tokens {
		if tok == want {
			return true
		}
	}
	return false
}

func TestKnowledgeImportSkipsUnsupportedFiles(t *testing.T) {
	t.Setenv("ONECREAT_KNOWLEDGE_DIR", t.TempDir())
	app := newBareApp(nil, nil)
	base, err := app.KnowledgeCreate("资料")
	if err != nil {
		t.Fatalf("KnowledgeCreate: %v", err)
	}
	src := filepath.Join(t.TempDir(), "spec.pdf")
	if err := os.WriteFile(src, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := app.knowledgeImportPaths(base.ID, []string{src})
	if err != nil {
		t.Fatalf("knowledgeImportPaths: %v", err)
	}
	if len(result.Imported) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("import result = %+v, want one skipped", result)
	}
	if !strings.Contains(result.Skipped[0].Error, "第一版") {
		t.Fatalf("skip error = %q, want staged support message", result.Skipped[0].Error)
	}
}

func TestKnowledgeDeleteRemovesBaseAndDocs(t *testing.T) {
	t.Setenv("ONECREAT_KNOWLEDGE_DIR", t.TempDir())
	app := newBareApp(nil, nil)
	base, err := app.KnowledgeCreate("待删除")
	if err != nil {
		t.Fatalf("KnowledgeCreate: %v", err)
	}
	src := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(src, []byte("本地知识库删除测试"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.knowledgeImportPaths(base.ID, []string{src}); err != nil {
		t.Fatalf("knowledgeImportPaths: %v", err)
	}
	if err := app.KnowledgeDelete(base.ID); err != nil {
		t.Fatalf("KnowledgeDelete: %v", err)
	}
	view, err := app.KnowledgeView()
	if err != nil {
		t.Fatalf("KnowledgeView: %v", err)
	}
	if len(view.Bases) != 0 || len(view.Documents) != 0 {
		t.Fatalf("after delete view = %+v", view)
	}
}
