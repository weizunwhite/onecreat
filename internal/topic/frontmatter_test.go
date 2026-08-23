package topic

import (
	"reflect"
	"testing"
)

// TestParseFrontMatter 表驱动覆盖真实语料里出现过的写法与坏输入。
func TestParseFrontMatter(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		want    FrontMatter
		wantErr bool
	}{
		{
			name: "映射列表写法（真实语料主流）",
			text: "---\nkind: 产题\nsource: 深度产题\ngroup: 混合\ndate: 2026-08-20\n" +
				"subject: 花粉过敏\ntopics:\n  - title: 甲题\n    brief: 说明\n    score: S\n" +
				"  - title: 乙题\n    brief: \"\"\n---\n\n正文",
			want: FrontMatter{Kind: "产题", Source: "深度产题", Group: "混合", Date: "2026-08-20",
				Subject: "花粉过敏", Topics: []TopicEntry{{Title: "甲题", Brief: "说明"}, {Title: "乙题"}}},
		},
		{
			name: "纯字符串列表写法",
			text: "---\nkind: 产题\nsource: 快速产题\ntopics:\n  - 甲题\n  - 乙题\n---\n",
			want: FrontMatter{Kind: "产题", Source: "快速产题", Topics: []TopicEntry{{Title: "甲题"}, {Title: "乙题"}}},
		},
		{
			name: "带引号与内嵌转义引号",
			text: "---\nsubject: \"校园场景\"\ntopics:\n  - title: \"教室\\\"后排听不清\\\"测量仪\"\n---\n",
			want: FrontMatter{Subject: "校园场景", Topics: []TopicEntry{{Title: "教室\"后排听不清\"测量仪"}}},
		},
		{
			name: "单引号标量",
			text: "---\ngroup: '初中'\n---\n",
			want: FrontMatter{Group: "初中"},
		},
		{
			name: "BOM + CRLF",
			text: "\uFEFF---\r\nkind: 评题\r\nsource: 深度评题\r\ntopics:\r\n  - title: 甲题\r\n---\r\n",
			want: FrontMatter{Kind: "评题", Source: "深度评题", Topics: []TopicEntry{{Title: "甲题"}}},
		},
		{
			name: "没有 frontmatter：零值 + 不报错",
			text: "# 只是一份普通 markdown\n\ntopics: 这行不算头\n",
			want: FrontMatter{},
		},
		{
			name:    "头开了没闭合：报错不 panic",
			text:    "---\nkind: 产题\ntopics:\n  - title: 甲题\n",
			wantErr: true,
		},
		{
			name: "空文件",
			text: "",
			want: FrontMatter{},
		},
		{
			name: "认不出的行不报错，只跳过",
			text: "---\nkind: 产题\n>>> 这行不是 yaml\ntopics:\n  - title: 甲题\n---\n",
			want: FrontMatter{Kind: "产题", Topics: []TopicEntry{{Title: "甲题"}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseFrontMatter(c.text)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil（%+v）", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestParseFileName 覆盖命名契约 <简要说明>_<组别>_<YYYYMMDD>.md 的右起匹配与坏输入。
func TestParseFileName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    FileName
		wantErr bool
	}{
		{"常规", "校园场景_12课题_小学高_20260806.md", FileName{"校园场景_12课题", "小学高", "20260806"}, false},
		{"简要说明里带组别（右起匹配）", "AI方向_初中_初中_20260814.md", FileName{"AI方向_初中", "初中", "20260814"}, false},
		{"简要说明就是组别名", "初中_初中_20260820.md", FileName{"初中", "初中", "20260820"}, false},
		{"markdown 后缀", "偏振光_初中_20260711.markdown", FileName{"偏振光", "初中", "20260711"}, false},
		{"末段不是日期（对外版）", "AI方向_初中_初中_20260814_对外版.md", FileName{}, true},
		{"组别不在枚举内", "某题_大学_20260814.md", FileName{}, true},
		{"段数不够", "某题_20260814.md", FileName{}, true},
		{"日期不是 20xx 开头", "某题_初中_19990101.md", FileName{}, true},
		{"缺简要说明段", "_初中_20260814.md", FileName{}, true},
		{"空文件名", "", FileName{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseFileName(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestEnumsComeFromRegistry 守住「枚举唯一真源」：本包不得自己抄一份组别/来源。
func TestEnumsComeFromRegistry(t *testing.T) {
	e := enums()
	if e.err != nil {
		t.Fatalf("注册表加载失败：%v", e.err)
	}
	for _, group := range []string{"小学低", "小学高", "初中", "高中", "混合"} {
		if !e.group[group] {
			t.Errorf("enums.groups 缺 %q", group)
		}
	}
	for _, source := range []string{"快速产题", "深度产题", "快速评题", "深度评题", "存量"} {
		if !e.source[source] {
			t.Errorf("enums.sources 缺 %q", source)
		}
	}
}
