package cli

// 科创工作流「材料作业 = 一轮会话」的装配半边(M2 · W2)。
//
// 领域层(internal/workflow)只认一个接缝:workflow.Runner —— "给我一段 prompt,
// 在这个项目目录里跑一轮,告诉我最后说了什么、写了哪些文件"。本文件是它在生产
// 环境里的唯一实现:装 Controller、选引擎、换上材料作业专用门禁、收产物、收口。
//
// 三件事刻意都放在这一层,而不是领域层:
//  1. **装配**(boot.Build)——领域层不许 import boot/control/engine(架构铁律 A1);
//  2. **门禁口径**——材料作业没有 ask,只有 allow/deny(M2 拍板 1),这是"这类会话
//     是什么"的判断,属于装配层;
//  3. **产物采集**——WrittenFiles 必须来自宿主观测到的真实工具结果,而不是模型自述;
//     宿主观测点就是事件流(ToolResult),它只在这一层看得见。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/boot"
	"reasonix/internal/event"
	"reasonix/internal/workflow"
	"reasonix/internal/workspace"
)

// ControllerRunner 用一个真 Controller 跑一轮材料作业,实现 workflow.Runner。
//
// 每一轮都是**独立的一次装配**:一个项目目录 = 一个工作区 = 一条会话,跑完就 Close
// (释放这条会话的 MCP 子进程)。不共享 Factory —— CLI 是单工作区语义,一个进程里
// 一次只跑一个项目,私有 Factory 正是 boot 为这种前端准备的默认。
type ControllerRunner struct {
	// Engine 是回合引擎名:""/"native" 走内置 Go 内核,"dsh" 走 sidecar。
	Engine string
	// Model 覆盖配置里的 default_model;空串用配置默认。
	Model string
	// MaxSteps 覆盖单轮最大工具轮数;0 用配置默认。
	MaxSteps int
	// Sink 是上层想看的事件流(CLI 渲染到 stdout)。nil = 不渲染,只采集。
	Sink event.Sink
}

// 编译期确认它确实满足领域层的合同。
var _ workflow.Runner = (*ControllerRunner)(nil)

