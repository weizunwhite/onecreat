package topic

import (
	"encoding/json"
	"os"
	"testing"
)

// goldenCase 是 testdata/golden_keys.json 的一条黄金样本。
type goldenCase struct {
	Input        string          `json:"input"`
	Key          string          `json:"key"`
	Source       string          `json:"source"`
	CollidesWith json.RawMessage `json:"collides_with,omitempty"`
}

// loadGolden 这个函数做什么：读全部 129 条黄金样本。
func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden_keys.json")
	if err != nil {
		t.Fatalf("读黄金样本失败：%v", err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("解析黄金样本失败：%v", err)
	}
	if len(cases) == 0 {
		t.Fatal("黄金样本为空")
	}
	return cases
}

// TestKeyMatchesGolden 是 M1 的硬约束：Go 归一化必须与历史 key 逐字节一致。
func TestKeyMatchesGolden(t *testing.T) {
	cases := loadGolden(t)
	if len(cases) != 129 {
		t.Fatalf("黄金样本条数变了：want 129, got %d", len(cases))
	}
	for _, c := range cases {
		got := Key(c.Input)
		if got != c.Key {
			t.Errorf("Key(%q) = %q, want %q（来源 %s）", c.Input, got, c.Key, c.Source)
		}
	}
}

// TestKeyCollisions 断言样本里标注的碰撞对确实归一到同一个 key。
func TestKeyCollisions(t *testing.T) {
	cases := loadGolden(t)
	pairs := 0
	for _, c := range cases {
		if len(c.CollidesWith) == 0 {
			continue
		}
		var others []string
		if err := json.Unmarshal(c.CollidesWith, &others); err != nil {
			// collides_with 可能是单个字符串，也可能是字符串数组。
			var one string
			if err2 := json.Unmarshal(c.CollidesWith, &one); err2 != nil {
				t.Fatalf("collides_with 既不是字符串也不是数组：%s", c.CollidesWith)
			}
			others = []string{one}
		}
		for _, other := range others {
			pairs++
			if Key(c.Input) != Key(other) {
				t.Errorf("碰撞对未碰撞：Key(%q)=%q vs Key(%q)=%q",
					c.Input, Key(c.Input), other, Key(other))
			}
		}
	}
	if pairs == 0 {
		t.Fatal("样本里一条 collides_with 都没读到，断言形同虚设")
	}
	t.Logf("校验碰撞对 %d 条", pairs)
}
