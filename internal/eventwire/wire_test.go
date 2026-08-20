package eventwire

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sync"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// declaredKinds 直接从 internal/event 的源码里读出 Kind 常量块。
//
// 这是这轮传输边界收口的核心守卫:KindNames 现在是 Desktop 与 SSE 共用的唯一真源,
// 而 Encode 对没登记的 Kind 会安静地发出 kind:"",前端 reducer / SSE 客户端直接丢弃
// (历史上 MCPSurfaceReady 就这么漏过一次,E8)。用「常量块的上界」当断言边界抓不到
// 追加在末尾的新 Kind —— 那正是最容易漏的情况 —— 所以这里按 AST 枚举,新增一个 Kind
// 就必须同时在 KindNames 里登记,否则本测试红。
func declaredKinds(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join("..", "event"), nil, 0)
	if err != nil {
		t.Fatalf("解析 internal/event: %v", err)
	}
	pkg, ok := pkgs["event"]
	if !ok {
		t.Fatal("internal/event 里没找到 package event")
	}
	var names []string
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			// 常量块里只有第一个 spec 写类型(`TurnStarted Kind = iota`),
			// 后续 spec 继承它。
			blockType := ""
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if id, ok := vs.Type.(*ast.Ident); ok {
					blockType = id.Name
				}
				if blockType != "Kind" {
					continue
				}
				for _, name := range vs.Names {
					if name.Name != "_" {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("没有从 internal/event 解析出任何 Kind 常量,守卫失效")
	}
	return names
}

func TestKindNamesCoversEveryDeclaredKind(t *testing.T) {
	declared := declaredKinds(t)
	if len(KindNames) != len(declared) {
		t.Fatalf("KindNames 有 %d 项,internal/event 声明了 %d 个 Kind(%v)——新增 Kind 必须在这里登记",
			len(KindNames), len(declared), declared)
	}
	seen := make(map[string]string, len(declared))
	for i, name := range declared {
		k := event.Kind(i)
		wire, ok := KindNames[k]
		if !ok || wire == "" {
			t.Errorf("event.%s(=%d) 没有 wire 名 — Encode 会发出 kind:\"\" 被前端丢弃", name, i)
			continue
		}
		if prev, dup := seen[wire]; dup {
			t.Errorf("wire 名 %q 同时映射自 event.%s 和 event.%s", wire, prev, name)
		}
		seen[wire] = name
	}
}

// TestKindWireNamesAreStable 钉死已经发布出去的 kind 字符串:Plan 00 只搬家,不改协议。
func TestKindWireNamesAreStable(t *testing.T) {
	want := map[event.Kind]string{
		event.TurnStarted:       "turn_started",
		event.Reasoning:         "reasoning",
		event.Text:              "text",
		event.Message:           "message",
		event.ToolDispatch:      "tool_dispatch",
		event.ToolResult:        "tool_result",
		event.Usage:             "usage",
		event.Notice:            "notice",
		event.Phase:             "phase",
		event.ApprovalRequest:   "approval_request",
		event.AskRequest:        "ask_request",
		event.TurnDone:          "turn_done",
		event.CompactionStarted: "compaction_started",
		event.CompactionDone:    "compaction_done",
		event.ToolProgress:      "tool_progress",
		event.MCPSurfaceReady:   "mcp_surface_ready",
	}
	for k, name := range want {
		if got := KindNames[k]; got != name {
			t.Errorf("kind %d 的 wire 名 = %q, want %q", k, got, name)
		}
	}
}

