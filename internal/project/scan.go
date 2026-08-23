package project

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// dirPattern 是 `01_在研项目/` 下项目目录名的宽口径正则，与编号登记簿一致：
// `P26[CBG]?-NNN` 前缀（`?` 是为了收下存量的 `P26-NNN`），后面跟 `_短名`。
// 只锚定前缀，不校验短名内容 —— 短名是中文自由文本。
var dirPattern = regexp.MustCompile(`^P26[CBG]?-\d{3}`)

// UnestablishedDir 是未立项包所在目录，扫描时跳过：它不分配 P 号、不预建七目录。
const UnestablishedDir = "_未立项"

// Skip 是一条被跳过的一级目录。缺 project.json 或解析失败都不该让整轮扫描崩掉，
// 而是收进报告一并返回，让调用方决定怎么呈现。
type Skip struct {
	Dir    string // 目录名（不是全路径）
	Reason string // 人话原因
	Err    error  // 底层错误，可能为 nil
}

// ScanResult 是一次扫描的完整结果。
type ScanResult struct {
	Projects []*Project
	Skipped  []Skip
}

// Scan 枚举 root（语义上是 `课题/01_在研项目/`）下的项目目录。
//
// 只看一级目录：名字匹配 dirPattern 的才算项目，`_未立项/`、说明文件、`.DS_Store`
// 等散文件一律忽略（不进 Skipped —— 它们本来就不是项目）。匹配到名字但读不出
// project.json 的目录进 Skipped，不返回错误。
//
// 只有 root 本身读不了才返回错误。
func Scan(root string) (*ScanResult, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == UnestablishedDir || !dirPattern.MatchString(name) {
			continue
		}
		p, err := Load(filepath.Join(root, name))
		if err != nil {
			reason := "读 project.json 失败"
			if os.IsNotExist(err) {
				reason = "目录下没有 project.json"
			}
			res.Skipped = append(res.Skipped, Skip{Dir: name, Reason: reason, Err: err})
			continue
		}
		res.Projects = append(res.Projects, p)
	}
	// 按目录名排序，让结果可比、可 diff。
	sort.Slice(res.Projects, func(i, j int) bool {
		return res.Projects[i].Dir < res.Projects[j].Dir
	})
	sort.Slice(res.Skipped, func(i, j int) bool {
		return res.Skipped[i].Dir < res.Skipped[j].Dir
	})
	return res, nil
}
