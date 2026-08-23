package topic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"reasonix/internal/workflow/registry"
)

// FrontMatter 是题 md 的 YAML 头里本包需要的扁平字段（注册表 naming.frontmatter_required）。
// 这里**不引 yaml 库**：题头的形状是命名契约定死的，手写解析器足够，也避免为一个
// 领域包拉一条新依赖。
type FrontMatter struct {
	Kind    string
	Source  string
	Group   string
	Date    string
	Subject string
	// Topics 是题目列表。真实语料里 topics 有两种写法：纯字符串列表，
	// 以及 `- title: xxx` 的映射列表（还挂 brief/score/verify 等）。两种都吃；
	// 映射写法取 title 与 brief（brief 是假题守卫的判据，见 scoringTopicsPlausible），
	// score/verify/collision 本包不需要。
	Topics []TopicEntry
}

// TopicEntry 是 topics 列表里的一条。
type TopicEntry struct {
	Title string
	Brief string
}

// 支持的 frontmatter 行形状（缩进宽度不敏感，只认层级关系）：
//
//	key: value          → 标量
//	key:                → 后跟 `- item` 或 `- subkey: value`
//	  - item
//	  - title: xxx
//	    brief: yyy
var (
	scalarLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):[ \t]*(.*)$`)
	itemLine   = regexp.MustCompile(`^-[ \t]*(.*)$`)
)

// ParseFrontMatter 这个函数做什么：解析一份题 md 的 YAML 头。
//
// 容错 BOM 与 CRLF。没有 frontmatter 返回零值和 nil（不是错误——旧存量文件本来就没有）；
// 只有头开了却没闭合这种真坏输入才返回错误。任何情况下都不 panic。
func ParseFrontMatter(text string) (FrontMatter, error) {
	var fm FrontMatter
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return fm, nil
	}
	body := text[len("---\n"):]
	end := strings.Index(body, "\n---")
	if end < 0 {
		return fm, fmt.Errorf("frontmatter 未闭合")
	}
	lines := strings.Split(body[:end], "\n")

	currentKey := "" // 当前正在收集的列表键
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		indented := len(trimmed) != len(line)

		// 列表项：`- xxx`
		if m := itemLine.FindStringSubmatch(trimmed); m != nil && currentKey != "" {
			value := strings.TrimSpace(m[1])
			if sub := scalarLine.FindStringSubmatch(value); sub != nil {
				// 映射写法的第一行，如 `- title: xxx`。
				if currentKey == "topics" && sub[1] == "title" {
					fm.Topics = append(fm.Topics, TopicEntry{Title: cleanScalar(sub[2])})
				}
				continue
			}
			if currentKey == "topics" && value != "" {
				fm.Topics = append(fm.Topics, TopicEntry{Title: cleanScalar(value)})
			}
			continue
		}

		m := scalarLine.FindStringSubmatch(trimmed)
		if m == nil {
			// 认不出的行（多半是映射项里的多行标量续行）直接跳过，不报错。
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		if indented {
			// 映射列表项的续行。只收 brief——假题守卫要靠它判「下一步行动表」，
			// 其余（score/verify/collision…）本包不需要，忽略。
			if currentKey == "topics" && key == "brief" && len(fm.Topics) > 0 {
				last := &fm.Topics[len(fm.Topics)-1]
				if last.Brief == "" {
					last.Brief = cleanScalar(value)
				}
			}
			continue
		}
		if value == "" {
			currentKey = key
			continue
		}
		currentKey = ""
		switch key {
		case "kind":
			fm.Kind = cleanScalar(value)
		case "source":
			fm.Source = cleanScalar(value)
		case "group":
			fm.Group = cleanScalar(value)
		case "date":
			fm.Date = cleanScalar(value)
		case "subject":
			fm.Subject = cleanScalar(value)
		}
	}
	return fm, nil
}

// cleanScalar 这个函数做什么：剥掉标量外层引号（口径同 topic_index.py:_clean_scalar）。
func cleanScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first == '"' && last == '"' {
		var out string
		if err := json.Unmarshal([]byte(value), &out); err == nil {
			return out
		}
		return value[1 : len(value)-1]
	}
	if first == '\'' && last == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}

// FileName 是题文件名按命名契约 `<简要说明>_<组别>_<YYYYMMDD>.md` 拆出来的三段。
type FileName struct {
	Brief string
	Group string
	Date  string // YYYYMMDD
}

var datePattern = regexp.MustCompile(`^20\d{6}$`)

// ParseFileName 这个函数做什么：按命名契约拆题文件名。
//
// 组别必须在注册表 enums.groups 内，**右起匹配**：末段是 8 位日期、倒数第二段是组别，
// 剩下的全归简要说明——这样 `AI方向_初中_初中_20260814.md` 的简要说明才是「AI方向_初中」
// 而不是「AI方向」。不合契约（例如 `..._对外版.md`）返回错误，由调用方记 warning。
func ParseFileName(name string) (FileName, error) {
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".markdown"), ".md")
	parts := strings.Split(stem, "_")
	if len(parts) < 3 {
		return FileName{}, fmt.Errorf("文件名不合契约 <简要说明>_<组别>_<YYYYMMDD>：%s", name)
	}
	date := parts[len(parts)-1]
	group := parts[len(parts)-2]
	if !datePattern.MatchString(date) {
		return FileName{}, fmt.Errorf("文件名末段不是 YYYYMMDD 日期：%s", name)
	}
	if !enums().group[group] {
		return FileName{}, fmt.Errorf("文件名组别 %q 不在注册表 enums.groups 内：%s", group, name)
	}
	brief := strings.Join(parts[:len(parts)-2], "_")
	if brief == "" {
		return FileName{}, fmt.Errorf("文件名缺少简要说明段：%s", name)
	}
	return FileName{Brief: brief, Group: group, Date: date}, nil
}

// enumSets 是注册表枚举的只读快照（组别 / 来源）。
type enumSets struct {
	group  map[string]bool
	source map[string]bool
	err    error
}

var (
	enumsOnce  sync.Once
	enumsCache enumSets
)

// enums 这个函数做什么：取注册表里的组别与来源枚举，进程内只加载一次。
// 枚举唯一真源在 internal/workflow/registry，本包一个字都不复刻。
func enums() enumSets {
	enumsOnce.Do(func() {
		reg, err := registry.Load()
		if err != nil {
			enumsCache.err = err
			enumsCache.group = map[string]bool{}
			enumsCache.source = map[string]bool{}
			return
		}
		enumsCache.group = toSet(reg, "groups")
		enumsCache.source = toSet(reg, "sources")
	})
	return enumsCache
}

func toSet(reg *registry.Registry, name string) map[string]bool {
	values, _ := reg.EnumValues(name)
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}
