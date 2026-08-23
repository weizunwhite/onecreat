package topic

import (
	"reflect"
	"strings"
	"testing"
)

// scanFake 这个函数做什么：扫描 testdata/fakekb 假课题树，返回按 Key 取卡的映射。
func scanFake(t *testing.T) (*Index, map[string]Card) {
	t.Helper()
	index, err := Scan("testdata/fakekb")
	if err != nil {
		t.Fatalf("Scan 失败：%v", err)
	}
	byKey := make(map[string]Card, len(index.Cards))
	for _, card := range index.Cards {
		byKey[card.Key] = card
	}
	return index, byKey
}

// TestScanFakeTree 覆盖三态、同题多来源合并、别名提升与非法来源降级。
func TestScanFakeTree(t *testing.T) {
	index, byKey := scanFake(t)

	// ① 同题三处出现（产题 md / 评题 md / project.json）合并成一张已立项卡。
	noise, ok := byKey["校园噪声监测仪"]
	if !ok {
		t.Fatalf("缺题卡「校园噪声监测仪」，实得 %v", keysOf(index))
	}
	if noise.State != StateEstablished || noise.ProjectCode != "P26C-999" {
		t.Errorf("已立项状态错：state=%q code=%q", noise.State, noise.ProjectCode)
	}
	if noise.Group != "初中" {
		t.Errorf("组别错：%q", noise.Group)
	}
	if want := []string{"20260805", "20260801"}; !reflect.DeepEqual(noise.Dates, want) {
		t.Errorf("日期应倒序去重：got %v want %v", noise.Dates, want)
	}
	wantSources := []SourceRef{
		{Path: "00_产题评题/产题/校园噪声监测仪_初中_20260801.md", Kind: "深度产题"},
		{Path: "00_产题评题/评题/校园噪声监测仪_初中_20260805.md", Kind: "深度评题"},
		{Path: "01_在研项目/P26C-999_噪声仪/project.json", Kind: "立项"},
	}
	if !reflect.DeepEqual(noise.Sources, wantSources) {
		t.Errorf("来源合并错：\ngot  %v\nwant %v", noise.Sources, wantSources)
	}
	if noise.UnestablishedReason != "" {
		t.Errorf("已立项卡不该带未立项原因：%q", noise.UnestablishedReason)
	}

	// ② 候选态：同一份产题文件里的第二道题。
	if card := byKey["走廊噪声地图仪"]; card.State != StateCandidate || card.ProjectCode != "" {
		t.Errorf("候选卡错：%+v", card)
	}

	// ③ 未立项态：目录名拆题 + 未立项原因取首行。
	unest, ok := byKey["纸飞机滞空计时器"]
	if !ok {
		t.Fatalf("缺未立项题卡，实得 %v", keysOf(index))
	}
	if unest.State != StateUnestablished {
		t.Errorf("状态应为 unestablished：%q", unest.State)
	}
	if unest.UnestablishedReason != "测量精度撑不起一个课题，先放回题库" {
		t.Errorf("未立项原因只取首行：%q", unest.UnestablishedReason)
	}
	if unest.Group != "混合" || !reflect.DeepEqual(unest.Dates, []string{"20260707"}) {
		t.Errorf("未立项目录名拆解错：group=%q dates=%v", unest.Group, unest.Dates)
	}

	// ④ short_name 别名提升已有候选卡，且别名自己不建卡。
	alias, ok := byKey["教室灯光测试台"]
	if !ok {
		t.Fatal("缺被别名提升的题卡「教室灯光测试台」")
	}
	if alias.State != StateEstablished || alias.ProjectCode != "P26C-998" {
		t.Errorf("别名未提升：state=%q code=%q", alias.State, alias.ProjectCode)
	}
	if full := byKey["教室灯光智能测试台"]; full.State != StateEstablished {
		t.Errorf("full_name 应建立已立项卡：%+v", full)
	}

	// ⑤ 文件名不合契约时不出「文件名题」，只出 frontmatter 的题。
	if _, bad := byKey["不合契约的文件名"]; bad {
		t.Error("文件名不合契约却出了题卡")
	}
	if card := byKey["图书角借阅提醒器"]; card.State != StateCandidate || card.Group != "小学高" {
		t.Errorf("坏文件名的 frontmatter 题应照常出卡：%+v", card)
	}

	// ⑥ 非法 source 降级成「存量」。
	if card := byKey["教室灯光测试台"]; len(card.Sources) == 0 || card.Sources[0].Kind != sourceFallback {
		t.Errorf("非法 source 应降级为「存量」：%v", card.Sources)
	}
}

