package topic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 题卡状态。铁律：文件位置就是状态（注册表 enums.topic_states）。
//
// 注册表里还有第四态 establishing（老板点了「立项」、有活跃立项线程）。它依赖对话
// 线程这份**运行时**状态，磁盘上没有任何痕迹，本扫描器看不见也不该猜——establishing
// 由 M3 的线程层在扫描结果之上叠加。
const (
	StateCandidate     = "candidate"
	StateUnestablished = "unestablished"
	StateEstablished   = "established"
)

// 三处来源（相对 ketiRoot）。
const (
	dirSource        = "00_产题评题"
	dirProjects      = "01_在研项目"
	dirUnestablished = "_未立项"
	dirStorage       = "_存量文件夹"

	// kindUnestablished / kindEstablished 是两种非产评来源的标记。
	// 产评来源的 Kind 用注册表 enums.sources 的取值（快速产题/深度产题/…/存量）。
	kindUnestablished = "未立项"
	kindEstablished   = "立项"
	sourceFallback    = "存量"
)

// SourceRef 是题卡的一处来源。
type SourceRef struct {
	// Path 是相对 ketiRoot 的路径，始终用 / 分隔。
	Path string
	// Kind 是来源类型：产评文件取 frontmatter.source（必须在 enums.sources 内，
	// 不合法降级成「存量」并记 warning），未立项目录取「未立项」，项目取「立项」。
	Kind string
}

// Card 是一张题卡。题卡不是文件，是索引产物：同一道题在产题、评题、未立项、已立项
// 四处出现，合并成这一张。
type Card struct {
	Key                 string      // 归一化题名（见 Key）
	Title               string      // 展示用原题名，取各来源中最长的一条
	State               string      // candidate / unestablished / established
	Sources             []SourceRef // 这道题出现过的地方，按路径排序
	Group               string      // 学段组别，缺省「混合」
	Dates               []string    // 出现过的日期（YYYYMMDD），倒序
	ProjectCode         string      // established 才有
	UnestablishedReason string      // unestablished 才有
}

// Index 是一次扫描的快照。Cards 按 Key 升序，保证同一棵目录树扫两次结果逐字节一致。
type Index struct {
	Cards    []Card
	Warnings []string
}

// record 是合并前的一条原始题记录。
type record struct {
	key      string
	title    string
	state    string
	group    string
	date     string
	source   SourceRef
	code     string
	reason   string
	hasState bool
}

// Scan 这个函数做什么：扫描课题根目录，产出题卡索引。
//
// ketiRoot 是「课题」根的绝对路径（内含 00_产题评题/ 与 01_在研项目/）。
// **全程只读**：不创建、不改名、不写任何文件。四处来源：
//
//	00_产题评题/{产题,评题}/*.md            → candidate
//	00_产题评题/_存量文件夹/<夹名>/          → candidate（一夹一卡）
//	01_在研项目/_未立项/<题名>_<组别>_<日期>/ → unestablished
//	01_在研项目/P26*/project.json           → established
//
// 某处来源不存在不是错误（记 warning）；只有根目录本身不可读才返回 error。
func Scan(ketiRoot string) (*Index, error) {
	if ketiRoot == "" {
		return nil, fmt.Errorf("ketiRoot 为空")
	}
	root, err := filepath.Abs(ketiRoot)
	if err != nil {
		return nil, fmt.Errorf("解析 ketiRoot 失败：%w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("读 ketiRoot 失败：%w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ketiRoot 不是目录：%s", root)
	}
	if e := enums(); e.err != nil {
		return nil, fmt.Errorf("加载工作流注册表失败：%w", e.err)
	}

	var records []record
	var warnings []string
	for _, bucket := range []string{"产题", "评题"} {
		got, warn := scanSourceBucket(root, filepath.Join(root, dirSource, bucket))
		records = append(records, got...)
		warnings = append(warnings, warn...)
	}
	stoRecords, stoWarn := scanStorageFolders(root, filepath.Join(root, dirSource, dirStorage))
	records = append(records, stoRecords...)
	warnings = append(warnings, stoWarn...)

	unRecords, unWarn := scanUnestablished(root, filepath.Join(root, dirProjects, dirUnestablished))
	records = append(records, unRecords...)
	warnings = append(warnings, unWarn...)

	projRecords, aliases, projWarn := scanProjects(root, filepath.Join(root, dirProjects))
	records = append(records, projRecords...)
	warnings = append(warnings, projWarn...)

	return &Index{Cards: merge(records, aliases), Warnings: warnings}, nil
}

// skipMarkdown 这个函数做什么：判断一份 md 要不要整份跳过（不出任何题记录）。
//
// 口径照抄 topic_index.py，四条：
//   - `readme*`（不分大小写）——索引说明，不是题（records_from_markdown 首行）
//   - 文件名含「索引」——同上
//   - `._` 开头——macOS 资源分叉残留
//   - stem 含「对外版」——对外版是同名内部版的「发 B 端」删减稿，题都在内部版里；
//     单独索引只会把它的大标题当成一张假题卡（scan_source_records 原注释）
//
// 另加 `.` 开头的隐藏文件。
func skipMarkdown(name string) bool {
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".markdown"), ".md")
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "."):
		return true
	case strings.HasPrefix(lower, "readme"):
		return true
	case strings.Contains(stem, "索引"):
		return true
	case strings.Contains(stem, "对外版"):
		return true
	}
	return false
}

