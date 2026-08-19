package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"reasonix/internal/diff"
)

// EngineBackend 是「底层 agent 引擎」在 Controller 之下的最小接缝。
//
// native 引擎(现有 Go 内核)不实现它 —— 那条路径上 Controller 的 engine 字段是
// nil,行为与迁移前一字不差。dsh sidecar 引擎(internal/engine/dsh.Engine)实现它,
// 于是 Controller 只需要在几个"引擎自己有状态"的命令上分流,其余(Compose、
// hooks、checkpoint、审批、证据、slash、记忆、@引用)全部复用。
//
// 一轮对话本身走的是 agent.Runner —— dsh.Engine 同时实现了 Runner,所以
// boot.Build 只是把 Options.Runner 换成它。
type EngineBackend interface {
	// BindSession 把引擎的会话绑到一个 Go 会话文件路径上(新建/恢复/切换时调用)。
	BindSession(path string)
	// SetApprover 接上交互式审批回调(nil = 非交互,Ask 按 native 语义放行)。
	SetApprover(func(ctx context.Context, toolName, subject string) (bool, bool, error))
	// SetPreEdit 接上"工具即将执行"的回调,Go 侧据此为 checkpoint 做文件快照。
	SetPreEdit(func(name string, args json.RawMessage))
	// Cancel 取消当前 turn。
	Cancel() error
	// Running 报告引擎侧是否在跑。
	Running() bool
	// SetPlanMode 下发计划模式。
	SetPlanMode(active bool) error
	// Inject 把一段文本作为下一次 pre-step 的模型可见上下文注入(不进系统提示)。
	Inject(text string) error
	// Close 关掉引擎(sidecar 进程),退出无残留。
	Close() error
}

// engineUnsupported 是 dsh 引擎下暂不支持的操作的统一错误文案。
// 每一条都记在 docs/dsh调研/05_Phase1-2_实施报告.md 的功能对照表里。
func engineUnsupported(op string) error {
	return errUnsupported{op: op}
}

type errUnsupported struct{ op string }

func (e errUnsupported) Error() string {
	return "dsh 引擎暂不支持「" + e.op + "」(会话历史的真源在 dsh 侧,该操作需要重写消息日志)"
}

// engineActive 报告当前是否跑在非 native 引擎上。
func (c *Controller) engineActive() bool {
	return c.engine != nil
}

// engineBind 在会话文件路径变化时同步给引擎。
func (c *Controller) engineBind(path string) {
	if c.engine != nil {
		c.engine.BindSession(path)
	}
}

// RequestApproval 是审批的导出入口,供引擎后端(dsh 的审批桥)复用 native 的
// 整套审批语义:会话内授权记忆、YOLO/bypass、pendingApprovals 重放、Notification
// 钩子。返回 (allow, remember, err)。
func (c *Controller) RequestApproval(ctx context.Context, tool, subject string) (bool, bool, error) {
	return c.requestApproval(ctx, tool, subject)
}

// enginePreEdit 是 dsh 引擎的文件快照钩子:sidecar 每次要执行工具前调它,我们把
// 参数里出现的文件路径的**当前内容**存进本轮 checkpoint,于是"代码 rewind"在
// dsh 引擎下照旧可用(dsh 自己没有文件级 checkpoint)。
func (c *Controller) enginePreEdit(name string, args json.RawMessage) {
	c.mu.Lock()
	cp := c.cp
	root := c.cpRoot
	c.mu.Unlock()
	if cp == nil {
		return
	}
	for _, path := range editPathsFromArgs(name, args) {
		abs := path
		if !filepath.IsAbs(abs) && root != "" {
			abs = filepath.Join(root, abs)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			// 文件还不存在 = 这次是创建:记 Create,rewind 时删掉它。
			cp.Snapshot(diff.Change{Path: abs, Kind: diff.Create})
			continue
		}
		old := string(data)
		if !utf8.ValidString(old) {
			// 二进制:checkpoint.Snapshot 自己会 base64,这里原样交过去。
			cp.Snapshot(diff.Change{Path: abs, Kind: diff.Modify, OldText: old})
			continue
		}
		cp.Snapshot(diff.Change{Path: abs, Kind: diff.Modify, OldText: old})
	}
}

// editPathsFromArgs 从工具参数里挑出"会被写"的文件路径。只认写类工具,读类工具
// 不占 checkpoint 配额。
func editPathsFromArgs(name string, args json.RawMessage) []string {
	switch name {
	case "write", "edit", "create", "str_replace_editor", "write_file", "edit_file":
	default:
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "file_path", "filePath", "file"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
