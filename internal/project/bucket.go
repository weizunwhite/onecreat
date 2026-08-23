package project

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"reasonix/internal/workflow/registry"
)

// 七目录。名称必须逐字一致，不得自造第八桶、不得改写变体
// （`01_在研项目/README.md` §七 AI 操作前自检第 3 条）。
const (
	BucketProposal  = "01_立项定题"
	BucketPlatform  = "02_平台材料"
	BucketWorkshop  = "03_研发工作区"
	BucketClassroom = "04_课堂记录"
	BucketPhotos    = "05_上课照片"
	BucketCollab    = "06_项目协作"
	BucketContest   = "07_竞赛提交"
)

// Buckets 是七目录的固定顺序列表。
var Buckets = []string{
	BucketProposal, BucketPlatform, BucketWorkshop,
	BucketClassroom, BucketPhotos, BucketCollab, BucketContest,
}

var bucketSet = func() map[string]bool {
	m := make(map[string]bool, len(Buckets))
	for _, b := range Buckets {
		m[b] = true
	}
	return m
}()

// ErrReject 是哨兵错误：这条 relpath 不接受分类。
// 用 errors.Is(err, ErrReject) 判定，具体原因在 RejectError.Reason。
var ErrReject = errors.New("拒绝分类")

// ErrUnknownMaterial 是哨兵错误：材料类型没在注册表里登记。
// 注册表是唯一真源，未注册一律 fail-closed，不许兜底猜测。
var ErrUnknownMaterial = errors.New("材料类型未注册")

// RejectError 带上被拒的路径与原因。
type RejectError struct {
	Path   string
	Reason string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("拒绝分类 %q: %s", e.Path, e.Reason)
}

func (e *RejectError) Unwrap() error { return ErrReject }

func reject(relpath, reason string) (BucketResult, error) {
	return BucketResult{}, &RejectError{Path: relpath, Reason: reason}
}

// BucketResult 是一次判定的结果。Note 记判定依据，给 UI / 日志解释"为什么进这个桶"。
type BucketResult struct {
	Bucket string
	Note   string
}

// 机制目录白名单（README §五）。键是机制目录名，值是它**只**允许出现在哪些桶下面。
// 机制目录不是业务桶，它是"就地归属"标记：文件落在哪个桶下的机制目录里，就归哪个桶。
var mechanismDirs = map[string][]string{
	"App产物": Buckets, // 生成物隔离，七目录下都可以（须与产物业务类型相符，那是内容语义，本函数不判）
	"平台":    {BucketPlatform, BucketClassroom, BucketPhotos},
	"_未分类":  {BucketWorkshop},
	"投放":    {BucketCollab},
	"交稿":    {BucketCollab},
	"平台素材包": {BucketContest},
}

// 关键词规则表。**顺序即优先级**，第一条命中的赢。
//
// 顺序的关键处：竞赛/答辩类要排在研发图纸类前面 —— `结构原理图_答辩导出.png`
// 里既有"原理图"又有"答辩"，README §三判例④ 说这种随答辩材料归 07。
var nameRules = []struct {
	bucket   string
	exts     []string
	keywords []string
}{
	{
		bucket:   BucketContest,
		keywords: []string{"答辩", "展板", "提交包", "竞赛", "金鹏", "查新", "匿名化", "论文", "研究报告", "研究方案", "三折"},
	},
	{
		bucket:   BucketWorkshop,
		exts:     []string{".py", ".ino", ".cpp", ".cc", ".c", ".h", ".hpp", ".js", ".ts", ".go", ".rs", ".java", ".stl", ".step", ".stp", ".f3d", ".3mf", ".scad", ".drawio", ".sch", ".kicad_pcb", ".kicad_sch"},
		keywords: []string{"源码", "源代码", "驱动", "采购", "选型", "实验数据", "结构图", "原理图", "爆炸图", "装配图", "设计图", "接线图", "固件", "BOM"},
	},
	{
		bucket:   BucketPhotos,
		exts:     []string{".jpg", ".jpeg", ".heic", ".heif"},
		keywords: []string{"照片", "随手拍", "抓拍", "合影"},
	},
	{
		bucket:   BucketClassroom,
		keywords: []string{"课堂", "学生作业", "出勤", "考勤", "转写", "课后"},
	},
	{
		bucket:   BucketPlatform,
		keywords: []string{"教案", "课件", "备课", "辅导手册", "技术方案", "论文骨架", "采集手册", "研究日志框架"},
	},
}

var (
	regOnce sync.Once
	regVal  *registry.Registry
	regErr  error
)

// loadRegistry 缓存注册表。材料类型 → 默认桶的映射只在注册表里定义一次
// （`outputs.dir`），本包**不复刻**一份表。
func loadRegistry() (*registry.Registry, error) {
	regOnce.Do(func() { regVal, regErr = registry.Load() })
	return regVal, regErr
}

