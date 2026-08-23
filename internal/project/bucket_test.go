package project

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// bucketCase 是 testdata/bucket_cases.json 里的一条判例。
type bucketCase struct {
	MaterialType *string `json:"material_type"`
	Relpath      string  `json:"relpath"`
	WantBucket   string  `json:"want_bucket"`
	WantNote     string  `json:"want_note"`
}

// TestClassifyAgainstCaseTable 是 bucket.Classify 的验收测试：
// testdata/bucket_cases.json 的 90 条判例逐条断言。
//
// 这份用例表是 `01_在研项目/README.md` §三判例表 + §五机制目录白名单的机械转写，
// 是回归基线而不是穷举规范。判例表本身若变，先改判例表再补用例，
// 不得反过来靠用例反推规则。
func TestClassifyAgainstCaseTable(t *testing.T) {
	data, err := os.ReadFile("testdata/bucket_cases.json")
	if err != nil {
		t.Fatalf("读用例表: %v", err)
	}
	var cases []bucketCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("解析用例表: %v", err)
	}
	if len(cases) != 90 {
		t.Fatalf("用例表应有 90 条，实际 %d 条", len(cases))
	}

	for i, c := range cases {
		mt := ""
		if c.MaterialType != nil {
			mt = *c.MaterialType
		}
		got, err := Classify(mt, c.Relpath)
		if c.WantBucket == "REJECT" {
			if !errors.Is(err, ErrReject) {
				t.Errorf("#%d type=%q rel=%q：期望 ErrReject，实际 bucket=%q err=%v\n  判例出处：%s",
					i, mt, c.Relpath, got.Bucket, err, c.WantNote)
			}
			continue
		}
		if err != nil {
			t.Errorf("#%d type=%q rel=%q：期望 %s，实际报错 %v\n  判例出处：%s",
				i, mt, c.Relpath, c.WantBucket, err, c.WantNote)
			continue
		}
		if got.Bucket != c.WantBucket {
			t.Errorf("#%d type=%q rel=%q：得到 %s（依据：%s），期望 %s\n  判例出处：%s",
				i, mt, c.Relpath, got.Bucket, got.Note, c.WantBucket, c.WantNote)
		}
	}
}

// TestClassifyEveryMaterialTypeIsCovered 验证 25 种注册材料在用例表里都有正例。
func TestClassifyEveryMaterialTypeIsCovered(t *testing.T) {
	data, err := os.ReadFile("testdata/bucket_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []bucketCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.MaterialType != nil && c.WantBucket != "REJECT" {
			seen[*c.MaterialType] = true
		}
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range reg.Materials {
		if !seen[m.Type] {
			t.Errorf("材料类型 %q 在用例表里没有正例", m.Type)
		}
	}
}

// TestClassifyUnknownMaterialFailsClosed 验证注册表 fail-closed：
// 未注册的材料类型报错，不许兜底猜一个桶。
func TestClassifyUnknownMaterialFailsClosed(t *testing.T) {
	_, err := Classify("我编的材料", "02_平台材料/x.md")
	if !errors.Is(err, ErrUnknownMaterial) {
		t.Errorf("未注册材料类型应返回 ErrUnknownMaterial，实际 %v", err)
	}
}

// TestClassifyAcceptsMaterialAliases 验证旧别名走 registry.Canonical。
func TestClassifyAcceptsMaterialAliases(t *testing.T) {
	got, err := Classify("研究日志", "02_平台材料/research_log_scaffold_x.json")
	if err != nil {
		t.Fatalf("别名「研究日志」应能解析: %v", err)
	}
	if got.Bucket != BucketPlatform {
		t.Errorf("别名「研究日志」→ %s，期望 %s", got.Bucket, BucketPlatform)
	}
}

// TestRejectErrorCarriesReason 验证哨兵错误带得动原因。
func TestRejectErrorCarriesReason(t *testing.T) {
	_, err := Classify("", "../../../etc/passwd")
	var re *RejectError
	if !errors.As(err, &re) {
		t.Fatalf("期望 *RejectError，实际 %T", err)
	}
	if re.Reason == "" {
		t.Error("RejectError.Reason 不该为空")
	}
	if !errors.Is(err, ErrReject) {
		t.Error("RejectError 应当 Unwrap 到 ErrReject")
	}
}
