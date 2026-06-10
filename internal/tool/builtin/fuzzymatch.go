package builtin

import (
	"errors"
	"strings"
)

// 弱模型(如 deepseek-v4-flash)复述代码时常带行尾空格或缩进差异,逐字节精确
// 匹配会让 edit_file 反复报 not found、改文件失败。借鉴 Codex apply-patch 的
// seek_sequence(codex-rs/apply-patch/src/lib.rs):精确失败后按行降级匹配,
// 每级都必须唯一才命中;命中模糊级时替换的是文件「原文窗口」,并在结果消息里
// 如实标注用了哪级,保证 transcript 可审计。
var (
	errEditNoMatch   = errors.New("old_string not found")
	errEditNotUnique = errors.New("old_string is not unique")
)

// findUniqueMatch 在 content 里定位 old 的唯一一次出现,返回字节区间 [start,end)
// 与模糊级说明 note(精确命中时为空)。匹配级别:
//
//	0 级:精确子串(行为与旧实现一致);
//	1 级:按行比较,忽略行尾空白;
//	2 级:按行比较,忽略首尾空白(缩进差异);要求 old 至少有一行非空,防误配。
func findUniqueMatch(content, old string) (start, end int, note string, err error) {
	// 0 级:精确
	if i := strings.Index(content, old); i >= 0 {
		if strings.Index(content[i+1:], old) >= 0 {
			return 0, 0, "", errEditNotUnique
		}
		return i, i + len(old), "", nil
	}

	// old 末尾的换行单独记下:窗口按"整行"匹配,结尾换行在算区间时补上。
	trailingNL := strings.HasSuffix(old, "\n")
	oldLines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")

	lines := strings.Split(content, "\n")
	if len(oldLines) > len(lines) {
		return 0, 0, "", errEditNoMatch
	}
	// 每行在 content 里的字节起点(按 "\n" 切分可精确还原)。
	starts := make([]int, len(lines))
	off := 0
	for i, l := range lines {
		starts[i] = off
		off += len(l) + 1 // +1 是切掉的那个 "\n"
	}

	type level struct {
		norm func(string) string
		note string
	}
	levels := []level{
		{func(s string) string { return strings.TrimRight(s, " \t") }, "fuzzy match: ignored trailing whitespace"},
		{strings.TrimSpace, "fuzzy match: ignored indentation"},
	}
	for li, lv := range levels {
		// 2 级(忽略缩进)若 old 全是空白行,任何空白区域都会命中,直接跳过。
		if li == 1 {
			allBlank := true
			for _, ol := range oldLines {
				if strings.TrimSpace(ol) != "" {
					allBlank = false
					break
				}
			}
			if allBlank {
				continue
			}
		}
		normOld := make([]string, len(oldLines))
		for i, ol := range oldLines {
			normOld[i] = lv.norm(ol)
		}
		hit, count := -1, 0
		for i := 0; i+len(oldLines) <= len(lines); i++ {
			match := true
			for j := range normOld {
				if lv.norm(lines[i+j]) != normOld[j] {
					match = false
					break
				}
			}
			if match {
				count++
				if hit < 0 {
					hit = i
				}
				if count > 1 {
					break
				}
			}
		}
		if count > 1 {
			return 0, 0, "", errEditNotUnique
		}
		if count == 1 {
			lastLine := hit + len(oldLines) - 1
			s := starts[hit]
			e := starts[lastLine] + len(lines[lastLine])
			if trailingNL && e < len(content) {
				e++ // old 以换行结尾:把窗口末尾的 "\n" 一并算进替换区间
			}
			return s, e, lv.note, nil
		}
	}
	return 0, 0, "", errEditNoMatch
}
