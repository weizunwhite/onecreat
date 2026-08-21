// Package dsh 是 OneCreat 的 dsh(DeepSeek Harness)sidecar 引擎的驱动层(spike)。
//
// 它把一个 dsh 运行时当子进程拉起,用 newline-delimited JSON-RPC 2.0 over stdio
// 驱动(对齐 dsh 的 @deepseek-ai/dsh-sdk-jsonrpc-server 协议,dsh 版本 0.1.0-rc.7)。
// dsh 把每一条 durable 会话事实以 session.event 通知全量推回来,本包把它们映射成
// internal/event.Event 喂给现有前端 sink。
//
// 网关红线:dsh 会把真实 provider/model 名写进 request/header / request/context
// 事件。本包在映射前必须经 scrub.go 擦除,绝不让真实模型名到达 UI/日志。
//
// 现状:协议客户端、进程守护、事件映射、脱敏均已实现并有单测;adapter.go 已把它接到
// `engine.TurnEngine` 边界上(Plan 12)。但**这个引擎目前不允许被启用** ——
// `boot.selectEngine` 对 `engine = "dsh"` fail-closed,因为它的工具跑在自己的进程里,
// OneCreat 的权限门 / plan mode / PreToolUse hook / 写前检查点 / 证据链都够不着
// (复核 AR-R01)。在 dsh 协议支持把工具调用委托回 OneCreat 执行之前,这里的代码是
// 已接线但未放行的状态。
package dsh

// dsh SDK JSON-RPC 方法名(client→server 请求)。
const (
	MethodInitialize    = "initialize"
	MethodSessionPrompt = "session/prompt"
	MethodShutdown      = "shutdown"
)

// dsh SDK JSON-RPC 通知名(server→client)。
const (
	NotifySessionEvent     = "session.event"
	NotifySessionStatus    = "session.status"
	NotifySubagentStarted  = "subagent.started"
	NotifySubagentFinished = "subagent.finished"
)

// WireServerName 是 dsh SDK 运行时的稳定身份(initialize 结果里的 serverInfo.name)。
const WireServerName = "deepseek-harness-sdk-runtime"

// InitializeParams 是进程级握手参数。model 应下发档位占位符,绝不填真实模型名。
type InitializeParams struct {
	CWD       string `json:"cwd"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	MaxTokens int    `json:"maxTokens,omitempty"`
}

// InitializeResult 是握手返回的服务器身份。
type InitializeResult struct {
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// ContentBlock 是 dsh 的内容块(此处只需 text 类型,其它透传保留原始 JSON)。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextBlock 便捷构造一个 text 内容块。
func TextBlock(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// SessionPromptParams 是一次 user turn 的入队参数。
type SessionPromptParams struct {
	SessionID     string         `json:"sessionId"`
	ContentBlocks []ContentBlock `json:"contentBlocks"`
}

// SessionPromptResult 是 prompt 的入队回执(仅标识入队,不代表某条 assistant 回复)。
type SessionPromptResult struct {
	MessageID string `json:"messageId"`
}

// SessionEventNotification 是 session.event 通知:一条会话日志事件,记录即推。
// Event 是完整的 dsh 会话日志事件封套(type + 载荷),此处保留原始 JSON 以便映射。
type SessionEventNotification struct {
	SessionID string          `json:"sessionId"`
	Event     RawSessionEvent `json:"event"`
}

// SessionStatusNotification 是整 agent 生命周期状态(idle / running)。
type SessionStatusNotification struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
}
