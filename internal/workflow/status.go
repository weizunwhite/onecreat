// Package workflow 是科创工作流的编排层领域包。
//
// 本文件（M1）只做一件事：**材料齐备度**——拿注册表里登记的 25 种材料，逐一去
// 一个在研项目的七目录里找它的产物，回答"这个项目哪些材料已经有了、哪些还没有、
// 哪些本包判不了"。
//
// 判定策略是**只读 + 保守**：宁可漏判（报缺失）也不误判（报已有）。它靠
// 注册表 `outputs.dir` 落桶 + `outputs.exts` 扩展名 + 每材料一条的命名关键词
// 三重条件同时成立才算命中；三条里任何一条对不上就当没有。判不了的（产题/评题
// 这类产物根本不在项目目录里的材料）进 Status.Unknown，绝不假装判过。
//
// 判定规则表是 materialRules，**故意写成一材料一条的明表**：M2 逐个 skill 实测时
// 会拿真实产物来调它，明表比散在代码里的 if 好调。
//
// 边界（架构铁律，见 docs/科创工作流迁移/00_架构规划_v1.md §8.0 A1）：
// 本包属于 L4 领域层，只依赖标准库与同层的 internal/workflow/registry、
// internal/project。它**不得**被 internal/engine、internal/engine/dsh、
// internal/agent import，也不认识 control / toolpolicy / tool / plugin。
//
// 全程只读：不创建、不改名、不写任何文件。
package workflow

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

// evidenceLedgerSuffix 是证据台账的文件名后缀（注册表 materials[研究日志框架].outputs.naming）。
const evidenceLedgerSuffix = "_evidence_ledger.json"

// MaterialStatus 是单种材料的齐备度判定结果。
type MaterialStatus struct {
	Type    string   // 材料类型（注册表 materials[].type）
	Stage   string   // 科创一条链五段之一
	Kind    string   // conversational / oneclick
	Present bool     // 是否已在项目目录里找到产物
	Paths   []string // 命中的产物，项目根下的相对路径，升序
	Rule    string   // 本次用的判定规则（人话，便于 M2 调表）
}

// Status 是一个在研项目的完整齐备度快照。
type Status struct {
	Dir       string // 项目目录绝对路径
	Code      string // P26X-NNN
	ShortName string
	FullName  string
	Line      string
	Student   string
	Stage     int    // 项目生命周期八段，1–8；StageOK 为 false 时无意义
	StageOK   bool   // project.json 的 stage 是否读得出合法值
	StageName string // 段名（注册表 enums.project_stages）

	Materials []MaterialStatus // 按注册表顺序，一材料一条
	Unknown   []string         // 本包判不了的材料类型（产物不在项目目录里）

	BucketFiles    map[string]int // 七目录 → 递归文件计数（不含隐藏文件）
	MissingBuckets []string       // 七目录里磁盘上不存在的

	EvidenceLedger      bool     // 02_平台材料 下有没有 *_evidence_ledger.json
	EvidenceLedgerPaths []string // 台账文件，项目根下的相对路径
}

// materialRule 是一条判定规则。
//
// SubDir 是相对桶的子目录前缀（注册表 outputs.dir 比桶名更深时用），空表示整个桶。
// Keywords 任一命中即可；空表示用材料类型名本身兜底。
type materialRule struct {
	subDir   string
	keywords []string
}

