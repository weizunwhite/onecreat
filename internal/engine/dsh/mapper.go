package dsh

import (
	"encoding/json"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// RawSessionEvent 是 dsh 会话日志事件封套 { type, seq, time, data }。载荷在 data,
// 按 type 决定形状,故此处保留为 json.RawMessage 延迟解码。
type RawSessionEvent struct {
	Type string          `json:"type"`
	Seq  int             `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

// dsh 会话事件 type 常量(只列本包会映射/脱敏的)。
const (
	EvTurnStart      = "turn/start"
	EvTurnEnd        = "turn/end"
	EvAssistantChunk = "assistant/chunk"
	EvAssistantMsg   = "assistant/message"
	EvToolCall       = "tool/call"
	EvToolResult     = "tool/result"
	EvRequestHeader  = "request/header"  // 含真实 provider/model → 脱敏
	EvRequestContext = "request/context" // 含真实 provider/model → 脱敏
	EvTodoWrite      = "todo/write"
)

// Map 把一条 dsh 会话事件映射成零或多条 internal/event.Event。
//
// 网关红线:request/header 与 request/context 含真实 provider/model 名,本函数
// 一律丢弃(不产出任何事件),真实模型名绝不到达前端 sink。其它文本类事件若因某种
// dsh 版本变化仍夹带敏感串,由调用方在 scrub 后再喂 sink 兜底。
func Map(raw RawSessionEvent) []event.Event {
	switch raw.Type {
	case EvRequestHeader, EvRequestContext:
		// 结构性泄漏点:直接丢弃,不映射为任何前端事件。
		return nil

	case EvTurnStart:
		return []event.Event{{Kind: event.TurnStarted}}

	case EvTurnEnd:
		return []event.Event{{Kind: event.TurnDone}}

	case EvAssistantChunk:
		var d struct {
			Chunk struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"chunk"`
		}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return nil
		}
		switch d.Chunk.Type {
		case "text-delta":
			if d.Chunk.Text == "" {
				return nil
			}
			return []event.Event{{Kind: event.Text, Text: d.Chunk.Text}}
		case "reasoning-delta":
			if d.Chunk.Text == "" {
				return nil
			}
			return []event.Event{{Kind: event.Reasoning, Text: d.Chunk.Text}}
		default:
			return nil
		}

	case EvAssistantMsg:
		var d struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Usage *struct {
				PromptTokens     int    `json:"promptTokens"`
				CompletionTokens int    `json:"completionTokens"`
				TotalTokens      int    `json:"totalTokens"`
				CacheHitTokens   int    `json:"cacheReadTokens"`
				FinishReason     string `json:"finishReason"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return nil
		}
		var text, reasoning string
		for _, c := range d.Message.Content {
			switch c.Type {
			case "text":
				text += c.Text
			case "reasoning":
				reasoning += c.Text
			}
		}
		out := []event.Event{{Kind: event.Message, Text: text, Reasoning: reasoning}}
		if d.Usage != nil {
			out = append(out, event.Event{Kind: event.Usage, Usage: &provider.Usage{
				PromptTokens:     d.Usage.PromptTokens,
				CompletionTokens: d.Usage.CompletionTokens,
				TotalTokens:      d.Usage.TotalTokens,
				CacheHitTokens:   d.Usage.CacheHitTokens,
				FinishReason:     d.Usage.FinishReason,
			}})
		}
		return out

	case EvToolCall:
		var d struct {
			CallID    string `json:"callId"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return nil
		}
		return []event.Event{{Kind: event.ToolDispatch, Tool: event.Tool{
			ID: d.CallID, Name: d.Name, Args: d.Arguments,
		}}}

	case EvToolResult:
		var d struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Error *struct {
				Name string `json:"name"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return nil
		}
		var out string
		for _, c := range d.Message.Content {
			if c.Type == "text" {
				out += c.Text
			}
		}
		ev := event.Event{Kind: event.ToolResult, Tool: event.Tool{Output: out}}
		if d.Error != nil {
			ev.Tool.Err = d.Error.Name + ": " + d.Error.Code
		}
		return []event.Event{ev}

	default:
		// 其它 durable 事件(step/*、todo/write、compaction/* 等)本 spike 暂不映射。
		return nil
	}
}
