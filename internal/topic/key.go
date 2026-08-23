// Package topic 是科创工作流的题库扫描器：把 `课题/` 目录树读成题卡索引。
//
// 铁律「文件位置就是状态」（注册表 enums.topic_states）：题卡不是文件，是扫描产物。
// 本包**只读**——不写盘、不改名、不落缓存；调用方拿到的 Index 是一次扫描的快照。
//
// 边界：本包属领域层，只依赖标准库 + golang.org/x/text（NFKC）+ internal/workflow/registry
// （枚举唯一真源）。不 import control / agent / engine / project。
package topic

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Key 这个函数做什么：把题名归一化成跨来源合并用的 key。
//
// 口径与历史三份实现（topic_index.py:25-31 / web/main.py:893-899 /
// conversation-threads.ts:74-79）必须逐字节一致，否则历史题卡会被当成新题：
//
//	NFKC 归一 → 转小写 → 丢弃空白字符 → 丢弃 Unicode 大类为 P（标点）或 S（符号）的字符
//
// 三条易错口径：① 必须是 NFKC（不是 NFC/NFD），全角/罗马数字/半角片假名都会被折叠；
// ② Symbol 类（S）也要丢，所以 emoji、`+`、`~`、`♻` 全没了；③ **不折简繁**，
// 「會追太陽」与「会追太阳」是两个 key。
func Key(title string) string {
	normalized := strings.ToLower(norm.NFKC.String(title))
	var b strings.Builder
	b.Grow(len(normalized))
	for _, r := range normalized {
		if isSpace(r) {
			continue
		}
		if unicode.Is(unicode.P, r) || unicode.Is(unicode.S, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isSpace 这个函数做什么：对齐 Python `str.isspace()` 的空白判定。
//
// Go 的 unicode.IsSpace 覆盖了 White_Space 属性，但 Python 还额外把
// U+001C–U+001F（文件/组/记录/单元分隔符）算作空白；这几个字符属 Cc 类，
// 不会被 P/S 过滤掉，所以必须在这里显式补上，否则口径会和历史 key 分叉。
func isSpace(r rune) bool {
	if r >= 0x1C && r <= 0x1F {
		return true
	}
	return unicode.IsSpace(r)
}
