package topic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realKetiRoot 是本机真实题库根。只读，永不写。
const realKetiRoot = "/Users/localwork/课题"

// TestScanRealKeti 拿真实题库跑一遍全量扫描（只读）。
//
// 默认跳过——它依赖本机数据，CI 上没有。开关：ONECREAT_REAL_KETI=1。
//
// **隐私**：这个测试只断言计数与结构，绝不断言、也绝不打印任何学生姓名；
// 打印的只有三态计数、warning 条数与 warning 的类别摘要。
func TestScanRealKeti(t *testing.T) {
	if os.Getenv("ONECREAT_REAL_KETI") != "1" {
		t.Skip("需要 ONECREAT_REAL_KETI=1 且本机有真实题库")
	}
	if _, err := os.Stat(realKetiRoot); err != nil {
		t.Skipf("本机没有 %s：%v", realKetiRoot, err)
	}

	index, err := Scan(realKetiRoot)
	if err != nil {
		t.Fatalf("真实题库扫描失败：%v", err)
	}

	counts := map[string]int{}
	for _, card := range index.Cards {
		counts[card.State]++
		if card.State == StateEstablished && card.ProjectCode == "" {
			t.Errorf("已立项题卡缺项目编号：key=%q", card.Key)
		}
		if card.Key == "" {
			t.Error("出现空 key 题卡")
		}
	}

	// 已立项题卡数必须等于磁盘上真实的 P26*/project.json 份数：题卡是扫描产物，
	// 「文件位置就是状态」这条铁律在真实数据上就该逐一对上。
	//
	// 基线：夹具 golden_keys.json 生成时（2026-08-23 早些时候）是 27 个项目，当天
	// 又新建了 P26C-031，所以这里对着磁盘实测数比对，而不是钉死一个会过期的常数。
	wantEstablished := countProjectDirs(t, filepath.Join(realKetiRoot, dirProjects))
	if counts[StateEstablished] != wantEstablished {
		t.Errorf("已立项题卡数 = %d，磁盘上 project.json = %d",
			counts[StateEstablished], wantEstablished)
	}

	t.Logf("题卡合计 %d：established=%d unestablished=%d candidate=%d",
		len(index.Cards), counts[StateEstablished], counts[StateUnestablished], counts[StateCandidate])
	t.Logf("warning 合计 %d：%s", len(index.Warnings), warningSummary(index.Warnings))
}

// countProjectDirs 这个函数做什么：数磁盘上带 project.json 的 P26* 项目目录。
func countProjectDirs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读项目目录失败：%v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "P26") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), "project.json")); err == nil {
			count++
		}
	}
	return count
}

// warningSummary 这个函数做什么：把 warning 按类别计数，避免日志里带出路径与姓名。
func warningSummary(warnings []string) string {
	categories := []string{"文件名不合命名契约", "不在 enums.sources 内", "未立项目录名不合命名契约",
		"疑似「下一步行动表」", "存量散文件未索引", "非平铺目录未索引", "跳过软链",
		"frontmatter 解析失败", "project.json"}
	counts := map[string]int{}
	for _, w := range warnings {
		matched := false
		for _, category := range categories {
			if strings.Contains(w, category) {
				counts[category]++
				matched = true
				break
			}
		}
		if !matched {
			counts["其它"]++
		}
	}
	var parts []string
	for _, category := range append(categories, "其它") {
		if counts[category] > 0 {
			parts = append(parts, category+"×"+itoa(counts[category]))
		}
	}
	return strings.Join(parts, " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