// isMarkdown 这个函数做什么：只认 .md / .markdown 两种后缀。
func isMarkdown(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown")
}

// mdOptions 是解析一份题 md 时的外部提示。
type mdOptions struct {
	groupHint string // frontmatter 没写合法组别时用它
	dateHint  string // frontmatter 没写日期时用它（YYYYMMDD）
	// allowFileNameTopic 决定「文件名的 <简要说明>」能不能当一道题。
	// 口径同 topic_index.py 的 allow_title_fallback：**只在 frontmatter.topics 为空时**
	// 兜底，topics 非空时文件名不出题——否则一份 12 题的选题包会凭空多出一张假卡，
	// 与 NASApp 现网看板的卡数对不上。
	allowFileNameTopic bool
}

// recordsFromMarkdown 这个函数做什么：把一份题 md 读成若干条题记录。
func recordsFromMarkdown(root, path string, opts mdOptions) ([]record, []string) {
	var warnings []string
	relPath := rel(root, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{fmt.Sprintf("文件不可读：%s（%v）", relPath, err)}
	}
	fm, err := ParseFrontMatter(string(raw))
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("frontmatter 解析失败：%s（%v）", relPath, err))
	}

	// 来源必须在注册表 enums.sources 内；不合法降级成「存量」并记 warning
	// （口径同 NASApp：单题发散产物 frontmatter 漏写「深度产题」就会被降级）。
	source := fm.Source
	if !enums().source[source] {
		warnings = append(warnings, fmt.Sprintf("来源 %q 不在 enums.sources 内，降级为「存量」：%s", source, relPath))
		source = sourceFallback
	}
	ref := SourceRef{Path: relPath, Kind: source}

	group := fm.Group
	if !enums().group[group] {
		group = opts.groupHint
	}
	date := normalizeDate(fm.Date)
	if date == "" {
		date = opts.dateHint
	}

	entries := append([]TopicEntry{}, fm.Topics...)
	// 假题守卫：旧版评题报告把「下一步行动表」整段写进了 frontmatter.topics，
	// 每行都会变成一张假题卡（2026-08-20 RLHF 卡片暴露）。判假就整体作废，改用
	// subject 合成唯一一题——一份评题报告评的本来就是 subject 那一道题。
	if fm.Kind == "评题" && len(entries) > 0 && !scoringTopicsPlausible(entries, fm.Subject) {
		warnings = append(warnings, "评题 topics 疑似「下一步行动表」，整体作废改用 subject："+relPath)
		entries = nil
		if subject := trimTitle(fm.Subject); subject != "" {
			entries = []TopicEntry{{Title: subject}}
		}
	}

	titles := make([]string, 0, len(entries))
	for _, entry := range entries {
		titles = append(titles, entry.Title)
	}
	if len(titles) == 0 && opts.allowFileNameTopic {
		// 兜底：这份文件自己就是一道题，题名取命名契约里的 <简要说明> 段。
		if parsed, err := ParseFileName(filepath.Base(path)); err == nil {
			titles = append(titles, parsed.Brief)
		}
	}
	var records []record
	for _, title := range titles {
		if rec, ok := newRecord(title, group, date, ref); ok {
			records = append(records, rec)
		}
	}
	return records, warnings
}

