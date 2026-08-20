// Package eventwire owns the JSON-facing event contract shared by every rich frontend.
//
// Keep the domain event package transport-agnostic: event.Event describes what happened
// inside the runtime, while this package is the single place that decides how those
// events are serialized for browsers/webviews. Desktop and HTTP/SSE transports should
// delegate here instead of maintaining parallel copies of the schema and kind mapping.
package eventwire

import "reasonix/internal/event"

// Event is the stable JSON shape an event.Event takes on the way to a rich
// frontend — the Wails webview and the SSE stream consume the identical typed
// stream, so the React client and a browser SSE client share contract types.
// The Kind enum becomes a stable string and the TurnDone error becomes a
// message, since neither serializes cleanly.
type Event struct {
	Kind       string      `json:"kind"`
	Text       string      `json:"text,omitempty"`
	Reasoning  string      `json:"reasoning,omitempty"`
	Level      string      `json:"level,omitempty"`
	Tool       *Tool       `json:"tool,omitempty"`
	Usage      *Usage      `json:"usage,omitempty"`
	Approval   *Approval   `json:"approval,omitempty"`
	Ask        *Ask        `json:"ask,omitempty"`
	Compaction *Compaction `json:"compaction,omitempty"`
	Err        string      `json:"err,omitempty"`
}

// Compaction is the JSON form of an event.Compaction. On a compaction_started
// event only Trigger is set; compaction_done carries the rest (an aborted pass
// leaves Summary empty so the frontend drops its placeholder).
type Compaction struct {
	Trigger  string `json:"trigger,omitempty"`
	Messages int    `json:"messages,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Archive  string `json:"archive,omitempty"`
}

// AskOption is one selectable answer option.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is one structured question in an ask request.
type AskQuestion struct {
	ID      string      `json:"id"`
	Header  string      `json:"header,omitempty"`
	Prompt  string      `json:"prompt"`
	Options []AskOption `json:"options"`
	Multi   bool        `json:"multi,omitempty"`
}

// Ask is the wire form of event.Ask.
type Ask struct {
	ID        string        `json:"id"`
	Questions []AskQuestion `json:"questions"`
}

// Tool is the wire form used by dispatch/result/progress events.
type Tool struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Args      string `json:"args,omitempty"`
	Output    string `json:"output,omitempty"`
	Err       string `json:"err,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
	Truncated bool   `json:"truncated,omitempty"`
	Partial   bool   `json:"partial,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
}

// Usage is the JSON telemetry exposed to rich frontends.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	CacheHitTokens   int `json:"cacheHitTokens"`
	CacheMissTokens  int `json:"cacheMissTokens"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
	// Session-cumulative cache tokens — the status line shows the aggregate
	// hit-rate Σhit/Σ(hit+miss), steadier than the single-turn CacheHitTokens.
	SessionCacheHitTokens  int     `json:"sessionCacheHitTokens"`
	SessionCacheMissTokens int     `json:"sessionCacheMissTokens"`
	CostUSD                float64 `json:"costUsd,omitempty"`
}

// Approval is the wire form of event.Approval.
type Approval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
}

// KindNames is the single domain-kind -> stable-wire-name mapping for every
// rich frontend. An unmapped Kind encodes as kind:"" and is silently dropped by
// the frontend reducer / SSE client, so a new event.Kind must be added here —
// TestKindNamesCoversEveryDeclaredKind fails until it is.
var KindNames = map[event.Kind]string{
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
	event.MCPSurfaceReady:   "mcp_surface_ready", // 漏了它 → MCP phase B 完成时前端收到 kind:"" 被丢弃(E8)
}

// EncodeAsk converts an event.Ask into its transport form.
func EncodeAsk(a event.Ask) *Ask {
	qs := make([]AskQuestion, len(a.Questions))
	for i, q := range a.Questions {
		opts := make([]AskOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = AskOption{Label: o.Label, Description: o.Description}
		}
		qs[i] = AskQuestion{
			ID: q.ID, Header: q.Header, Prompt: q.Prompt, Options: opts, Multi: q.Multi,
		}
	}
	return &Ask{ID: a.ID, Questions: qs}
}

// Encode converts one domain event into the shared JSON-facing event contract.
func Encode(e event.Event) Event {
	w := Event{Kind: KindNames[e.Kind], Text: e.Text, Reasoning: e.Reasoning}
	switch e.Kind {
	case event.Notice:
		if e.Level == event.LevelWarn {
			w.Level = "warn"
		} else {
			w.Level = "info"
		}
	case event.ToolDispatch, event.ToolResult, event.ToolProgress:
		w.Tool = &Tool{
			ID: e.Tool.ID, Name: e.Tool.Name, Args: e.Tool.Args,
			Output: e.Tool.Output, Err: e.Tool.Err,
			ReadOnly: e.Tool.ReadOnly, Truncated: e.Tool.Truncated,
			Partial: e.Tool.Partial, ParentID: e.Tool.ParentID,
		}
	case event.Usage:
		if u := e.Usage; u != nil {
			w.Usage = &Usage{
				PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
				TotalTokens: u.TotalTokens, CacheHitTokens: u.CacheHitTokens,
				CacheMissTokens: u.CacheMissTokens, ReasoningTokens: u.ReasoningTokens,
				SessionCacheHitTokens: e.SessionHit, SessionCacheMissTokens: e.SessionMiss,
			}
			if e.Pricing != nil {
				w.Usage.CostUSD = e.Pricing.Cost(u)
			}
		}
	case event.ApprovalRequest:
		w.Approval = &Approval{ID: e.Approval.ID, Tool: e.Approval.Tool, Subject: e.Approval.Subject}
	case event.AskRequest:
		w.Ask = EncodeAsk(e.Ask)
	case event.CompactionStarted, event.CompactionDone:
		w.Compaction = &Compaction{
			Trigger: e.Compaction.Trigger, Messages: e.Compaction.Messages,
			Summary: e.Compaction.Summary, Archive: e.Compaction.Archive,
		}
	case event.TurnDone:
		if e.Err != nil {
			w.Err = e.Err.Error()
		}
	}
	return w
}
