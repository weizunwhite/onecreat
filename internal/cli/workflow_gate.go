package cli

// 材料作业专用门禁(M2 拍板 1:**没有 ask,只有 allow/deny**)。
//
// 为什么不能直接用配置里的 permission.Policy:
//
//	permission.Policy 的兜底口径是"写工具按 Mode、读工具一律 Allow"。Mode 设成 deny
//	也只挡得住写工具 —— 一个只读的网络工具(web_fetch)、一个只读的子代理工具,照样
//	Allow。而材料作业是**无人值守**的批量生成:没有人坐在那儿答 ask,ask 在 headless
//	下会退化成 Allow(见 permission.Gate.Check 的 "non-interactive: preserve autonomy"),
//	那正是 05 §4/N4 记下的口径缺口。所以这里不复用规则求值,而是在装配层包一层
//	**白名单优先、兜底 deny** 的 Gate —— 它满足 toolpolicy.Gate 接口,经 boot.Options.Gate
//	装到同一条 toolpolicy 流水线上,native 与 dsh 两条路径拿到的是同一个门。
//
// 允许矩阵(白名单之外一律 deny,包括所有 MCP 工具、网络工具、子代理工具):
//
//	工具                                    判定
//	read_file / ls / glob / grep            路径全在项目目录内 → allow,否则 deny
//	write_file / edit_file / multi_edit     同上
//	notebook_edit                           同上
//	todo_write / complete_step              allow(不碰文件系统、不联网;证据引擎靠它们记账)
//	bash                                    命令名 ∈ {mkdir, ls} 且无 shell 元字符且路径在内 → allow
//	其余(web_fetch / task / ask / run_skill / delete_* / mcp__* / …)  deny
//
// bash 白名单**故意从紧**只留 mkdir / ls:材料作业要的是"建目录 + 看目录",读文件有
// read_file、写文件有 write_file,没有一件事非得开一个 shell 才能做。以后要放宽(cat、
// python3 给某些 skill 用),在 materialBashAllowed 里加一条,并同步改这段注释。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"reasonix/internal/toolpolicy"
)

// materialGate 是材料作业会话的工具门禁,把项目目录当唯一可写边界。
type materialGate struct {
	// root 是项目目录绝对路径(未解符号链接的原样)。
	root string
	// real 是 root 解完符号链接的样子。macOS 上 /var → /private/var 这类链接会让
	// 纯字符串前缀比较误判成"在外面",两份都留着比。
	real string
}

// 编译期确认它满足流水线要的门禁接口。
var _ toolpolicy.Gate = (*materialGate)(nil)

func newMaterialGate(root string) *materialGate {
	clean := filepath.Clean(root)
	real := clean
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		real = resolved
	}
	return &materialGate{root: clean, real: real}
}

// Check 是 toolpolicy.Gate 的实现:允许返回 (true, "", nil);拒绝返回
// (false, 给模型看的原因, nil)。**永远不返回 ask** —— 这一层没有人可问。
func (g *materialGate) Check(_ context.Context, name string, args json.RawMessage, _ bool) (bool, string, error) {
	switch {
	case materialPlainTools[name]:
		return true, "", nil
	case materialPathTools[name]:
		if bad, ok := g.firstOutsidePath(args); !ok {
			return false, g.outsideReason(name, bad), nil
		}
		return true, "", nil
	case name == "bash":
		return g.checkBash(args)
	default:
		return false, fmt.Sprintf(
			"材料作业门禁拒绝了 %q:这轮只允许在项目目录内读写文件、建目录、记 todo,不允许联网、"+
				"派子代理、提问或调用其他工具。不要重试这个工具,换成 read_file / write_file / edit_file 完成任务;"+
				"确实缺信息就按合理假设继续写,并在正文里标注这是假设。", name), nil
	}
}

// checkBash 判定一条 shell 命令能不能跑。
func (g *materialGate) checkBash(args json.RawMessage) (bool, string, error) {
	cmd := strings.TrimSpace(stringArg(args, "command"))
	if cmd == "" {
		return false, "材料作业门禁拒绝了空 bash 命令。", nil
	}
	// 有 shell 元字符就直接拒:一旦允许 `&&` / `|` / `$(...)`,白名单就形同虚设
	// ——`mkdir x && curl ...` 的命令名也是 mkdir。
	if i := strings.IndexAny(cmd, ";&|`\n\r><$(){}*?~"); i >= 0 {
		return false, fmt.Sprintf(
			"材料作业门禁拒绝了这条 bash 命令(含 shell 元字符 %q):这轮只允许最简单的单条命令(mkdir / ls),"+
				"不允许管道、重定向、命令替换与通配。不要重试,改用 write_file / read_file 完成。", string(cmd[i])), nil
	}
	fields := strings.Fields(cmd)
	if !materialBashAllowed[filepath.Base(fields[0])] {
		return false, fmt.Sprintf(
			"材料作业门禁拒绝了 bash 命令 %q:这轮的命令白名单只有 mkdir 和 ls。不要重试这条命令 ——"+
				"读文件用 read_file,写文件用 write_file,建目录用 mkdir(不带任何管道)。", fields[0]), nil
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if !g.inside(arg) {
			return false, g.outsideReason("bash", arg), nil
		}
	}
	return true, "", nil
}