// timeish 是「时限短语」的指纹：本周 / 立项前 / 6-8 周 / 3 天内 …
// 这是「下一步行动表」被整段写进 topics 时 brief 列的长相（topic_index.py:224）。
var timeish = regexp.MustCompile(`^(?:本周|下周|本月|下月|立项前|立项后|开题前|\d+\s*[-–~～]\s*\d+\s*周|\d+\s*周内?|\d+\s*天内?|当天|即刻|随时)`)

// scoringTopicsPlausible 这个函数做什么：判断评题报告 frontmatter 里的 topics 是不是真题。
//
// 口径同 topic_index.py:207。判假需要**同时**满足两条：
// ① 没有一条 topic 与 subject 沾边（归一化后互为子串就算沾边）；
// ② 过半 topic 的 brief 是时限短语——这就是「下一步行动表」的指纹。
// 批量评题（subject 是批名、topics 是多道真题、brief 是正经一句话）不满足 ②，照旧信任。
func scoringTopicsPlausible(topics []TopicEntry, subject string) bool {
	if len(topics) == 0 {
		return true
	}
	if subjectKey := Key(trimTitle(subject)); subjectKey != "" {
		for _, topic := range topics {
			key := Key(trimTitle(topic.Title))
			if key != "" && (strings.Contains(subjectKey, key) || strings.Contains(key, subjectKey)) {
				return true
			}
		}
	}
	hits := 0
	for _, topic := range topics {
		if timeish.MatchString(strings.TrimSpace(topic.Brief)) {
			hits++
		}
	}
	return hits*2 < len(topics)
}

// trimTitle 这个函数做什么：剥掉题名外层的书名号与引号（口径同 Python 的 strip("《》\"'")）。
func trimTitle(value string) string {
	return strings.Trim(strings.TrimSpace(value), "《》\"'")
}

// scanSourceBucket 这个函数做什么：扫描产题/评题桶下的平铺 md。
//
// 每份文件出 frontmatter.topics 逐条一题；topics 为空时才用文件名的 <简要说明> 兜底一题。
func scanSourceBucket(root, dir string) ([]record, []string) {
	var records []record
	var warnings []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("来源目录不可读：%s（%v）", rel(root, dir), err)}
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "跳过软链："+rel(root, path))
			continue
		}
		if entry.IsDir() {
			warnings = append(warnings, "非平铺目录未索引："+rel(root, path))
			continue
		}
		name := entry.Name()
		if !isMarkdown(name) || skipMarkdown(name) {
			continue
		}
		parsed, nameErr := ParseFileName(name)
		if nameErr != nil {
			warnings = append(warnings, "文件名不合命名契约："+rel(root, path))
		}
		got, warn := recordsFromMarkdown(root, path, mdOptions{
			groupHint:          parsed.Group, // ParseFileName 已校验过枚举
			dateHint:           parsed.Date,
			allowFileNameTopic: nameErr == nil,
		})
		records = append(records, got...)
		warnings = append(warnings, warn...)
	}
	return records, warnings
}

// legacyPackage 是存量夹里唯一还继续拆题的东西：旧 topic-express 的「内部版」选题包
// （topic_index.py:LEGACY_PACKAGE_RE = ^选题包.*内部版）。
var legacyPackage = regexp.MustCompile(`^选题包.*内部版`)

// scanStorageFolders 这个函数做什么：扫描 00_产题评题/_存量文件夹，**一夹一卡**。
//
// 口径同 topic_index.py:scan_storage_folders：存量夹不进正文拆题，整个文件夹就是一道
// 「题」（题名=文件夹名，来源=存量，组别从夹名推断，日期取夹的 mtime）；唯一例外是夹内
// 的旧内部版选题包，那里面的 frontmatter.topics 照常逐条拆题、但**不做文件名兜底**。
func scanStorageFolders(root, dir string) ([]record, []string) {
	var records []record
	var warnings []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("存量文件夹不可读：%s（%v）", rel(root, dir), err)}
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "跳过软链："+rel(root, path))
			continue
		}
		if !entry.IsDir() {
			warnings = append(warnings, "存量散文件未索引："+rel(root, path))
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		group := inferGroup(entry.Name())
		date := ""
		if info, err := entry.Info(); err == nil {
			date = info.ModTime().Format("20060102")
		}
		if rec, ok := newRecord(entry.Name(), group, date,
			SourceRef{Path: rel(root, path), Kind: sourceFallback}); ok {
			records = append(records, rec)
		}

		// 夹内递归找旧内部版选题包。
		walkErr := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("存量夹遍历出错：%s（%v）", rel(root, p), err))
				return nil //nolint:nilerr // 单个子项出错不该让整棵树扫描失败
			}
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
				return nil
			}
			name := d.Name()
			if !isMarkdown(name) || strings.HasPrefix(name, "._") {
				return nil
			}
			stem := strings.TrimSuffix(strings.TrimSuffix(name, ".markdown"), ".md")
			if !legacyPackage.MatchString(stem) {
				return nil
			}
			got, warn := recordsFromMarkdown(root, p, mdOptions{
				groupHint: group, dateHint: date, allowFileNameTopic: false,
			})
			records = append(records, got...)
			warnings = append(warnings, warn...)
			return nil
		})
		if walkErr != nil {
			warnings = append(warnings, fmt.Sprintf("存量夹遍历失败：%s（%v）", rel(root, path), walkErr))
		}
	}
	return records, warnings
}