// TestFileNameTopicOnlyAsFallback 断言 topics 非空时文件名不出题（口径同 topic_index.py）。
func TestFileNameTopicOnlyAsFallback(t *testing.T) {
	_, byKey := scanFake(t)
	// 坏来源_初中_20260802.md 的 frontmatter 有一道题，文件名段是「坏来源」——不该出卡。
	if _, bad := byKey["坏来源"]; bad {
		t.Error("topics 非空时文件名不该再出一张题卡")
	}
	if _, ok := byKey["教室灯光测试台"]; !ok {
		t.Error("该文件 frontmatter 里的题应照常出卡")
	}
	// 五年级课桌课题/选题包_内部版.md 有 topics，文件名不合契约也不影响。
	if _, ok := byKey["课桌高度可调实验台"]; !ok {
		t.Error("存量夹内部版选题包的 topics 应照常拆题")
	}
}

// TestSkippedMarkdowns 断言 _对外版 / README / 索引 类文件整份跳过。
func TestSkippedMarkdowns(t *testing.T) {
	_, byKey := scanFake(t)
	for _, key := range []string{"对外版假题", "选题包对外版", "索引里的假题", "readme选题包索引"} {
		if _, bad := byKey[key]; bad {
			t.Errorf("应整份跳过的文件却出了题卡：%q", key)
		}
	}
	// skipMarkdown 的四条规则逐条过一遍。
	for _, name := range []string{
		"选题包_对外版_初中_20260804.md", "README_选题包索引_初中_20260806.md",
		"课题索引_初中_20260806.md", "._幽灵_初中_20260806.md", ".隐藏.md",
	} {
		if !skipMarkdown(name) {
			t.Errorf("skipMarkdown(%q) 应为 true", name)
		}
	}
	for _, name := range []string{"校园噪声监测仪_初中_20260801.md", "选题包_内部版_初中_20260731.md"} {
		if skipMarkdown(name) {
			t.Errorf("skipMarkdown(%q) 应为 false", name)
		}
	}
}

// TestScanStorageFolders 断言存量夹一夹一卡 + 内部版选题包例外拆题。
func TestScanStorageFolders(t *testing.T) {
	_, byKey := scanFake(t)
	folder, ok := byKey["五年级课桌课题"]
	if !ok {
		t.Fatal("存量夹应出一夹一卡")
	}
	if folder.State != StateCandidate || folder.Group != "小学高" {
		t.Errorf("存量卡错：state=%q group=%q（组别应从夹名推断出小学高）", folder.State, folder.Group)
	}
	if len(folder.Sources) != 1 || folder.Sources[0].Kind != sourceFallback ||
		folder.Sources[0].Path != "00_产题评题/_存量文件夹/五年级课桌课题" {
		t.Errorf("存量卡来源错：%v", folder.Sources)
	}
	if _, bad := byKey["随手记"]; bad {
		t.Error("存量夹里的普通 md 不该拆题")
	}
}

