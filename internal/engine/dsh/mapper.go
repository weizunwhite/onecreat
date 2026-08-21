package dsh

import (
	"encoding/json"
	"strings"

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

// durableStatePrefixes 是 dsh 的**状态**事件族 —— 它们描述的不是流式文本,而是这一轮
// 做了什么(待办清单、步骤进度、上下文压缩)。
//
// 这些还没有可核实的映射:OneCreat 侧的 todo / 证据链由自己的工具写,而 dsh 在别的
// 进程里跑自己的工具;compaction 的载荷形状本仓库也无从核对。**但"不知道怎么映射"
// 不等于可以当没发生**(复核 AR-R08.3:「不得默认静默丢弃 todo/step/compaction 状态」)。
// 用户看着模型写了一份计划、界面上什么都没有,是这里最坏的失败形态 —— 它不报错。
//
// 所以降级成一条带类型名的 Notice:信息量不足,但至少是**可见**的,而且等上游形状
// 确定后,把某一族换成真映射就是在下面加一个 case。
var durableStatePrefixes = []string{"todo/", "step/", "compaction/"}

func isDurableState(typ string) bool {
	for _, p := range durableStatePrefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

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
			// callId / name:结果必须能对回是哪一次调用。一轮里可以有多个并行工具
			// 调用,而结果可能乱序到达 —— 少了这两个字段,前端只能按到达顺序猜,猜错
			// 会把一个失败的结果挂到别的调用卡片上(复核 AR-R08.2)。
			CallID  string `json:"callId"`
			Name    string `json:"name"`
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
		ev := event.Event{Kind: event.ToolResult, Tool: event.Tool{
			ID: d.CallID, Name: d.Name, Output: out,
		}}
		if d.Error != nil {
			ev.Tool.Err = d.Error.Name + ": " + d.Error.Code
		}
		return []event.Event{ev}

	default:
		// 状态事件降级成一条可见的通知,而不是悄悄消失(AR-R08.3)。
		if isDurableState(raw.Type) {
			return []event.Event{{
				Kind:  event.Notice,
				Level: event.LevelInfo,
				Text:  "dsh 状态事件 " + raw.Type + " 暂未映射为 OneCreat 事件(内容未展示)",
			}}
		}
		// 其余未知类型:可能是高频的新增流式事件,不往 UI 上灌通知。
		return nil
	}
}