// inferGroup 这个函数做什么：从夹名/文件名的年级线索猜组别（口径同 topic_index.py:infer_group）。
// 猜不出回落「混合」。只用于没有命名契约可依的存量夹。
func inferGroup(values ...string) string {
	text := strings.Join(values, " ")
	switch {
	case strings.Contains(text, "初中") || reJunior.MatchString(text):
		return "初中"
	case strings.Contains(text, "高中") || reSenior.MatchString(text):
		return "高中"
	case reLowerPrimary.MatchString(text):
		return "小学低"
	case reUpperPrimary.MatchString(text):
		return "小学高"
	}
	return "混合"
}

var (
	reJunior       = regexp.MustCompile(`初[一二三123]`)
	reSenior       = regexp.MustCompile(`高[一二三123]`)
	reLowerPrimary = regexp.MustCompile(`(?:一|二|1|2)年级`)
	reUpperPrimary = regexp.MustCompile(`(?:三|四|五|六|3|4|5|6)年级`)
)

// scanUnestablished 这个函数做什么：把未立项目录变成 unestablished 记录。
func scanUnestablished(root, dir string) ([]record, []string) {
	var records []record
	var warnings []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("未立项目录不可读：%s（%v）", rel(root, dir), err)}
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "跳过软链："+rel(root, path))
			continue
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// 目录名与题文件同一套命名契约，只是没有 .md 后缀。
		title, group, date := entry.Name(), "", ""
		if parsed, err := ParseFileName(entry.Name() + ".md"); err != nil {
			warnings = append(warnings, "未立项目录名不合命名契约："+rel(root, path))
		} else {
			title, group, date = parsed.Brief, parsed.Group, parsed.Date
		}

		reason := ""
		if raw, err := os.ReadFile(filepath.Join(path, "未立项原因.md")); err == nil {
			reason = firstLine(string(raw))
		}

		rec, ok := newRecord(title, group, date, SourceRef{Path: rel(root, path), Kind: kindUnestablished})
		if !ok {
			continue
		}
		rec.state, rec.hasState, rec.reason = StateUnestablished, true, reason
		records = append(records, rec)
	}
	return records, warnings
}

// projectMeta 是 project.json 里本包唯一关心的三个键。
//
// 刻意**不** import internal/project、也不复刻它：题库扫描只需要「这道题已建档、
// 编号是什么」，多读一个字段都是耦合。只读，不写。
type projectMeta struct {
	Code      string `json:"code"`
	FullName  string `json:"full_name"`
	ShortName string `json:"short_name"`
}

