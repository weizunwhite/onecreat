package evidence

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// TodoItem mirrors the todo_write item shape the host needs for step matching.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
	Level      int    `json:"level,omitempty"`
}

// TodoStepMatch is the result of matching complete_step.step against the latest
// successful todo_write list in this turn.
type TodoStepMatch struct {
	Found      bool
	Index      int
	Content    string
	Status     string
	ActiveForm string
}

// Receipt is the host-runtime record of one tool call. It stays in memory for
// the current agent turn and is not serialized into prompts or session state.
type Receipt struct {
	ToolName string          `json:"tool_name"`
	Args     json.RawMessage `json:"args,omitempty"`
	Success  bool            `json:"success"`
	Command  string          `json:"command,omitempty"`
	Step     string          `json:"step,omitempty"`
	TodoStep *TodoStepMatch  `json:"todo_step,omitempty"`
	Paths    []string        `json:"paths,omitempty"`
	Read     bool            `json:"read,omitempty"`
	Write    bool            `json:"write,omitempty"`
	Todos    []TodoItem      `json:"todos,omitempty"`
}

// Ledger stores the receipts available to complete_step for the current turn.
type Ledger struct {
	mu       sync.Mutex
	receipts []Receipt
}

func NewLedger() *Ledger { return &Ledger{} }

// Reset clears receipts between user turns.
func (l *Ledger) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.receipts = nil
}

// Record appends a receipt. Failed receipts are retained for auditability but
// are never accepted by the HasSuccessful* matchers.
func (l *Ledger) Record(r Receipt) {
	if l == nil {
		return
	}
	r.Command = strings.TrimSpace(r.Command)
	r.Step = strings.TrimSpace(r.Step)
	r.Paths = normalizePaths(r.Paths)
	r.Todos = normalizeTodos(r.Todos)
	if r.Args != nil {
		cp := make(json.RawMessage, len(r.Args))
		copy(cp, r.Args)
		r.Args = cp
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Success && r.ToolName == "complete_step" && r.Step != "" && r.TodoStep == nil {
		if match := latestTodoStep(r.Step, l.receipts); match.Found {
			r.TodoStep = &match
		}
	}
	l.receipts = append(l.receipts, r)
}

func (l *Ledger) HasSuccessfulCommand(command string) bool {
	command = strings.TrimSpace(command)
	if l == nil || command == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success {
			continue
		}
		// bash 收据:精确相等之外,容忍空白/引号差异与截短复述(commandsEquivalent)。
		if r.ToolName == "bash" && commandsEquivalent(command, r.Command) {
			return true
		}
		// 工具型验证:硬件/技能流程的验证多走 MCP 工具(hardware_project_validate、
		// hardware_detect 等)或内置工具,它们不产生 bash 收据、没有 Command 字段。
		// 只要该工具成功调用过、且其核心名(剥掉 mcp__<server>__ 前缀)被模型填的
		// verification command 提及,就认作有效验证——否则硬件流程里 complete_step
		// 的证据收据永远凑不齐,模型会反复撞墙。
		if core := toolCoreName(r.ToolName); core != "bash" && len(core) >= 4 && strings.Contains(command, core) {
			return true
		}
	}
	return false
}

// commandsEquivalent 判断模型引用的 verification command 与真实运行过的 bash 命令
// 是否语义等价。实测(行空板会话)flash 复述命令时会改引号(双引号↔单引号)、
// 压缩空白或截短,全文精确匹配必然 miss。归一化(压空白、去引号/反斜杠)后:
// 相等即认;一方包含另一方(带最短长度护栏,防 "ls" 这类碰瓷)也认——被包含
// 仍意味着引用的内容真实运行过,凭空编造的命令依然对不上任何收据。
func commandsEquivalent(cited, actual string) bool {
	if cited == actual {
		return true
	}
	norm := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		return strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(s)
	}
	nc, na := norm(cited), norm(actual)
	if nc == "" || na == "" {
		return false
	}
	if nc == na {
		return true
	}
	const minLen = 12
	if len(nc) >= minLen && strings.Contains(na, nc) {
		return true
	}
	if len(na) >= minLen && strings.Contains(nc, na) {
		return true
	}
	return false
}

// toolCoreName strips the "mcp__<server>__" namespace prefix from an MCP tool
// name (mcp__hardware__hardware_project_validate → hardware_project_validate);
// built-in tool names pass through unchanged.
func toolCoreName(name string) string {
	if rest := strings.TrimPrefix(name, "mcp__"); rest != name {
		if i := strings.Index(rest, "__"); i >= 0 {
			return rest[i+2:]
		}
		return rest
	}
	return name
}

