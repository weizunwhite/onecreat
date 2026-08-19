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
// 现状(spike):协议客户端、进程守护、事件映射、脱敏均已实现并有单测;尚未接进
// control.Controller(见 docs/dsh调研/04_Phase1_spike报告.md 的收尾清单)。
package dsh

// dsh SDK JSON-RPC 方法名(client→server 请求)。
const (
	MethodInitialize    = "initialize"
	MethodSessionPrompt = "session/prompt"
	MethodShutdown      = "shutdown"
)

// OneCreat 自己补在同一条 wire 上的方法(由 dsh/plugins/control 实现)。
// 官方 SDK server 只有上面三个方法,没有取消/审批/计划模式/resume(见
// docs/dsh调研/01 §6 的 wire 缺口),这些是我们自己补的。
const (
	MethodSessionCancel  = "onecreat/session.cancel"
	MethodPlanModeSet    = "onecreat/planMode.set"
	MethodInject         = "onecreat/inject"
	MethodSessionLoad    = "onecreat/session.load"
	MethodSessionHistory = "onecreat/session.history"
	// MethodCredentialsSet 轮换子进程里的连接凭证(平台 token 约 50 分钟过期,
	// 而子进程环境是 spawn 时的快照)。只更新非空字段,值绝不落盘、不打印。
	MethodCredentialsSet = "onecreat/credentials.set"
)

// GatewayProviderRoute 是 OneCreat 自命名的 provider 路由名。真实厂商路由名
// (dsh 内置的那个)绝不出现在本仓库的 wire、日志与 UI 里 —— 见 dsh/plugins/gateway。
const GatewayProviderRoute = "onecreat-gateway"

// dsh SDK JSON-RPC 通知名(server→client)。
const (
	NotifySessionEvent     = "session.event"
	NotifySessionStatus    = "session.status"
	NotifySubagentStarted  = "subagent.started"
	NotifySubagentFinished = "subagent.finished"
)

// OneCreat 桥接通知。每条出站通知带一个 id,Go 侧用同一个 id 回一条应答通知。
// (用通知对而不是 server→client 请求,是为了让 Go 侧的 LineClient 保持只需
// 处理"响应 + 通知"两种入站帧。)
const (
	NotifyApprovalRequest    = "onecreat/approval.request"     // dsh → Go
	NotifyApprovalResolve    = "onecreat/approval.resolve"     // Go → dsh
	NotifyToolInvoke         = "onecreat/tool.invoke"          // dsh → Go
	NotifyToolResult         = "onecreat/tool.result"          // Go → dsh
	NotifyToolPreExecute     = "onecreat/tool.preExecute"      // dsh → Go
	NotifyToolPreExecuteDone = "onecreat/tool.preExecute.done" // Go → dsh
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

// SessionRefParams 是只带会话 id 的参数(cancel / load / history 共用)。
type SessionRefParams struct {
	SessionID string `json:"sessionId"`
}

// PlanModeParams 是 onecreat/planMode.set 的参数。
type PlanModeParams struct {
	SessionID string `json:"sessionId"`
	Active    bool   `json:"active"`
}

// InjectParams 是 onecreat/inject 的参数(每轮运行时状态,进 pre-step 而非系统提示)。
type InjectParams struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

// CredentialsParams 是 onecreat/credentials.set 的参数:只带需要更新的字段,
// 空字段表示"不动"。
type CredentialsParams struct {
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseURL,omitempty"`
}

// WireMessage 是会话消息投影里的一条(只有角色与文本,不含任何 provider/model 信息)。
type WireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SessionLoadResult 是 onecreat/session.load / session.history 的返回。
type SessionLoadResult struct {
	Messages []WireMessage `json:"messages"`
}

// ApprovalRequestNotification 是 dsh 侧发来的审批请求。
type ApprovalRequestNotification struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	ToolName  string `json:"toolName"`
	CallID    string `json:"callId"`
	Reason    string `json:"reason"`
}

// ApprovalResolveNotification 是 Go 侧的审批答复。
type ApprovalResolveNotification struct {
	ID    string `json:"id"`
	Allow bool   `json:"allow"`
}

// ToolInvokeNotification 是 dsh 侧请求 Go 执行一个内置工具(工具桥)。
type ToolInvokeNotification struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResultNotification 是 Go 侧的工具执行结果。
type ToolResultNotification struct {
	ID     string `json:"id"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

// ToolPreExecuteNotification 是 dsh 侧在执行工具前给 Go 的快照机会。
type ToolPreExecuteNotification struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// AckNotification 是只带 id 的应答通知。
type AckNotification struct {
	ID string `json:"id"`
}

// PreExecuteDecision 是 Go 侧对一次工具预执行的裁定:allow / ask 后的结果 / deny。
type PreExecuteDecision struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
