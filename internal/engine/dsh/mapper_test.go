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
	res := Map(raw(t, EvToolResult, map[string]any{
		"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "Sketch uses 296090 bytes"}}},
	}))
	if len(res) != 1 || res[0].Kind != event.ToolResult || res[0].Tool.Output == "" {
		t.Fatalf("tool/result 映射错误: %+v", res)
	}
}

func TestMapUsage(t *testing.T) {
	evs := Map(raw(t, EvAssistantMsg, map[string]any{
		"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "done"}}},
		"usage":   map[string]any{"promptTokens": 100, "completionTokens": 20, "totalTokens": 120, "finishReason": "stop"},
	}))
	// 期望:Message + Usage 两条。
	if len(evs) != 2 || evs[0].Kind != event.Message || evs[1].Kind != event.Usage {
		t.Fatalf("assistant/message+usage 映射错误: %+v", evs)
	}
	if evs[1].Usage == nil || evs[1].Usage.PromptTokens != 100 {
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