// deviceActionTools 是会改变真机状态的硬件动作工具(烧录/部署/推代码上板),
// 按剥掉 mcp__<server>__ 前缀后的核心名匹配。
var deviceActionTools = map[string]bool{
	"arduino_upload":     true,
	"arduino_ota_upload": true,
	"platformio_run":     true,
	"esp_idf_run":        true,
	"mpremote_run":       true,
	"ssh_deploy_run":     true,
}

// HasDeviceActionThisTurn 报告本轮是否成功执行过改变真机状态的动作(烧录/部署)。
// 供 complete_step 拒绝 manual 后门:Ledger 按用户轮清空,单轮之内不可能有新的用户
// 输入,所以"用户已确认本轮烧录后的实物现象"这类 manual 证据在同一轮内必然是编造的——
// 用户还没有机会观察并回复。
func (l *Ledger) HasDeviceActionThisTurn() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && deviceActionTools[toolCoreName(r.ToolName)] {
			return true
		}
	}
	return false
}

func (l *Ledger) HasSuccessfulWrite(paths []string) bool {
	return l.hasSuccessfulPaths(paths, func(r Receipt) bool { return r.Write })
}

func (l *Ledger) HasSuccessfulReadOrWrite(paths []string) bool {
	return l.hasSuccessfulPaths(paths, func(r Receipt) bool { return r.Read || r.Write })
}

// LatestTodos returns the todo list from this turn's most recent successful
// todo_write receipt. ok is false when the turn recorded none — the turn-end
// reconcile in agent.Run treats that as "nothing to reconcile" (a stale list
// from an earlier turn is not this turn's claim, so it is never nagged about).
func (l *Ledger) LatestTodos() (todos []TodoItem, ok bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.receipts) - 1; i >= 0; i-- {
		r := l.receipts[i]
		if r.Success && r.ToolName == "todo_write" {
			return r.Todos, true
		}
	}
	return nil, false
}

func (l *Ledger) MatchLatestTodoStep(step string) (TodoStepMatch, bool) {
	step = strings.TrimSpace(step)
	if l == nil || step == "" {
		return TodoStepMatch{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.receipts) - 1; i >= 0; i-- {
		r := l.receipts[i]
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		return matchTodoStep(step, r.Todos), true
	}
	return TodoStepMatch{}, false
}

// UnverifiedCompletedTodos reports current completed todos that transitioned
// from the latest prior successful todo_write receipt without a matching
// successful complete_step receipt earlier in the same turn. If this turn has no
// prior todo_write baseline, hasBaseline is false and callers should preserve
// the existing loose validation behavior.
func (l *Ledger) UnverifiedCompletedTodos(current []TodoItem) (missing []TodoStepMatch, hasBaseline bool) {
	current = normalizeTodos(current)
	if l == nil {
		return nil, false
	}

	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()

	var previous []TodoItem
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		previous = r.Todos
		hasBaseline = true
		break
	}
	if !hasBaseline {
		return nil, false
	}

	for i, t := range current {
		if todoStatus(t.Status) != "completed" {
			continue
		}
		index := i + 1
		if previousTodoCompleted(index, t, previous) {
			continue
		}
		if hasSuccessfulCompleteStepForTodo(receipts, index, current) {
			continue
		}
		missing = append(missing, TodoStepMatch{
			Found:      true,
			Index:      index,
			Content:    t.Content,
			Status:     todoStatus(t.Status),
			ActiveForm: t.ActiveForm,
		})
	}
	return missing, true
}

func (l *Ledger) hasSuccessfulPaths(paths []string, accept func(Receipt) bool) bool {
	wanted := normalizePaths(paths)
	if l == nil || len(wanted) == 0 {
		return false
	}
	found := map[string]bool{}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success || !accept(r) {
			continue
		}
		for _, p := range r.Paths {
			for _, w := range wanted {
				if pathRefersTo(w, p) {
					found[w] = true
				}
			}
		}
	}
	return len(found) == len(wanted)
}

// pathRefersTo 判断模型引用的路径与收据路径是否指向同一文件。
// 实测(行空板会话)模型引用相对路径(unihiker_kaleidoscope/main.py)而收据存的是
// 工作区绝对路径,精确相等匹配全部 miss → complete_step 反复撞墙。
// 放宽为"按路径段为边界的后缀匹配":相对/绝对、嵌套前缀差异都能对上,
// 但仍必须指向本轮真实出现过的文件,不能凭空声称。
func pathRefersTo(cited, actual string) bool {
	if cited == actual {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasSuffix(actual, sep+cited) || strings.HasSuffix(cited, sep+actual)
}

type contextKey struct{}

func WithLedger(ctx context.Context, ledger *Ledger) context.Context {
	if ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, ledger)
}

