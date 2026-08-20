// Package engine is the boundary between OneCreat's application policies and
// whatever actually runs a turn.
//
// 为什么要有这一层(A14):OneCreat 的产品逻辑 —— 记忆、证据链、权限、计费、检查点
// —— 全都长在 `control.Controller` 上,而真正「跑一轮」的东西是可替换的:内置 Go
// 内核是一种,dsh sidecar 是另一种。在此之前这条缝是 `agent.Runner`,由**内置引擎
// 自己的包**定义 —— 于是任何第二种引擎都得 import `internal/agent`,再顺着它把
// provider / tool / session 一路拖进来。那正是「长出第二套 OneCreat」的起点。
//
// 这一层刻意小到极致,而且**必须**保持这么小:
//
//	TurnEngine —— 一个方法:开始一轮。
//	TurnHandle —— 取消它、等它结束。
//
// 明确【不】放进来的东西(A14 点名的反模式):Approve / PendingApprovals /
// History / Resume / Fork / Rewind / SetPlanMode / NewSession / SessionPath。
// 这些是**应用策略**,不是引擎能力 —— 把它们塞进 TurnEngine,等于把整个
// Controller API 改个名叫 engineBackend,一行架构也没改善。`engine_boundary_test.go`
// 用 AST 把这条约束钉死。
//
// 引擎之间能力不同(dsh 没有 mid-turn 取消、不共享会话日志),用 Capability 声明,
// 不用往接口上加方法。
package engine

import "context"

// TurnRequest 是一轮的输入。它是结构体而不是裸 string,只是为了以后加字段时不必
// 改接口签名 —— 不是给它长成「第二个 Controller 参数包」的许可。
type TurnRequest struct {
	// Input 是已经由应用策略组装完毕的那段文本(@ 引用已展开、Compose 已注入)。
	// 引擎拿到它时不再需要知道这些策略存在。
	Input string
}

// TurnEngine 开始一轮。这就是引擎边界的全部。
//
// Start 只负责「开始」并立刻返回句柄,不阻塞到跑完 —— dsh 那种 sidecar 引擎本来
// 就是异步的(submit 之后靠通知流推进),让接口形状迁就同步实现会逼着适配器造一个
// 假的阻塞语义。事件在此期间流向引擎构造时绑定的 sink。
type TurnEngine interface {
	Start(ctx context.Context, req TurnRequest) (TurnHandle, error)
}

// TurnHandle 是一轮进行中的句柄。
type TurnHandle interface {
	// Cancel 请求中止这一轮。允许是尽力而为:dsh 目前没有 mid-turn 取消 RPC,
	// 它的 Cancel 会降级为关掉 sidecar 进程(见 dsh.Engine.Cancel 的注释)。
	Cancel() error
	// Wait 阻塞到这一轮结束,返回该轮的错误。
	//
	// 计划文档里的接口草图只有 Cancel,没有任何「跑完了」的信号 —— 但现有的
	// `agent.Runner.Run` 是同步的,Controller 在它返回之后才收尾(Stop hook、
	// plan mode 审批门、会话落盘)。没有完成信号就无法表达既有语义,所以这里
	// 补上 Wait,并且只补这一个。
	Wait(ctx context.Context) error
}

// Capability 是引擎的可选能力。默认**没有** —— 不声明就是不支持,失败方向朝安全。
type Capability string

const (
	// CapStreaming:增量输出(文本 / 推理 / 工具进度)会流向 sink,而不是只在
	// 结束时给一坨。
	CapStreaming Capability = "streaming"
	// CapApproval:引擎会在执行工具前停下来等审批。dsh 在自己的进程里跑自己的
	// 工具,OneCreat 的权限门根本够不着 —— 所以它不声明这一条。
	CapApproval Capability = "approval"
	// CapResume:引擎的会话状态可以从 OneCreat 侧的消息日志恢复。
	CapResume Capability = "resume"
	// CapFork:可以从历史某一点分叉出一条新会话(rewind / branch 依赖它)。
	CapFork Capability = "fork"
)

// Capable 是可选的能力声明接口。引擎实现它来自报能力;不实现的引擎一律按「什么
// 可选能力都没有」处理。
type Capable interface {
	Supports(Capability) bool
}

// Supports 报告 e 是否支持 c。没实现 Capable 的引擎返回 false —— 未声明即不支持。
func Supports(e TurnEngine, c Capability) bool {
	cc, ok := e.(Capable)
	return ok && cc.Supports(c)
}

// Set 是一组能力,给适配器当 Capable 的现成实现用。
type Set map[Capability]bool

// Supports 让 Set 自己就能当 Capable 用。
func (s Set) Supports(c Capability) bool { return s[c] }

// Names 按固定顺序返回已声明的能力,供诊断输出使用(map 迭代序不稳定,直接打印
// 会让日志和测试都抖)。
func (s Set) Names() []string {
	var out []string
	for _, c := range []Capability{CapStreaming, CapApproval, CapResume, CapFork} {
		if s[c] {
			out = append(out, string(c))
		}
	}
	return out
}