// Classify 判定一份材料应该落进哪个桶。
//
// materialType 是注册表里的材料 type（可以是旧别名），空串表示"没有材料语境，
// 只按路径判断"。relpath 是**项目根下的相对路径**，调用方必须先转换好——
// 绝对路径一律拒绝。
//
// 判定顺序：
//
//  1. 路径合法性（绝对路径 / 路径穿越 / 隐藏文件 / 一级目录不是七目录之一）→ ErrReject
//  2. 机制目录白名单（README §五）→ 位置非法则 ErrReject，位置合法则**就地归属**
//  3. 有材料类型：注册表 outputs.dir 落在哪个桶就归哪个桶（正本归属会覆盖当前位置）
//  4. 没有材料类型（或该材料的 outputs.dir 不在七目录内）：按文件名关键词推断
//  5. 都推不出来：留在当前路径前缀的桶里
//
// 本函数只负责**判定或拒绝**，不负责搬文件，也不会把未命中的路径自动导向
// `03_研发工作区/_未分类/`（那是调用方的兜底策略，05_夹具与基线 §2 模糊条目 1）。
func Classify(materialType string, relpath string) (BucketResult, error) {
	segs, res, err := splitAndValidate(relpath)
	if err != nil {
		return res, err
	}
	bucket := segs[0]

	// 2. 机制目录：位置合法就就地归属，非法直接拒。
	for _, seg := range segs[1:] {
		allowed, ok := mechanismDirs[seg]
		if !ok {
			continue
		}
		if !contains(allowed, bucket) {
			return reject(relpath, fmt.Sprintf("机制目录 %s/ 只允许出现在 %s 下（README §五）",
				seg, strings.Join(allowed, "、")))
		}
		return BucketResult{
			Bucket: bucket,
			Note:   fmt.Sprintf("README §五：机制目录 %s/ 就地归属 %s", seg, bucket),
		}, nil
	}

	// 3. 有材料类型：注册表说了算。
	if materialType != "" {
		reg, err := loadRegistry()
		if err != nil {
			return BucketResult{}, err
		}
		canon := reg.Canonical(materialType)
		m, ok := reg.Material(canon)
		if !ok {
			return BucketResult{}, fmt.Errorf("%w: %q", ErrUnknownMaterial, materialType)
		}
		if m.Outputs != nil {
			if b := bucketOfDir(m.Outputs.Dir); b != "" {
				return BucketResult{
					Bucket: b,
					Note:   fmt.Sprintf("registry materials[%s].outputs.dir=%s", canon, m.Outputs.Dir),
				}, nil
			}
		}
		// outputs.dir 不在七目录内（产题/评题类材料的 dir 是立项前的题库位置），
		// 只能按 relpath 判——立项后它们是随题带入的孵化阶段原始材料。
	}

	// 4. 关键词推断。
	base := path.Base(relpath)
	if b, note := inferByName(base); b != "" {
		return BucketResult{Bucket: b, Note: note}, nil
	}

	// 5. 兜底：留在当前前缀桶。
	return BucketResult{
		Bucket: bucket,
		Note:   fmt.Sprintf("relpath 前缀已是合法七目录 %s，且无更强判据，判为就地归属", bucket),
	}, nil
}

// splitAndValidate 做全部路径合法性检查，返回切好的路径段。
func splitAndValidate(relpath string) ([]string, BucketResult, error) {
	if relpath == "" {
		res, err := reject(relpath, "relpath 为空")
		return nil, res, err
	}
	if strings.HasPrefix(relpath, "/") {
		res, err := reject(relpath, "绝对路径一律拒绝，Classify 只接受项目根下的相对路径")
		return nil, res, err
	}
	segs := strings.Split(relpath, "/")
	for _, seg := range segs {
		switch {
		case seg == "":
			res, err := reject(relpath, "路径含空段（连续或结尾的 /）")
			return nil, res, err
		case seg == "..":
			res, err := reject(relpath, "路径穿越：relpath 不得含 .. 跳出项目根")
			return nil, res, err
		case seg == ".":
			res, err := reject(relpath, "路径含 . 段")
			return nil, res, err
		case strings.HasPrefix(seg, "."):
			res, err := reject(relpath, fmt.Sprintf("隐藏路径段 %s 不参与分类（.DS_Store / .onecreat/ 等不是业务内容）", seg))
			return nil, res, err
		}
	}
	if len(segs) < 2 {
		res, err := reject(relpath, "项目根散文件不进任何桶：根目录只允许 project.json 一个散文件（README §二）")
		return nil, res, err
	}
	if !bucketSet[segs[0]] {
		res, err := reject(relpath, fmt.Sprintf("一级目录 %q 不是七目录之一，名称须逐字一致、不得自造第八桶（README §七）", segs[0]))
		return nil, res, err
	}
	return segs, BucketResult{}, nil
}

// bucketOfDir 从注册表的 outputs.dir 里找出它落在哪个桶。
// 用子串扫描而不是按 / 切段：`立项` 的 dir 写成
// `01_在研项目/<P>/01_立项定题（建档后）| …`，切段切不出干净的桶名。
// 取出现位置最靠前的那个桶。
func bucketOfDir(dir string) string {
	best, bestAt := "", -1
	for _, b := range Buckets {
		if at := strings.Index(dir, b); at >= 0 && (bestAt < 0 || at < bestAt) {
			best, bestAt = b, at
		}
	}
	return best
}

// inferByName 按文件名关键词/扩展名推断桶；推不出来返回空串。
func inferByName(base string) (string, string) {
	lower := strings.ToLower(base)
	for _, rule := range nameRules {
		for _, ext := range rule.exts {
			if strings.HasSuffix(lower, ext) {
				return rule.bucket, fmt.Sprintf("文件名推断：扩展名 %s → %s", ext, rule.bucket)
			}
		}
		for _, kw := range rule.keywords {
			if strings.Contains(base, kw) {
				return rule.bucket, fmt.Sprintf("文件名推断：关键词「%s」→ %s", kw, rule.bucket)
			}
		}
	}
	return "", ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