// TestEncodeJSONFieldContract 钉死前端真正读的 JSON 字段名与省略规则。
func TestEncodeJSONFieldContract(t *testing.T) {
	cases := []struct {
		name string
		in   event.Event
		want string
	}{
		{
			name: "text",
			in:   event.Event{Kind: event.Text, Text: "hi"},
			want: `{"kind":"text","text":"hi"}`,
		},
		{
			name: "notice info",
			in:   event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: "n"},
			want: `{"kind":"notice","text":"n","level":"info"}`,
		},
		{
			name: "notice warn",
			in:   event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "n"},
			want: `{"kind":"notice","text":"n","level":"warn"}`,
		},
		{
			name: "tool dispatch",
			in:   event.Event{Kind: event.ToolDispatch, Tool: event.Tool{ID: "1", Name: "bash", Args: `{}`, ReadOnly: true}},
			want: `{"kind":"tool_dispatch","tool":{"id":"1","name":"bash","args":"{}","readOnly":true}}`,
		},
		{
			name: "tool progress",
			in:   event.Event{Kind: event.ToolProgress, Tool: event.Tool{ID: "1", Output: "chunk"}},
			want: `{"kind":"tool_progress","tool":{"id":"1","name":"","output":"chunk","readOnly":false}}`,
		},
		{
			name: "usage",
			in: event.Event{
				Kind:        event.Usage,
				Usage:       &provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CacheHitTokens: 8, CacheMissTokens: 2},
				SessionHit:  80,
				SessionMiss: 20,
			},
			want: `{"kind":"usage","usage":{"promptTokens":10,"completionTokens":2,"totalTokens":12,"cacheHitTokens":8,"cacheMissTokens":2,"sessionCacheHitTokens":80,"sessionCacheMissTokens":20}}`,
		},
		{
			name: "approval",
			in:   event.Event{Kind: event.ApprovalRequest, Approval: event.Approval{ID: "7", Tool: "bash", Subject: "rm"}},
			want: `{"kind":"approval_request","approval":{"id":"7","tool":"bash","subject":"rm"}}`,
		},
		{
			name: "ask",
			in: event.Event{Kind: event.AskRequest, Ask: event.Ask{
				ID:        "a1",
				Questions: []event.AskQuestion{{ID: "q1", Header: "H", Prompt: "P", Options: []event.AskOption{{Label: "A"}}}},
			}},
			want: `{"kind":"ask_request","ask":{"id":"a1","questions":[{"id":"q1","header":"H","prompt":"P","options":[{"label":"A"}]}]}}`,
		},
		{
			name: "compaction done",
			in:   event.Event{Kind: event.CompactionDone, Compaction: event.Compaction{Trigger: "auto", Messages: 3, Summary: "s", Archive: "a"}},
			want: `{"kind":"compaction_done","compaction":{"trigger":"auto","messages":3,"summary":"s","archive":"a"}}`,
		},
		{
			name: "turn done error",
			in:   event.Event{Kind: event.TurnDone, Err: errors.New("boom")},
			want: `{"kind":"turn_done","err":"boom"}`,
		},
		{
			name: "turn done ok",
			in:   event.Event{Kind: event.TurnDone},
			want: `{"kind":"turn_done"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(Encode(tc.in))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("wire JSON =\n  %s\nwant\n  %s", b, tc.want)
			}
		})
	}
}

// TestEncodeUsageCost 保留 Pricing 存在时才写 costUsd 的行为。
func TestEncodeUsageCost(t *testing.T) {
	w := Encode(event.Event{
		Kind:    event.Usage,
		Usage:   &provider.Usage{CacheHitTokens: 1_000_000},
		Pricing: &provider.Pricing{CacheHit: 1.0, Input: 2.0, Output: 10.0},
	})
	if w.Usage == nil || w.Usage.CostUSD != 1.0 {
		t.Fatalf("costUsd = %+v, want 1.0", w.Usage)
	}
}

// TestStamperMakesLossDetectable is the envelope's reason to exist: a client can
// only notice it missed frames if the stream numbers them. The sequence must be
// gap-free and start at 1, so any jump the client sees is a real gap.
func TestStamperMakesLossDetectable(t *testing.T) {
	s := NewStamper("sess", "tab-2")
	var last uint64
	for i := 0; i < 5; i++ {
		w := s.Wire(event.Event{Kind: event.Text, Text: "x"})
		if w.Sequence != last+1 {
			t.Fatalf("sequence jumped: %d after %d", w.Sequence, last)
		}
		last = w.Sequence
		if w.SchemaVersion != SchemaVersion {
			t.Errorf("schemaVersion = %d, want %d", w.SchemaVersion, SchemaVersion)
		}
		if w.SessionID != "sess" || w.TabID != "tab-2" {
			t.Errorf("stream identity missing: %+v", w)
		}
		if w.EventID == "" || w.Timestamp == "" {
			t.Errorf("envelope incomplete: %+v", w)
		}
	}
}

// TestStamperMarksQoSOnTheWire: the client is told which frames could have been
// dropped, so it can judge whether a gap it detected mattered.
func TestStamperMarksQoSOnTheWire(t *testing.T) {
	s := NewStamper("", "")
	if s.Wire(event.Event{Kind: event.Text}).Durable {
		t.Error("a text delta must be marked ephemeral")
	}
	for _, k := range []event.Kind{event.ApprovalRequest, event.AskRequest, event.TurnDone, event.ToolResult, event.Message} {
		if !s.Wire(event.Event{Kind: k}).Durable {
			t.Errorf("kind %d must be marked durable on the wire", k)
		}
	}
}

// TestEncodeLeavesTheEnvelopeEmpty pins the split: Encode produces the payload
// and nothing else, so the payload contract above compares what it claims to.
func TestEncodeLeavesTheEnvelopeEmpty(t *testing.T) {
	w := Encode(event.Event{Kind: event.TurnDone})
	if w.SchemaVersion != 0 || w.Sequence != 0 || w.EventID != "" || w.Timestamp != "" || w.Durable {
		t.Fatalf("Encode filled envelope fields; that is a Stamper's job: %+v", w)
	}
}

// TestConcurrentStamping: one stream, several emitters (the desktop's tabs share
// nothing, but a hub's publishers may not be serialised).
func TestConcurrentStamping(t *testing.T) {
	s := NewStamper("sess", "")
	const n = 200
	seqs := make(chan uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seqs <- s.Wire(event.Event{Kind: event.Text}).Sequence
		}()
	}
	wg.Wait()
	close(seqs)
	seen := map[uint64]bool{}
	for v := range seqs {
		if seen[v] {
			t.Fatalf("sequence %d handed out twice", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct sequences, want %d", len(seen), n)
	}
}
