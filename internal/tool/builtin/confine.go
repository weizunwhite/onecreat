package builtin

import (
	"fmt"
	"path/filepath"
	"strings"

	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// ConfineBash returns the bash built-in bound to an OS-sandbox spec, overriding
// the unconfined instance registered at init. When the spec enforces, bash runs
// each command through the sandbox (see package sandbox).
func ConfineBash(spec sandbox.Spec) tool.Tool {
	return bash{sb: spec, shell: sandbox.ResolveShell()}
}

// ConfineWriters returns the file-writing built-ins (write_file, edit_file,
// multi_edit, notebook_edit) bound to roots — the only directories they may
// modify. The composition root adds these to the per-run registry to override
// the unconfined instances registered at init time, so writes stay inside the
// workspace by default. roots may be relative; they are resolved to absolute,
// symlink-free paths once here. An empty roots slice yields unconfined writers.
func ConfineWriters(roots []string) []tool.Tool {
	rs := realRoots(roots)
	return []tool.Tool{
		writeFile{roots: rs},
		editFile{roots: rs},
		multiEdit{roots: rs},
		notebookEdit{roots: rs},
		deleteRange{roots: rs},
		deleteSymbol{roots: rs},
	}
}

// realRoots resolves each root to an absolute, symlink-free path, dropping any
// that cannot be made absolute. Resolving here (once) means the per-call check
// only has to resolve the target.
func realRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if real, err := realPath(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// confine reports an error when target resolves outside every root. An empty
// roots slice is unconfined (returns nil) — the safe default for the built-in
// templates before a run configures the workspace. The error text is written
// for the model: it names the boundary and how the user can widen it.
func confine(roots []string, target string) error {
	if len(roots) == 0 {
		return nil
	}
	abs, err := realPath(target)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", target, err)
	}
	for _, r := range roots {
		if within(r, abs) {
			return nil
		}
	}
	// 行空板实测:模型想在工作区外建新项目,被拒后 bash 反复试 mkdir 甚至 sudo,
	// 最后把新项目嵌进旧项目目录。这里必须把"唯一正确的两条路"讲清楚,
	// 并明确劝阻 bash 绕行。
	return fmt.Errorf("路径 %q 在工作区之外(本会话只允许写入:%s)。"+
		"两个选择:① 改在当前工作区内创建;② 如果用户确实要一个新的独立项目目录,"+
		"停下来告诉用户「请在侧栏切换/新建工作区后重试」,让用户操作。"+
		"不要用 bash 的 mkdir/sudo 绕过这个限制——沙箱同样会拒绝,只会浪费轮次",
		target, strings.Join(roots, ", "))
}

// realPath resolves path to an absolute, symlink-free form. Because a write
// target need not exist yet (write_file creates it), it resolves the deepest
// existing ancestor with EvalSymlinks and re-appends the not-yet-existing tail.
// This stops a symlinked directory from smuggling a write outside a root.
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	tail := ""
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil // nothing along the path exists; use the cleaned abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// within reports whether path is at or below root. Both must be absolute,
// cleaned, symlink-free. It uses filepath.Rel so it is correct across volumes
// and is not fooled by a prefix that only matches a partial path component
// (e.g. /work-other is not within /work).
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