// Run 这个函数做什么:在 req.ProjectDir 里跑一轮会话,把 req.Prompt 提交给模型,
// 等这一轮结束,返回最后的可见回复与本轮真实写过的文件。
//
// 超时:req.Timeout > 0 时给这一轮加墙钟上限,到点取消 ctx —— Controller.Run 随
// ctx 结束,TurnHandle.Cancel 负责通知那些不会随 ctx 一起死的引擎(dsh 是独立进程)。
// 取消不是"放弃收口":已经写出去的文件照样出现在 WrittenFiles 里,由作业层判断这轮
// 算不算成功。
func (r *ControllerRunner) Run(ctx context.Context, req workflow.RunRequest) (workflow.RunResult, error) {
	root, err := filepath.Abs(strings.TrimSpace(req.ProjectDir))
	if err != nil {
		return workflow.RunResult{}, fmt.Errorf("项目目录路径不合法:%w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return workflow.RunResult{}, fmt.Errorf("项目目录不可用:%s", root)
	}
	ws, err := workspace.New(root)
	if err != nil {
		return workflow.RunResult{}, fmt.Errorf("打开工作区失败:%w", err)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return workflow.RunResult{}, errors.New("prompt 为空,不跑空轮")
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	col := &materialCollector{inner: r.Sink, root: ws.Root()}
	col.emitPhase(event.Workflow{Material: req.MaterialType, Phase: workflowPhaseStarted})

	ctrl, err := boot.Build(ctx, boot.Options{
		Model:      r.Model,
		MaxSteps:   r.MaxSteps,
		RequireKey: true,
		Sink:       col,
		Engine:     r.Engine,
		Workspace:  ws,
		// 材料作业专用门禁:allow/deny 二值,没有 ask(M2 拍板 1)。
		Gate: newMaterialGate(ws.Root()),
		// Factory 留 nil:CLI 单工作区,私有 Factory 随会话一起关。
	})
	if err != nil {
		col.emitPhase(event.Workflow{
			Material: req.MaterialType, Phase: workflowPhaseFailed, Detail: "装配失败:" + err.Error(),
		})
		return workflow.RunResult{}, fmt.Errorf("装配会话失败:%w", err)
	}
	// Close 会跑 SessionEnd 钩子并杀掉这条会话的 MCP 子进程 —— 漏了它,批量跑十种
	// 材料就是十组残留子进程。
	defer ctrl.Close()

	runErr := ctrl.Run(ctx, req.Prompt)

	res := workflow.RunResult{FinalText: col.finalText(), WrittenFiles: col.written()}
	if runErr == nil && ctx.Err() != nil {
		// 轮次被取消/超时的时候 Controller.Run 不把它当错误(用户主动取消是正常结束),
		// 但对材料作业来说"没跑完"就是没跑完,必须让作业层知道。
		runErr = ctx.Err()
	}
	if runErr != nil {
		detail := runErr.Error()
		if errors.Is(runErr, context.DeadlineExceeded) && req.Timeout > 0 {
			detail = fmt.Sprintf("超时 %s", req.Timeout)
			runErr = fmt.Errorf("材料作业超时(%s):%w", req.Timeout, runErr)
		}
		col.emitPhase(event.Workflow{Material: req.MaterialType, Phase: workflowPhaseFailed, Detail: detail})
		return res, runErr
	}
	col.emitPhase(event.Workflow{
		Material: req.MaterialType, Phase: workflowPhaseSucceeded,
		Detail: fmt.Sprintf("写入 %d 个文件", len(res.WrittenFiles)),
	})
	return res, nil
}

// 材料作业事件的三个阶段,与 journal 的动词同源(material_started/succeeded/failed)。
const (
	workflowPhaseStarted   = "started"
	workflowPhaseSucceeded = "succeeded"
	workflowPhaseFailed    = "failed"
)

// ---------- 产物采集 ----------

// materialCollector 是这一轮的事件旁路:把事件透给上层的同时,记下两件事实——
// 本轮**成功**写过哪些文件、模型最后说了什么。
//
// 为什么从事件流采而不是从 evidence.Ledger 采:Ledger 是 toolpolicy 流水线的私产,
// 装配层拿不到、也不该伸手进去拿;而 ToolResult 事件带着同一份事实(工具名 + 原始
// args + 是否出错),两条引擎(native / dsh)都发同一份。采集口径与 evidence 的
// isWriterTool/extractPaths 保持一致,见下面两张表。
//
// 并发:event.Sink 的合同是"由运行循环串行调用",所以这里不加锁;Run 在轮次结束
// 之后才读,天然 happens-after。
type materialCollector struct {
	inner event.Sink
	root  string

	files  map[string]bool
	last   string          // 最近一条 Message 的完整文本
	deltas strings.Builder // Message 没来时的兜底:累积的 Text 增量
}

func (c *materialCollector) Emit(e event.Event) {
	switch e.Kind {
	case event.ToolResult:
		// 只认成功的写工具:被门禁拒掉的调用 Err 非空,绝不能算进产物。
		if e.Tool.Err == "" && materialWriterTools[e.Tool.Name] {
			c.collectPaths(e.Tool.Args)
		}
	case event.Message:
		c.last = e.Text
	case event.Text:
		c.deltas.WriteString(e.Text)
	}
	if c.inner != nil {
		c.inner.Emit(e)
	}
}

// emitPhase 发一帧 workflow_material,并让它也走一遍上层 sink(CLI 可以不渲染)。
func (c *materialCollector) emitPhase(w event.Workflow) {
	c.Emit(event.Event{Kind: event.WorkflowMaterial, Workflow: w})
}

func (c *materialCollector) finalText() string {
	if strings.TrimSpace(c.last) != "" {
		return c.last
	}
	return c.deltas.String()
}

// written 返回项目根下的相对路径,去重升序。
func (c *materialCollector) written() []string {
	if len(c.files) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.files))
	for p := range c.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// collectPaths 从一次写工具调用的原始 args 里取出它写过的路径。
func (c *materialCollector) collectPaths(rawArgs string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawArgs), &fields); err != nil {
		return
	}
	for _, key := range []string{"path", "file_path", "filePath", "file", "notebook_path"} {
		var s string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &s) == nil {
			c.add(s)
		}
	}
	for _, key := range []string{"paths", "file_paths"} {
		var list []string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &list) == nil {
			for _, s := range list {
				c.add(s)
			}
		}
	}
}

// add 把一个路径归一成项目根下的相对路径。落在根外的一律丢弃 —— 那种调用本来就
// 会被门禁拒掉,真出现了也不该记进这个项目的产物。
func (c *materialCollector) add(p string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(c.root, abs)
	}
	rel, err := filepath.Rel(c.root, filepath.Clean(abs))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	if c.files == nil {
		c.files = map[string]bool{}
	}
	c.files[filepath.ToSlash(rel)] = true
}

// materialWriterTools 是"写过文件"的工具名表,与 evidence.isWriterTool 同口径,
// 外加 dsh 那套同义名(它在自己进程里跑自己的工具,名字更短)。
//
// 这张表**只影响采集**:漏一个名字不会放行任何东西,但会让那次写盘不算进
// WrittenFiles,于是作业层的假成功检测把一次真成功判成失败。宁可多列。
var materialWriterTools = map[string]bool{
	// native
	"write_file":    true,
	"edit_file":     true,
	"multi_edit":    true,
	"notebook_edit": true,
	"delete_range":  true,
	"delete_symbol": true,
	// dsh
	"write":     true,
	"edit":      true,
	"multiedit": true,
}
