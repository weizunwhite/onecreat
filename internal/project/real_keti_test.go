package project

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// realKetiRoot 是本机真实课题库的在研项目根。
const realKetiRoot = "/Users/localwork/课题/01_在研项目"

// TestRealKetiProjectJSONRoundTrips 拿真实课题库里全部 P26* 的 project.json 做
// **只读**字节回环：读进来 → 在内存里渲染 → 与原始字节比对。
//
// 这条测试**绝不写回真实目录**：既不 Save、也不建临时副本再落盘到课题库，
// 全程只在内存里比对。默认跳过，靠 ONECREAT_REAL_KETI=1 打开。
func TestRealKetiProjectJSONRoundTrips(t *testing.T) {
	if os.Getenv("ONECREAT_REAL_KETI") != "1" {
		t.Skip("需要 ONECREAT_REAL_KETI=1 且本机有真实课题库")
	}
	res, err := Scan(realKetiRoot)
	if err != nil {
		t.Skipf("真实课题库不可读，跳过: %v", err)
	}
	if len(res.Skipped) > 0 {
		t.Errorf("有目录读不出 project.json: %+v", res.Skipped)
	}
	if len(res.Projects) == 0 {
		t.Fatal("一个项目都没扫到")
	}
	t.Logf("扫到 %d 个项目", len(res.Projects))

	for _, p := range res.Projects {
		name := filepath.Base(p.Dir)
		path := filepath.Join(p.Dir, FileName)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got := p.Doc().Bytes() // 纯内存渲染，不落盘
		if !bytes.Equal(got, want) {
			t.Errorf("%s: 内存渲染与原文件不是逐字节相同\n--- want ---\n%q\n--- got ---\n%q",
				name, want, got)
		}
		if _, ok := p.Stage(); !ok {
			t.Errorf("%s: stage 读不出合法值（%s）", name, p.Code())
		}
	}
}