// scanProjects 这个函数做什么：从 P26* 项目的 project.json 反查已建档的题。
//
// 返回值第二项是别名表：short_name 的 key → 项目编号。别名**只提升已有题卡的状态**，
// 自己不建卡——短名是项目的内部叫法，不是一道题。
func scanProjects(root, dir string) ([]record, map[string]string, []string) {
	var records []record
	var warnings []string
	aliases := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, aliases, []string{fmt.Sprintf("项目目录不可读：%s（%v）", rel(root, dir), err)}
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "P26") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "跳过软链："+rel(root, path))
			continue
		}
		if !entry.IsDir() {
			continue
		}
		metaPath := filepath.Join(path, "project.json")
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			warnings = append(warnings, "project.json 读不到："+rel(root, metaPath))
			continue
		}
		var meta projectMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			warnings = append(warnings, fmt.Sprintf("project.json 无法解析：%s（%v）", rel(root, metaPath), err))
			continue
		}
		code := strings.TrimSpace(meta.Code)
		ref := SourceRef{Path: rel(root, metaPath), Kind: kindEstablished}
		rec, ok := newRecord(strings.TrimSpace(meta.FullName), "", "", ref)
		if !ok {
			warnings = append(warnings, "project.json 的 full_name 归一化后为空："+ref.Path)
			continue
		}
		rec.state, rec.hasState, rec.code = StateEstablished, true, code
		records = append(records, rec)

		if aliasKey := Key(strings.TrimSpace(meta.ShortName)); aliasKey != "" && aliasKey != rec.key {
			aliases[aliasKey] = code
		}
	}
	return records, aliases, warnings
}

// newRecord 这个函数做什么：把一条题名做成记录；归一化后为空的题名直接丢弃。
func newRecord(title, group, date string, ref SourceRef) (record, bool) {
	title = strings.Trim(strings.TrimSpace(title), "《》")
	key := Key(title)
	if key == "" {
		return record{}, false
	}
	return record{key: key, title: title, group: group, date: date, source: ref}, true
}

// statePriority 是同 key 多处出现时的状态优先级：established > unestablished > candidate。
func statePriority(state string) int {
	switch state {
	case StateEstablished:
		return 2
	case StateUnestablished:
		return 1
	default:
		return 0
	}
}

// merge 这个函数做什么：按 Key 把记录聚合成题卡。
func merge(records []record, aliases map[string]string) []Card {
	cards := map[string]*Card{}
	// 先按日期升序，让「组别取最新一次的说法」这条规则有确定的先后。
	sorted := append([]record{}, records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].date < sorted[j].date })

	for _, rec := range sorted {
		card := cards[rec.key]
		if card == nil {
			card = &Card{Key: rec.key, Title: rec.title, State: StateCandidate, Group: "混合"}
			cards[rec.key] = card
		}
		// 标题取最长的原题名；等长时取字典序小的一条，保证确定性。
		if len(rec.title) > len(card.Title) ||
			(len(rec.title) == len(card.Title) && rec.title < card.Title) {
			card.Title = rec.title
		}
		if enums().group[rec.group] {
			card.Group = rec.group
		}
		if rec.date != "" && !contains(card.Dates, rec.date) {
			card.Dates = append(card.Dates, rec.date)
		}
		if rec.source.Path != "" && !hasSource(card.Sources, rec.source) {
			card.Sources = append(card.Sources, rec.source)
		}
		if rec.hasState && statePriority(rec.state) >= statePriority(card.State) {
			card.State = rec.state
			if rec.code != "" {
				card.ProjectCode = rec.code
			}
			card.UnestablishedReason = rec.reason
		}
	}

	// 短名别名：只把已经存在的题卡提升成已立项，不新建卡。
	for key, code := range aliases {
		card := cards[key]
		if card == nil || card.State == StateEstablished {
			continue
		}
		card.State = StateEstablished
		card.ProjectCode = code
		card.UnestablishedReason = ""
	}

	out := make([]Card, 0, len(cards))
	for _, card := range cards {
		if card.State != StateUnestablished {
			card.UnestablishedReason = ""
		}
		if card.State != StateEstablished {
			card.ProjectCode = ""
		}
		sort.Slice(card.Sources, func(i, j int) bool {
			if card.Sources[i].Path != card.Sources[j].Path {
				return card.Sources[i].Path < card.Sources[j].Path
			}
			return card.Sources[i].Kind < card.Sources[j].Kind
		})
		sort.Sort(sort.Reverse(sort.StringSlice(card.Dates)))
		out = append(out, *card)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// normalizeDate 这个函数做什么：把 frontmatter 里的 2026-08-20 写法折成 20260820。
func normalizeDate(value string) string {
	compact := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if datePattern.MatchString(compact) {
		return compact
	}
	return ""
}

// firstLine 这个函数做什么：取正文首个非空行（未立项原因只用第一行）。
func firstLine(text string) string {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\uFEFF"), "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// rel 这个函数做什么：算相对 ketiRoot 的展示路径，始终用 / 分隔。
func rel(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func hasSource(list []SourceRef, want SourceRef) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
