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

// dsh 会话事件 type 常量(只列本包会映射/脱敏/喂证据的)。
const (
	EvTurnStart      = "turn/start"
	EvTurnEnd        = "turn/end"
	EvAssistantChunk = "assistant/chunk"
	EvAssistantMsg   = "assistant/message"
	EvUserMsg        = "user/message"
	EvToolCall       = "tool/call"
	EvToolResult     = "tool/result"
	EvRequestHeader  = "request/header"  // 含真实 provider/model → 丢弃
	EvRequestContext = "request/context" // 含真实 provider/model → 丢弃
	EvTodoWrite      = "todo/write"
	EvCompactStart   = "compaction/start"
	EvCompactEnd     = "compaction/end"
)

// contentBlock 是 dsh 的内容块(只解本包用得到的字段)。
type contentBlock struct {
	Type       string         `json:"type"`
	Text       string         `json:"text"`
	ToolCallID string         `json:"toolCallId"`
	Content    []contentBlock `json:"content"`
	IsError    bool           `json:"isError"`
}

// blocksText 把内容块里的 text 拼起来(嵌套的 tool-result 也递归拼)。
func blocksText(blocks []contentBlock) string {
	out := ""
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out += b.Text
		case "tool-result":
			out += blocksText(b.Content)
		}
	}
	return out
}

// ToolResultInfo 是从 tool/result 事件解出的结构化结果,供证据引擎与事件映射共用。
type ToolResultInfo struct {
	CallID  string
	Output  string
	IsError bool
	ErrName string
}

// ParseToolResult 解一条 tool/result 事件。ok=false 表示载荷不是预期形状。
func ParseToolResult(raw RawSessionEvent) (ToolResultInfo, bool) {
	var d struct {
		Message struct {
			Content []contentBlock `json:"content"`
		} `json:"message"`
		Error *struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw.Data, &d); err != nil {
		return ToolResultInfo{}, false
	}
	info := ToolResultInfo{}
	for _, b := range d.Message.Content {
		if b.Type == "tool-result" {
			info.CallID = b.ToolCallID
			info.Output += blocksText(b.Content)
			if b.IsError {
				info.IsError = true
			}
		}
	}
	if d.Error != nil {
		info.IsError = true
		info.ErrName = d.Error.Name + ": " + d.Error.Code
	}
	return info, true
}

// ToolCallInfo 是从 tool/call 事件解出的调用信息。
type ToolCallInfo struct {
	CallID string
	Name   string
	Args   string
}

// ParseToolCall 解一条 tool/call 事件。
func ParseToolCall(raw RawSessionEvent) (ToolCallInfo, bool) {
	var d struct {
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(raw.Data, &d); err != nil {
		return ToolCallInfo{}, false
	}
	return ToolCallInfo{CallID: d.CallID, Name: d.Name, Args: d.Arguments}, true
}

// ParseTodos 解一条 todo/write 事件的 todos 数组(原始 JSON,交给证据引擎解)。
func ParseTodos(raw RawSessionEvent) (json.RawMessage, bool) {
	var d struct {
		Todos json.RawMessage `json:"todos"`
	}
	if err := json.Unmarshal(raw.Data, &d); err != nil || len(d.Todos) == 0 {
		return nil, false
	}
	return d.Todos, true
}

// ParseTurnFailure 从 turn/end 事件里解出"这一轮为什么失败"的人话描述。
// ok=false 表示这一轮是正常结束(completed / aborted 等,不需要报错)。
func ParseTurnFailure(raw RawSessionEvent) (string, bool) {
	var d struct {
		Reason struct {
			Kind  string `json:"kind"`
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(raw.Data, &d); err != nil {
		return "", false
	}
	switch d.Reason.Kind {
	case "error":
		msg := "模型请求失败"
		if d.Reason.Error != nil {
			if d.Reason.Error.Message != "" {
				msg = d.Reason.Error.Message
			}
			if d.Reason.Error.Code != "" {
				msg = d.Reason.Error.Code + ": " + msg
			}
		}
		return msg, true
	case "blocked":
		return "这一轮被策略拦下(blocked)", true
	case "max-tokens":
		return "这一轮达到输出 token 上限,回答可能被截断", true
	default:
		return "", false
	}
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
		// TurnDone 由 Engine.Run 在整轮收敛后统一发(它才知道有没有错),这里不发,
		// 否则一轮里多次 turn/end 会让前端反复"收尾"。
		//
		// 但**失败必须说出来**:dsh 的 agent/error 是 agent 事件、不上 SDK wire,
		// turn/end 的 reason 是我们唯一能看到失败的地方。不映射它,一次 401/网络错
		// 就表现为"什么都没发生"(实测过)。
		if fail, ok := ParseTurnFailure(raw); ok {
			return []event.Event{{Kind: event.Notice, Level: event.LevelWarn, Text: fail}}
		}
		return nil

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
				Content []contentBlock `json:"content"`
			} `json:"message"`
			// dsh 的 TokenUsage:inputTokens 是「未命中缓存的输入」,命中的单列
			// cacheReadTokens(计费输入 = 三者之和),与我们 provider.Usage 的
			// PromptTokens(总输入)语义不同,这里换算回来。
			Usage *struct {
				InputTokens      int `json:"inputTokens"`
				OutputTokens     int `json:"outputTokens"`
				CacheReadTokens  int `json:"cacheReadTokens"`
				CacheWriteTokens int `json:"cacheWriteTokens"`
				ReasoningTokens  int `json:"reasoningTokens"`
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
			prompt := d.Usage.InputTokens + d.Usage.CacheReadTokens + d.Usage.CacheWriteTokens
			out = append(out, event.Event{Kind: event.Usage, Usage: &provider.Usage{
				PromptTokens:     prompt,
				CompletionTokens: d.Usage.OutputTokens,
				TotalTokens:      prompt + d.Usage.OutputTokens,
				CacheHitTokens:   d.Usage.CacheReadTokens,
				CacheMissTokens:  d.Usage.InputTokens,
				ReasoningTokens:  d.Usage.ReasoningTokens,
			}})
		}
		return out

	case EvToolCall:
		info, ok := ParseToolCall(raw)
		if !ok {
			return nil
		}
		return []event.Event{{Kind: event.ToolDispatch, Tool: event.Tool{
			ID: info.CallID, Name: info.Name, Args: info.Args,
		}}}

	case EvToolResult:
		info, ok := ParseToolResult(raw)
		if !ok {
			return nil
		}
		ev := event.Event{Kind: event.ToolResult, Tool: event.Tool{ID: info.CallID, Output: info.Output}}
		if info.ErrName != "" {
			ev.Tool.Err = info.ErrName
		} else if info.IsError {
			ev.Tool.Err = "tool failed"
		}
		return []event.Event{ev}

	default:
		// 其它 durable 事件(step/*、todo/write、compaction/* 等)不映射成前端事件;
		// todo/write 由 Engine 单独消费喂证据引擎。
		return nil
	}
}