// TestInferGroup 覆盖存量夹的组别推断（没有命名契约可依时才用）。
func TestInferGroup(t *testing.T) {
	cases := map[string]string{
		"五六年级AI优质课题": "小学高",
		"二年级手工":      "小学低",
		"初二机器人":      "初中",
		"初中机械AI组":    "初中",
		"高一物理":       "高中",
		"SpaceX":     "混合",
	}
	for input, want := range cases {
		if got := inferGroup(input); got != want {
			t.Errorf("inferGroup(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestScanFakeWarnings 断言 warning 覆盖到位。
func TestScanFakeWarnings(t *testing.T) {
	index, _ := scanFake(t)
	wantSubstrings := []string{
		"来源 \"胡说八道\" 不在 enums.sources 内",
		"文件名不合命名契约：00_产题评题/产题/不合契约的文件名.md",
	}
	for _, want := range wantSubstrings {
		if !hasWarning(index.Warnings, want) {
			t.Errorf("缺 warning %q，实得：\n%s", want, strings.Join(index.Warnings, "\n"))
		}
	}
}

// TestScanIsDeterministic 断言两次扫描结果完全一致（Cards 按 Key 排序）。
func TestScanIsDeterministic(t *testing.T) {
	first, err := Scan("testdata/fakekb")
	if err != nil {
		t.Fatalf("Scan 失败：%v", err)
	}
	second, err := Scan("testdata/fakekb")
	if err != nil {
		t.Fatalf("Scan 失败：%v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("两次扫描结果不一致")
	}
	for i := 1; i < len(first.Cards); i++ {
		if first.Cards[i-1].Key >= first.Cards[i].Key {
			t.Fatalf("Cards 未按 Key 升序：%q >= %q", first.Cards[i-1].Key, first.Cards[i].Key)
		}
	}
}

// TestScanBadRoot 断言坏根目录返回错误而不是 panic。
func TestScanBadRoot(t *testing.T) {
	if _, err := Scan(""); err == nil {
		t.Error("空 ketiRoot 应报错")
	}
	if _, err := Scan("testdata/根本不存在"); err == nil {
		t.Error("不存在的 ketiRoot 应报错")
	}
	if _, err := Scan("testdata/golden_keys.json"); err == nil {
		t.Error("ketiRoot 是文件时应报错")
	}
}

// TestScanMissingSubtreesAreWarnings 断言缺来源目录只记 warning，不失败。
func TestScanMissingSubtreesAreWarnings(t *testing.T) {
	index, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("空目录不该报错：%v", err)
	}
	if len(index.Cards) != 0 {
		t.Errorf("空目录不该出卡：%v", index.Cards)
	}
	if len(index.Warnings) != 5 {
		t.Errorf("五处来源目录都缺，应有 5 条 warning：%v", index.Warnings)
	}
}

func keysOf(index *Index) []string {
	out := make([]string, 0, len(index.Cards))
	for _, card := range index.Cards {
		out = append(out, card.Key)
	}
	return out
}

func hasWarning(warnings []string, substring string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substring) {
			return true
		}
	}
	return false
}

// TestBogusScoringTopicsDiscarded 断言假题守卫：评题的「下一步行动表」整体作废，
// 只留 subject 合成的一张卡；批量评题不受影响。
func TestBogusScoringTopicsDiscarded(t *testing.T) {
	index, byKey := scanFake(t)

	// ① 三条行动项一张卡都不该出。
	for _, key := range []string{"安排30分钟校园坡道走查", "确认学生技术起点", "走ideatobuild深度查新"} {
		if _, bad := byKey[key]; bad {
			t.Errorf("行动项不该成题卡：%q", key)
		}
	}
	// ② 改用 subject 合成唯一一题。
	card, ok := byKey["校园坡道通行性评估仪"]
	if !ok {
		t.Fatalf("应由 subject 合成一张卡，实得 %v", keysOf(index))
	}
	if card.State != StateCandidate || len(card.Sources) != 1 ||
		card.Sources[0].Path != "00_产题评题/评题/坡道走查报告_初中_20260809.md" {
		t.Errorf("subject 合成的卡不对：%+v", card)
	}
	if !hasWarning(index.Warnings, "疑似「下一步行动表」") {
		t.Error("整体作废时应记 warning")
	}

	// ③ 反例：批量评题（subject 是批名、topics 是真题）照旧信任，subject 自己不出卡。
	if _, bad := byKey["初中批次评题汇总"]; bad {
		t.Error("批量评题的 subject 不该被当成题")
	}
	for _, key := range []string{"走廊噪声地图仪", "课桌照度普查车"} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("批量评题的真题应照常出卡：%q", key)
		}
	}
}

// TestScoringTopicsPlausible 单测守卫的两条判据。
func TestScoringTopicsPlausible(t *testing.T) {
	actions := []TopicEntry{
		{Title: "安排走查", Brief: "本周内完成"},
		{Title: "确认技术起点", Brief: "立项前确认"},
		{Title: "补查新", Brief: "6-8 周"},
	}
	if scoringTopicsPlausible(actions, "校园坡道通行性评估仪") {
		t.Error("过半 brief 是时限短语且与 subject 无关 → 应判假")
	}
	// 有一条与 subject 沾边 → 信任（条件 ① 不满足）。
	related := append([]TopicEntry{{Title: "《校园坡道通行性评估仪》", Brief: "本周内"}}, actions...)
	if !scoringTopicsPlausible(related, "校园坡道通行性评估仪") {
		t.Error("有 topic 与 subject 沾边时应信任")
	}
	// brief 是正经一句话 → 信任（条件 ② 不满足）。
	real := []TopicEntry{
		{Title: "走廊噪声地图仪", Brief: "课间噪声分布画不出来"},
		{Title: "橡皮消字率测试台", Brief: "橡皮到底擦掉了多少"},
	}
	if !scoringTopicsPlausible(real, "初中批次评题汇总") {
		t.Error("批量评题应信任")
	}
	if !scoringTopicsPlausible(nil, "任意") {
		t.Error("空 topics 应信任")
	}
}
