package dsh

import (
	"encoding/json"
	"testing"

	"reasonix/internal/event"
)

func raw(t *testing.T, typ string, data any) RawSessionEvent {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return RawSessionEvent{Type: typ, Data: b}
}

func TestMapTextDelta(t *testing.T) {
	evs := Map(raw(t, EvAssistantChunk, map[string]any{
		"chunk": map[string]string{"type": "text-delta", "text": "hello"},
	}))
	if len(evs) != 1 || evs[0].Kind != event.Text || evs[0].Text != "hello" {
		t.Fatalf("text-delta 映射错误: %+v", evs)
	}
}

func TestMapReasoningDelta(t *testing.T) {
	evs := Map(raw(t, EvAssistantChunk, map[string]any{
		"chunk": map[string]string{"type": "reasoning-delta", "text": "think"},
	}))
	if len(evs) != 1 || evs[0].Kind != event.Reasoning || evs[0].Text != "think" {
		t.Fatalf("reasoning-delta 映射错误: %+v", evs)
	}
}

func TestMapToolCallAndResult(t *testing.T) {
	call := Map(raw(t, EvToolCall, map[string]any{
		"callId": "c1", "name": "mcp__hardware__arduino_compile", "arguments": `{"fqbn":"esp32:esp32:esp32"}`,
	}))
	if len(call) != 1 || call[0].Kind != event.ToolDispatch || call[0].Tool.Name != "mcp__hardware__arduino_compile" {
		t.Fatalf("tool/call 映射错误: %+v", call)
	}
	// tool/result 的真实形状:message.content = [ToolResultBlock],块里带 toolCallId
	// 与嵌套的 content 文本块(对齐 dsh 0.1.0-rc.8 的 llm 类型)。
	res := Map(raw(t, EvToolResult, map[string]any{
		"message": map[string]any{"content": []map[string]any{{
			"type":       "tool-result",
			"toolCallId": "c1",
			"content":    []map[string]string{{"type": "text", "text": "Sketch uses 296090 bytes"}},
		}}},
	}))
	if len(res) != 1 || res[0].Kind != event.ToolResult || res[0].Tool.Output == "" {
		t.Fatalf("tool/result 映射错误: %+v", res)
	}
	if res[0].Tool.ID != "c1" {
		t.Fatalf("tool/result 应带回 callId 以便与 tool/call 配对: %+v", res[0].Tool)
	}
}

func TestMapUsage(t *testing.T) {
	evs := Map(raw(t, EvAssistantMsg, map[string]any{
		"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "done"}}},
		// dsh 的 TokenUsage 字段是 inputTokens/outputTokens/cacheReadTokens,
		// 且 inputTokens 只算未命中缓存的部分(计费输入 = 三者之和)。
		"usage": map[string]any{"inputTokens": 40, "outputTokens": 20, "cacheReadTokens": 60},
	}))
	// 期望:Message + Usage 两条。
	if len(evs) != 2 || evs[0].Kind != event.Message || evs[1].Kind != event.Usage {
		t.Fatalf("assistant/message+usage 映射错误: %+v", evs)
	}
	if evs[1].Usage == nil || evs[1].Usage.PromptTokens != 100 || evs[1].Usage.CacheHitTokens != 60 || evs[1].Usage.CompletionTokens != 20 {
		t.Fatalf("usage 载荷错误: %+v", evs[1].Usage)
	}
}

// 网关红线:request/header 与 request/context 必须被丢弃,绝不产出事件。
func TestMapDropsModelLeakEvents(t *testing.T) {
	for _, typ := range []string{EvRequestHeader, EvRequestContext} {
		evs := Map(raw(t, typ, map[string]any{
			"header":   map[string]any{"provider": "deepseek-official", "model": "deepseek-v4-pro"},
			"provider": "deepseek-official", "model": "deepseek-v4-pro",
		}))
		if len(evs) != 0 {
			t.Fatalf("%s 应被丢弃(防模型名泄漏),却产出了 %+v", typ, evs)
		}
	}
}
