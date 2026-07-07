package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(editFile{}) }

// editFile replaces an exact string in a file. roots confines the target to the
// workspace when non-empty (see writeFile); workDir, when non-empty, is the
// directory a relative path resolves against (see resolveIn).
type editFile struct {
	roots   []string
	workDir string
}

func (editFile) Name() string { return "edit_file" }

func (editFile) Description() string {
	return "Replace an exact string in a file with another. old_string must occur exactly once; add surrounding context to disambiguate. Use for targeted edits instead of rewriting the whole file."
}

func (editFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"old_string":{"type":"string","description":"Exact text to replace (must be unique in the file)"},"new_string":{"type":"string","description":"Replacement text (may be empty to delete)"}},"required":["path","old_string","new_string"]}`)
}

func (editFile) ReadOnly() bool { return false }

func (e editFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path 必填")
	}
	if p.OldString == "" {
		return "", fmt.Errorf("old_string 必填")
	}
	p.Path = resolveIn(e.workDir, p.Path)
	if err := confine(e.roots, p.Path); err != nil {
		return "", err
	}

	content, enc, err := readFileEncoded(p.Path)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败:%w", p.Path, err)
	}

	start, end, note, ferr := findUniqueMatch(content, p.OldString)
	switch {
	case errors.Is(ferr, errEditNoMatch):
		return "", fmt.Errorf("在 %s 中找不到 old_string——文件当前内容可能与你记忆的不一致,请先用 read_file 重新读取该区域,再逐字复制要替换的原文(行尾空白/缩进差异已自动容错,仍未命中说明内容确实不同);不要改用 write_file 整体覆盖", p.Path)
	case errors.Is(ferr, errEditNotUnique):
		return "", fmt.Errorf("old_string 在 %s 中出现多次,无法确定改哪一处——请在 old_string 中多带几行上下文使其唯一", p.Path)
	case ferr != nil:
		return "", ferr
	}

	updated := content[:start] + p.NewString + content[end:]
	if err := writeFileEncoded(p.Path, updated, enc); err != nil {
		return "", fmt.Errorf("写入 %s 失败:%w", p.Path, err)
	}
	if note != "" {
		return fmt.Sprintf("edited %s (%s)", p.Path, note), nil
	}
	return fmt.Sprintf("edited %s", p.Path), nil
}