func FromContext(ctx context.Context) (*Ledger, bool) {
	ledger, ok := ctx.Value(contextKey{}).(*Ledger)
	return ledger, ok && ledger != nil
}

func ReceiptFromToolCall(toolName string, args json.RawMessage, success bool, readOnly bool) Receipt {
	r := Receipt{
		ToolName: toolName,
		Args:     args,
		Success:  success,
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err == nil {
		if toolName == "bash" {
			r.Command = stringField(fields, "command")
		}
		if toolName == "complete_step" {
			r.Step = stringField(fields, "step")
		}
		if toolName == "todo_write" {
			r.Todos = todoItemsField(fields, "todos")
		}
		r.Paths = extractPaths(fields)
	}

	if isWriterTool(toolName) {
		r.Write = true
	} else if isReaderTool(toolName) || (readOnly && len(r.Paths) > 0) {
		r.Read = true
	}
	return r
}

func isWriterTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "multi_edit", "notebook_edit", "delete_range", "delete_symbol":
		return true
	default:
		return false
	}
}

func isReaderTool(name string) bool {
	switch name {
	case "read_file", "ls", "grep":
		return true
	default:
		return false
	}
}

func extractPaths(fields map[string]json.RawMessage) []string {
	var paths []string
	for _, key := range []string{"path", "file_path", "notebook_path"} {
		if s := stringField(fields, key); s != "" {
			paths = append(paths, s)
		}
	}
	for _, key := range []string{"paths", "file_paths"} {
		paths = append(paths, stringSliceField(fields, key)...)
	}
	return paths
}

func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func stringSliceField(fields map[string]json.RawMessage, key string) []string {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

func todoItemsField(fields map[string]json.RawMessage, key string) []TodoItem {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var todos []TodoItem
	if err := json.Unmarshal(raw, &todos); err != nil {
		return nil
	}
	return normalizeTodos(todos)
}

func normalizeTodos(todos []TodoItem) []TodoItem {
	out := make([]TodoItem, 0, len(todos))
	for _, t := range todos {
		t.Content = strings.TrimSpace(t.Content)
		t.Status = strings.TrimSpace(t.Status)
		t.ActiveForm = strings.TrimSpace(t.ActiveForm)
		out = append(out, t)
	}
	return out
}

func todoStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "pending"
	}
	return status
}

func previousTodoCompleted(index int, current TodoItem, previous []TodoItem) bool {
	if index >= 1 && index <= len(previous) {
		p := previous[index-1]
		if todoStatus(p.Status) == "completed" && sameTodoIdentity(current, p) {
			return true
		}
	}
	for _, p := range previous {
		if todoStatus(p.Status) == "completed" && sameTodoIdentity(current, p) {
			return true
		}
	}
	return false
}

func sameTodoIdentity(a, b TodoItem) bool {
	return sameStepText(a.Content, b.Content) || sameStepText(a.ActiveForm, b.ActiveForm)
}

func hasSuccessfulCompleteStepForTodo(receipts []Receipt, index int, current []TodoItem) bool {
	for _, r := range receipts {
		if !r.Success || r.ToolName != "complete_step" || strings.TrimSpace(r.Step) == "" {
			continue
		}
		if r.TodoStep != nil && r.TodoStep.Found {
			if index >= 1 && index <= len(current) && sameTodoMatch(current[index-1], *r.TodoStep) {
				return true
			}
			continue
		}
		match := matchTodoStep(r.Step, current)
		if match.Found && match.Index == index {
			return true
		}
	}
	return false
}

func latestTodoStep(step string, receipts []Receipt) TodoStepMatch {
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		return matchTodoStep(step, r.Todos)
	}
	return TodoStepMatch{}
}

func sameTodoMatch(todo TodoItem, match TodoStepMatch) bool {
	return sameStepText(todo.Content, match.Content) || sameStepText(todo.ActiveForm, match.ActiveForm)
}

func matchTodoStep(step string, todos []TodoItem) TodoStepMatch {
	if n, ok := parseStepIndex(step); ok && n >= 1 && n <= len(todos) {
		t := todos[n-1]
		return TodoStepMatch{Found: true, Index: n, Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm}
	}
	for i, t := range todos {
		if sameStepText(step, t.Content) || sameStepText(step, t.ActiveForm) {
			return TodoStepMatch{Found: true, Index: i + 1, Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm}
		}
	}
	return TodoStepMatch{}
}

func parseStepIndex(step string) (int, bool) {
	step = strings.TrimSpace(strings.TrimSuffix(step, "."))
	n, err := strconv.Atoi(step)
	return n, err == nil
}

func sameStepText(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func normalizePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = normalizePath(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, `/`)
	p = filepath.Clean(filepath.FromSlash(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