// materialRules 是**每材料一条**的判定规则表（M1 版）。
//
// 没在表里的材料一律走兜底：桶 = 注册表 outputs.dir 所在的七目录之一，
// 扩展名 = outputs.exts，关键词 = 材料类型名本身。
//
// 表里这些是兜底不够用的：产物文件名与材料名对不上（研究日志框架 → research_log_scaffold_*.json）、
// 或者一种材料对应好几个产物名（立项 → 查新速报/立项确认单/…）。
var materialRules = map[string]materialRule{
	// 立项是对话型材料，一轮产出好几份文件，任一份存在即算这一步走过了。
	"立项": {keywords: []string{"查新速报", "技术实现方案", "立项确认单", "立项报告"}},
	// 研究日志框架的产物名是英文脚手架名 + 证据台账。
	"研究日志框架": {keywords: []string{"research_log_scaffold", "evidence_ledger", "研究日志"}},
	// 课件按平台 strict schema 落在 02_平台材料/课件/specs/lesson{N}.json。
	"课件": {subDir: "课件/specs", keywords: []string{"lesson"}},
	// 材料清单在 skill 侧有"材料采购清单"的写法。
	"材料清单":   {keywords: []string{"材料清单", "材料采购清单"}},
	"老师采集手册": {keywords: []string{"老师采集手册", "采集手册"}},
	"项目简要说明": {keywords: []string{"项目简要说明", "简要说明"}},
	"3D交互视图": {keywords: []string{"3D交互", "交互视图"}},
	"图表":     {keywords: []string{"图表", "架构图", "流程图", "接线图", "时序图", "模块图"}},
	"答辩PPT":  {keywords: []string{"答辩"}},
	"金鹏视频":   {keywords: []string{"金鹏"}},
	"原始资料":   {keywords: []string{"原始资料", "实验记录", "问卷"}},
	"照片清单":   {keywords: []string{"照片清单", "拍摄清单"}},
	"匿名化检查":  {keywords: []string{"匿名化"}},
	// 提交整理打包的产物是一个 zip，名字五花八门，只按"提交/打包/素材包"三词收。
	"提交整理打包": {keywords: []string{"提交", "打包", "素材包"}},
}

// ProjectStatus 这个函数做什么：对一个在研项目跑一遍 25 种材料的齐备度判定。
//
// reg 是已加载的注册表（调用方负责 Load，本函数不重复加载）；p 是已读出的项目。
// 全程只读磁盘，不写任何文件。项目目录读不了才返回 error——单个桶不存在不是错误，
// 会记进 MissingBuckets。
func ProjectStatus(reg *registry.Registry, p *project.Project) (*Status, error) {
	if reg == nil {
		return nil, fmt.Errorf("注册表为空")
	}
	if p == nil {
		return nil, fmt.Errorf("项目为空")
	}
	if _, err := os.Stat(p.Dir); err != nil {
		return nil, fmt.Errorf("读项目目录失败：%w", err)
	}

	st := &Status{
		Dir:         p.Dir,
		Code:        p.Code(),
		ShortName:   p.ShortName(),
		FullName:    p.FullName(),
		Line:        p.Line(),
		Student:     p.Student(),
		BucketFiles: map[string]int{},
	}
	st.Stage, st.StageOK = p.Stage()
	if st.StageOK {
		st.StageName = StageName(reg, st.Stage)
	}

	// 一次性把七个桶的文件列出来，后面 25 种材料都在这份快照上判，不重复走盘。
	files := map[string][]string{}
	for _, bucket := range project.Buckets {
		dir := filepath.Join(p.Dir, bucket)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			st.MissingBuckets = append(st.MissingBuckets, bucket)
			continue
		}
		list, err := walkBucket(p.Dir, dir)
		if err != nil {
			return nil, fmt.Errorf("扫描 %s 失败：%w", bucket, err)
		}
		files[bucket] = list
		st.BucketFiles[bucket] = len(list)
	}

	for i := range reg.Materials {
		m := &reg.Materials[i]
		bucket := bucketOfOutputs(m)
		if bucket == "" {
			// 产题/评题这类材料的 outputs.dir 是题库位置（00_产题评题/…），
			// 项目目录里根本没有它的正本——判不了就明说，不猜。
			st.Unknown = append(st.Unknown, m.Type)
			continue
		}
		ms := MaterialStatus{Type: m.Type, Stage: m.Stage, Kind: m.Kind}
		rule := materialRules[m.Type]
		keywords := rule.keywords
		if len(keywords) == 0 {
			keywords = []string{m.Type}
		}
		var exts []string
		if m.Outputs != nil {
			exts = m.Outputs.Exts
		}
		ms.Rule = describeRule(bucket, rule.subDir, exts, keywords)
		ms.Paths = matchFiles(files[bucket], bucket, rule.subDir, exts, keywords)
		ms.Present = len(ms.Paths) > 0
		st.Materials = append(st.Materials, ms)
	}

	// 证据台账：02_平台材料 下的 *_evidence_ledger.json。
	for _, rel := range files[project.BucketPlatform] {
		if strings.HasSuffix(strings.ToLower(rel), strings.ToLower(evidenceLedgerSuffix)) {
			st.EvidenceLedgerPaths = append(st.EvidenceLedgerPaths, rel)
		}
	}
	st.EvidenceLedger = len(st.EvidenceLedgerPaths) > 0
	return st, nil
}

