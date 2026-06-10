package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"reasonix/internal/diff"
	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(deleteRange{}) }

type deleteRange struct {
	roots   []string
	workDir string
}

func (deleteRange) Name() string { return "delete_range" }

func (deleteRange) Description() string {
	return "Delete a contiguous text range from a file using exact start/end text anchors. Each anchor must match exactly one line. Returns unified diff on success. Use for large deletions — smaller changes should use edit_file."
}

func (deleteRange) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path"},
			"start_anchor":{"type":"string","description":"Exact text of the first line to delete (must be unique in the file)"},
			"end_anchor":{"type":"string","description":"Exact text of the last line to delete (must be unique in the file)"},
			"inclusive":{"type":"boolean","description":"Whether to include the anchor lines in the deletion (default true)"}
		},
		"required":["path","start_anchor","end_anchor"]
	}`)
}

func (deleteRange) ReadOnly() bool { return false }

func (d deleteRange) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	change, err := d.preview(args)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(change.Path, []byte(change.NewText), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", change.Path, err)
	}
	return change.Diff, nil
}

func (d deleteRange) Preview(args json.RawMessage) (diff.Change, error) {
	return d.preview(args)
}

func (d deleteRange) preview(args json.RawMessage) (diff.Change, error) {
	var p struct {
		Path        string `json:"path"`
		StartAnchor string `json:"start_anchor"`
		EndAnchor   string `json:"end_anchor"`
		Inclusive   *bool  `json:"inclusive"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return diff.Change{}, fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return diff.Change{}, fmt.Errorf("path is required")
	}
	if p.StartAnchor == "" {
		return diff.Change{}, fmt.Errorf("start_anchor is required")
	}
	if p.EndAnchor == "" {
		return diff.Change{}, fmt.Errorf("end_anchor is required")
	}

	inclusive := true
	if p.Inclusive != nil {
		inclusive = *p.Inclusive
	}

	p.Path = resolveIn(d.workDir, p.Path)
	if err := confine(d.roots, p.Path); err != nil {
		return diff.Change{}, err
	}

	b, err := os.ReadFile(p.Path)
	if err != nil {
		return diff.Change{}, fmt.Errorf("read %s: %w", p.Path, err)
	}
	original := string(b)

	// 按 \n 切成「行记录」,每条保留自己的行尾(\r\n / \n / 末行可能无行尾)。匹配锚点用
	// 去掉行尾的内容,但拼回用记录原始字节——只删目标区间,其余行逐字节不变,绝不全局删
	// \r、也不统一混合行尾(C2)。
	records := splitLinesKeepEnding(original)
	lines := make([]string, len(records))
	for i, r := range records {
		lines[i] = trimLineEnding(r)
	}
	startLine := findUniqueLine(lines, p.StartAnchor)
	if startLine == -2 {
		return diff.Change{}, fmt.Errorf("start_anchor is not unique in %s; add more surrounding context", p.Path)
	}
	if startLine == -1 {
		return diff.Change{}, fmt.Errorf("start_anchor not found in %s", p.Path)
	}
	endLine := findUniqueLine(lines, p.EndAnchor)
	if endLine == -2 {
		return diff.Change{}, fmt.Errorf("end_anchor is not unique in %s; add more surrounding context", p.Path)
	}
	if endLine == -1 {
		return diff.Change{}, fmt.Errorf("end_anchor not found in %s", p.Path)
	}
	if startLine > endLine {
		return diff.Change{}, fmt.Errorf("start_anchor appears after end_anchor (lines %d and %d)", startLine+1, endLine+1)
	}

	// 拼回保留区间的「原始记录」(含各自行尾),被删区间外每行字节完全不变。
	var keep []string
	if inclusive {
		keep = append(keep, records[:startLine]...)
		keep = append(keep, records[endLine+1:]...)
	} else {
		// Same line for both anchors: the kept prefix and suffix would overlap at
		// that line and duplicate it. There is nothing strictly between a line and
		// itself, so the exclusive deletion is contradictory — reject it.
		if startLine == endLine {
			return diff.Change{}, fmt.Errorf("start_anchor and end_anchor match the same line in %s; with inclusive=false there is nothing between them to delete", p.Path)
		}
		keep = append(keep, records[:startLine+1]...)
		keep = append(keep, records[endLine:]...)
	}
	newContent := strings.Join(keep, "")

	return diff.Build(p.Path, original, newContent, diff.Modify), nil
}

// splitLinesKeepEnding 把内容按 \n 切成行记录,每条保留自己的行尾分隔符(\r\n 或 \n);
// 最后一段若无行尾则不带分隔符。拼接所有记录可逐字节还原原文。
func splitLinesKeepEnding(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			if s != "" {
				out = append(out, s)
			}
			return out
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
}

// trimLineEnding 去掉一条行记录尾部的行尾(\r\n / \n / 尾随 \r),仅用于锚点匹配;行内的
// \r(字符串字面量、旧 Mac 片段)保留不动。
func trimLineEnding(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

// findUniqueLine returns the index of the line that equals target.
// Returns -1 if not found, -2 if found on multiple lines.
func findUniqueLine(lines []string, target string) int {
	idx := -1
	for i, l := range lines {
		if l == target {
			if idx >= 0 {
				return -2
			}
			idx = i
		}
	}
	return idx
}