// firstOutsidePath 找出第一个落在项目目录外的路径参数;ok=false 时 bad 是它。
func (g *materialGate) firstOutsidePath(args json.RawMessage) (bad string, ok bool) {
	for _, p := range materialPathArgs(args) {
		if !g.inside(p) {
			return p, false
		}
	}
	return "", true
}

// inside 报告一个路径是否落在项目目录之内。相对路径按项目根解析(文件工具就是这么
// 解析的);`..` 逃逸、绝对路径越界、以及经符号链接绕出去,全都算不在内。
func (g *materialGate) inside(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	// `~` 开头一律当越界。今天的文件工具不展开它(会当成一个名叫 `~` 的目录),
	// 但只要哪天有一个工具展开了,门禁必须已经说过不。
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return false
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(g.root, abs)
	}
	abs = filepath.Clean(abs)
	if under(g.root, abs) && under(g.real, resolveExisting(abs)) {
		return true
	}
	return false
}

func (g *materialGate) outsideReason(tool, path string) string {
	return fmt.Sprintf(
		"材料作业门禁拒绝了 %s 对 %q 的访问:这轮只能读写项目目录 %s 之内的文件。不要重试这个路径,"+
			"把产物写进项目七目录里对应的桶。", tool, path, g.root)
}

// under 报告 p 是否在 root 之内(root 自身算在内)。
func under(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// resolveExisting 把路径里**已经存在**的那一段解掉符号链接,尾部还不存在的部分原样
// 拼回去。写新文件时目标本身还不存在,直接 EvalSymlinks 会失败;而真正能把人骗出
// 边界的,恰恰是已经存在的那段目录链。
func resolveExisting(p string) string {
	cur := p
	var tail []string
	for i := 0; i < 64; i++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for j := len(tail) - 1; j >= 0; j-- {
				resolved = filepath.Join(resolved, tail[j])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
	return p
}

// materialPathArgs 取出一次工具调用里的全部路径参数,与 evidence.extractPaths 同口径。
func materialPathArgs(args json.RawMessage) []string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return nil
	}
	var out []string
	// 两条引擎的键名不一样:native 用 path / file_path,dsh 还会用 filePath / file。
	for _, key := range []string{"path", "file_path", "filePath", "file", "notebook_path"} {
		var s string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	for _, key := range []string{"paths", "file_paths"} {
		var list []string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &list) == nil {
			for _, s := range list {
				if s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// stringArg 取一个字符串参数,取不到返回空串。
func stringArg(args json.RawMessage, key string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return ""
	}
	var s string
	if raw, ok := fields[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// materialPathTools 是"允许,但路径必须在项目目录内"的工具。
//
// 两套名字都要在:native 引擎用 Go 工具注册表的名字(read_file / write_file /
// edit_file),dsh 引擎在自己进程里跑自己那套(read / write / edit)。门禁是
// fail-closed 的,所以漏一个名字的后果是"该工具在那条引擎下用不了",不是放行 ——
// 但那会让 --engine dsh 的材料作业跑不出东西,所以两套都列全。
var materialPathTools = map[string]bool{
	// native(internal/tool/builtin)
	"read_file":     true,
	"write_file":    true,
	"edit_file":     true,
	"multi_edit":    true,
	"notebook_edit": true,
	// dsh(sidecar 自带的同名工具)
	"read":      true,
	"write":     true,
	"edit":      true,
	"multiedit": true,
	// 两边同名
	"ls":   true,
	"glob": true,
	"grep": true,
}

// materialPlainTools 是"不碰文件系统、不联网,直接允许"的工具:证据引擎的两件记账
// 工具。少了它们,complete_step 拿不到 todo 基线,假成功检测就没了参照。
var materialPlainTools = map[string]bool{
	"todo_write":    true,
	"complete_step": true,
}

// materialBashAllowed 是 bash 命令名白名单(取 basename 后比对)。
var materialBashAllowed = map[string]bool{
	"mkdir": true,
	"ls":    true,
}
