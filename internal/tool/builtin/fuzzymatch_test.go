package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 弱模型复述代码常带空白差异;这些测试钉死 edit_file 的三级匹配行为:
// 精确优先、行尾空白容错、缩进容错、模糊级也必须唯一、替换写回的是文件原文窗口。

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "src.py")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// 精确路径回归:行为与旧实现一致,结果消息不带 fuzzy 标注。
func TestEditFileExactStillWorks(t *testing.T) {
	f := writeTemp(t, "a = 1\nb = 2\n")
	out := runTool(t, editFile{}, map[string]any{"path": f, "old_string": "b = 2", "new_string": "b = 3"})
	if strings.Contains(out, "fuzzy") {
		t.Errorf("精确命中不应标注 fuzzy: %q", out)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "a = 1\nb = 3\n" {
		t.Errorf("content = %q", got)
	}
}

// 1 级:文件行尾带空格,模型给的 old_string 没带 → 应命中,且写回保留原文之外的部分。
func TestEditFileToleratesTrailingWhitespace(t *testing.T) {
	f := writeTemp(t, "def f():   \n    return 1\n") // 第一行行尾有空格
	out := runTool(t, editFile{}, map[string]any{
		"path":       f,
		"old_string": "def f():\n    return 1\n", // 模型复述时丢了行尾空格
		"new_string": "def f():\n    return 2\n",
	})
	if !strings.Contains(out, "fuzzy") {
		t.Errorf("应标注 fuzzy 命中: %q", out)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "def f():\n    return 2\n" {
		t.Errorf("content = %q", got)
	}
}

// 2 级:文件用 tab 缩进,模型给的是空格缩进 → 应命中(忽略缩进差异)。
func TestEditFileToleratesIndentation(t *testing.T) {
	f := writeTemp(t, "if x:\n\treturn old\n") // tab 缩进
	out := runTool(t, editFile{}, map[string]any{
		"path":       f,
		"old_string": "if x:\n    return old", // 模型用了 4 空格
		"new_string": "if x:\n    return new",
	})
	if !strings.Contains(out, "fuzzy") {
		t.Errorf("应标注 fuzzy 命中: %q", out)
	}
	got, _ := os.ReadFile(f)
	if !strings.Contains(string(got), "return new") {
		t.Errorf("content = %q", got)
	}
}

// 模糊级同样要求唯一:两处只差空白的候选 → 报 not unique,文件不动。
func TestEditFileFuzzyStillRequiresUnique(t *testing.T) {
	body := "x = 1 \ny\nx = 1\t\ny\n" // 两处 "x = 1\ny"(只差行尾空白)
	f := writeTemp(t, body)
	_, err := editFile{}.Execute(t.Context(), argsJSON(t, map[string]any{
		"path": f, "old_string": "x = 1\ny", "new_string": "x = 2\ny",
	}))
	if err == nil || !strings.Contains(err.Error(), "出现多次") {
		t.Fatalf("应报不唯一,err=%v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != body {
		t.Errorf("失败时文件不应被改动")
	}
}

// 完全不匹配:维持原 not found 口径。
func TestEditFileStillNotFound(t *testing.T) {
	f := writeTemp(t, "hello\n")
	_, err := editFile{}.Execute(t.Context(), argsJSON(t, map[string]any{
		"path": f, "old_string": "world", "new_string": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "找不到 old_string") {
		t.Fatalf("应报找不到,err=%v", err)
	}
}

// 全空白的 old_string 不允许走 2 级(防止命中任何空白区域)。
func TestEditFileBlankOldSkipsIndentLevel(t *testing.T) {
	f := writeTemp(t, "a\n  \nb\n\t\nc\n")
	_, err := editFile{}.Execute(t.Context(), argsJSON(t, map[string]any{
		"path": f, "old_string": "    \n", "new_string": "x\n",
	}))
	// 1 级(忽略行尾空白)把 "    " 归一成 "",会命中两处空白行 → not unique;
	// 关键断言是:绝不能因 2 级误配而"成功"。
	if err == nil {
		t.Fatal("全空白 old_string 不应模糊命中成功")
	}
}

// multi_edit 复用同一容错:一步精确 + 一步空白差异,原子成功并标注 fuzzy 计数。
func TestMultiEditFuzzyStep(t *testing.T) {
	f := writeTemp(t, "alpha = 1   \nbeta = 2\n")
	out := runTool(t, multiEdit{}, map[string]any{
		"path": f,
		"edits": []map[string]any{
			{"old_string": "alpha = 1\n", "new_string": "alpha = 10\n"}, // 行尾空白差异
			{"old_string": "beta = 2", "new_string": "beta = 20"},       // 精确
		},
	})
	if !strings.Contains(out, "fuzzy-matched") {
		t.Errorf("应标注 fuzzy 计数: %q", out)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "alpha = 10\nbeta = 20\n" {
		t.Errorf("content = %q", got)
	}
}
