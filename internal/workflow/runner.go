package workflow

import (
	"context"
	"time"
)

// —— 材料作业与「怎么跑一轮会话」之间的唯一接缝 ——
//
// 合同见 docs/科创工作流迁移/06_M2执行方案.md §2:领域层(本包)只知道"给我一段
// prompt,在这个项目目录里跑一轮,告诉我最后说了什么、写了哪些文件";**怎么跑**
// ——装配 Controller、选引擎、配材料作业专用门禁、收产物——全在 internal/cli 的
// ControllerRunner 里。所以本包不 import boot / control / engine / agent,
// 领域层的架构铁律(00_架构规划_v1.md §8.0 A1)才成立。
//
// 反过来也是这个接缝让作业层可测:单测塞一个 FakeRunner 就能把依赖拒绝、重试、
// 假成功检测、落桶校验全跑一遍,不花一分钱、不起一个子进程。

// RunRequest 是跑一轮材料作业需要的全部输入。
type RunRequest struct {
	// ProjectDir 是项目目录绝对路径,也就是这一轮的工作区:文件工具、bash、
	// 落桶校验全部相对它。
	ProjectDir string
	// MaterialType 是注册表 materials[].type,如 "技术方案"。只用于记账与事件,
	// 执行侧不按它改行为。
	MaterialType string
	// Prompt 是装配好的完整提示词(skill 正文 + 依赖产物 + 七目录说明 + 落桶指令)。
	Prompt string
	// Timeout 是这一轮的墙钟上限,<=0 表示不设限(由调用方的 ctx 兜底)。
	Timeout time.Duration
}

// RunResult 是一轮跑完之后领域层要的两件事实。
type RunResult struct {
	// FinalText 是模型最后一段可见回复(不含 reasoning)。
	FinalText string
	// WrittenFiles 是这一轮**成功**写过的文件,项目根下的相对路径、去重升序。
	// 假成功检测与落桶校验都吃它,所以它必须来自宿主观测到的真实工具结果,
	// 而不是模型自述。
	WrittenFiles []string
}

// Runner 跑一轮材料作业。实现见 internal/cli.ControllerRunner。
type Runner interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}