// StageName 这个函数做什么：把项目生命周期段号（1–8）翻成段名。
//
// 段名只在注册表 enums.project_stages 里定义一次，本包不复刻一份。
// 号对不上返回空串。
func StageName(reg *registry.Registry, stage int) string {
	if reg == nil {
		return ""
	}
	for _, d := range reg.Enums["project_stages"].Detail {
		if d.No == stage {
			return d.Value
		}
	}
	return ""
}

// bucketOfOutputs 这个函数做什么：找出一种材料的正本落在七目录的哪个桶里。
//
// 用子串扫描而不是按 / 切段：`立项` 的 outputs.dir 写成
// `01_在研项目/<P>/01_立项定题（建档后）| …`，切段切不出干净的桶名。
// 取出现位置最靠前的那个桶；一个桶都扫不到（题库位置）返回空串。
//
// 注：与 internal/project 里的同名内部函数同法，但那个没导出；两边都只认
// project.Buckets 这**一份**桶名表，不存在第二份表。
func bucketOfOutputs(m *registry.Material) string {
	if m == nil || m.Outputs == nil {
		return ""
	}
	best, bestAt := "", -1
	for _, b := range project.Buckets {
		if at := strings.Index(m.Outputs.Dir, b); at >= 0 && (bestAt < 0 || at < bestAt) {
			best, bestAt = b, at
		}
	}
	return best
}

// walkBucket 这个函数做什么：递归列出一个桶下的全部文件，返回项目根下的相对路径。
//
// 隐藏文件与隐藏目录（`.DS_Store`、`.onecreat/`）整棵跳过——它们不是业务内容。
// 递归而不是只看一层，是因为产物真实落点常带一层机制目录（`App产物/<材料>/x.md`）
// 或工程子目录（`docs/`、`课件/specs/`）。
func walkBucket(root, dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// matchFiles 这个函数做什么：在一个桶的文件清单里挑出符合某材料判定规则的产物。
//
// 三重条件**同时**成立才算命中：子目录前缀（有则必须在它下面）、扩展名（注册表
// outputs.exts，为空则不限）、命名关键词（任一命中）。三条里任何一条对不上就当没有——
// 宁可漏判也不误判。
func matchFiles(files []string, bucket, subDir string, exts, keywords []string) []string {
	prefix := bucket + "/"
	if subDir != "" {
		prefix = bucket + "/" + strings.Trim(subDir, "/") + "/"
	}
	var out []string
	for _, rel := range files {
		if !strings.HasPrefix(rel, prefix) {
			continue
		}
		base := filepath.Base(rel)
		if !extMatches(base, exts) {
			continue
		}
		if !keywordMatches(base, keywords) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// extMatches 扩展名判定；exts 为空表示不限扩展名。
func extMatches(base string, exts []string) bool {
	if len(exts) == 0 {
		return true
	}
	got := strings.ToLower(filepath.Ext(base))
	for _, want := range exts {
		if got == strings.ToLower(want) {
			return true
		}
	}
	return false
}

// keywordMatches 命名关键词判定；任一命中即可。英文关键词（lesson、
// research_log_scaffold）不分大小写。
func keywordMatches(base string, keywords []string) bool {
	lower := strings.ToLower(base)
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// describeRule 把一条判定规则写成人话，挂在 MaterialStatus.Rule 上，
// 让"为什么判它缺失"当场可查（M2 调表时要看这个）。
func describeRule(bucket, subDir string, exts, keywords []string) string {
	where := bucket
	if subDir != "" {
		where = bucket + "/" + strings.Trim(subDir, "/")
	}
	ext := "不限"
	if len(exts) > 0 {
		ext = strings.Join(exts, "/")
	}
	return fmt.Sprintf("桶=%s（含子目录）· 扩展名=%s · 关键词任一=%s",
		where, ext, strings.Join(keywords, "、"))
}
