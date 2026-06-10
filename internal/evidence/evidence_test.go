package evidence

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLedgerRecordsSuccessAndFailureReceipts(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "bash",
		Args:     json.RawMessage(`{"command":"go test ./..."}`),
		Success:  true,
		Command:  "go test ./...",
	})
	ledger.Record(Receipt{
		ToolName: "bash",
		Args:     json.RawMessage(`{"command":"go test ./internal/..."}`),
		Success:  false,
		Command:  "go test ./internal/...",
	})

	if !ledger.HasSuccessfulCommand("go test ./...") {
		t.Fatal("successful bash command should verify")
	}
	if ledger.HasSuccessfulCommand("go test ./internal/...") {
		t.Fatal("failed bash command must not verify")
	}
}

// 硬件/技能流程的验证走 MCP 工具(不产生 bash 收据)。complete_step 的
// verification command 填工具名时,成功调用过的工具应算有效证据——否则模型
// 反复撞「no matching successful receipt」(贪吃蛇会话实测的最大慢因)。
func TestLedgerVerifiesSuccessfulToolCall(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(ReceiptFromToolCall("mcp__hardware__hardware_project_validate",
		json.RawMessage(`{"project_dir":"/p"}`), true, true))
	ledger.Record(ReceiptFromToolCall("mcp__hardware__hardware_detect",
		json.RawMessage(`{"project_dir":"/p"}`), false, true)) // 失败的不算

	// 模型常填工具名 + 备注,如 "hardware_project_validate (pio run)"。
	if !ledger.HasSuccessfulCommand("hardware_project_validate (pio run)") {
		t.Fatal("成功的 MCP 验证工具应被认作有效证据")
	}
	if ledger.HasSuccessfulCommand("hardware_detect") {
		t.Fatal("失败的工具调用不应被认作证据")
	}
	if ledger.HasSuccessfulCommand("hardware_project_audit") {
		t.Fatal("没调用过的工具不应被认作证据")
	}
}

func TestLedgerMatchesFileReadAndWriteReceipts(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{ToolName: "read_file", Success: true, Paths: []string{`internal/tool/builtin/completestep.go`}, Read: true})
	ledger.Record(Receipt{ToolName: "write_file", Success: true, Paths: []string{`internal/evidence/evidence.go`}, Write: true})
	ledger.Record(Receipt{ToolName: "edit_file", Success: false, Paths: []string{`failed.go`}, Write: true})

	if !ledger.HasSuccessfulReadOrWrite([]string{`internal\tool\builtin\completestep.go`}) {
		t.Fatal("read receipt should verify the same path across separators")
	}
	if !ledger.HasSuccessfulWrite([]string{`internal/evidence/evidence.go`}) {
		t.Fatal("write receipt should verify written path")
	}
	if ledger.HasSuccessfulWrite([]string{`failed.go`}) {
		t.Fatal("failed write receipt must not verify")
	}
}

func TestLedgerResetClearsTurnReceipts(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{ToolName: "bash", Success: true, Command: "go test ./..."})

	ledger.Reset()

	if ledger.HasSuccessfulCommand("go test ./...") {
		t.Fatal("reset should clear prior-turn evidence")
	}
}

func TestContextCarriesLedger(t *testing.T) {
	ledger := NewLedger()
	ctx := WithLedger(context.Background(), ledger)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("ledger missing from context")
	}
	if got != ledger {
		t.Fatal("context returned a different ledger")
	}
}

func TestReceiptFromToolCallExtractsEvidenceFields(t *testing.T) {
	bash := ReceiptFromToolCall("bash", json.RawMessage(`{"command":"git diff --check"}`), true, false)
	if bash.Command != "git diff --check" {
		t.Fatalf("bash command = %q", bash.Command)
	}
	if bash.Write {
		t.Fatal("bash should not be treated as a verified file writer")
	}

	write := ReceiptFromToolCall("write_file", json.RawMessage(`{"path":"internal/evidence/evidence.go","content":"x"}`), true, false)
	if !write.Write || len(write.Paths) != 1 || write.Paths[0] != `internal/evidence/evidence.go` {
		t.Fatalf("write receipt not extracted: %+v", write)
	}

	read := ReceiptFromToolCall("read_file", json.RawMessage(`{"path":"internal/tool/builtin/completestep.go"}`), true, true)
	if !read.Read || len(read.Paths) != 1 {
		t.Fatalf("read receipt not extracted: %+v", read)
	}
}

func TestReceiptFromToolCallExtractsTodoWriteItems(t *testing.T) {
	receipt := ReceiptFromToolCall("todo_write", json.RawMessage(`{"todos":[
		{"content":"Add parser","status":"in_progress","activeForm":"Adding parser"},
		{"content":"Wire parser","status":"pending","level":1}
	]}`), true, true)

	if len(receipt.Todos) != 2 {
		t.Fatalf("todos not extracted: %+v", receipt)
	}
	if receipt.Todos[0].Content != "Add parser" || receipt.Todos[0].Status != "in_progress" || receipt.Todos[0].ActiveForm != "Adding parser" {
		t.Fatalf("first todo not extracted: %+v", receipt.Todos[0])
	}
	if receipt.Todos[1].Level != 1 {
		t.Fatalf("todo level not extracted: %+v", receipt.Todos[1])
	}
}

func TestReceiptFromToolCallExtractsCompleteStep(t *testing.T) {
	receipt := ReceiptFromToolCall("complete_step", json.RawMessage(`{
		"step":"Add parser",
		"result":"parser added",
		"evidence":[{"kind":"manual","summary":"checked manually"}]
	}`), true, true)

	if receipt.Step != "Add parser" {
		t.Fatalf("complete_step step = %q", receipt.Step)
	}
}

func TestLedgerMatchesLatestSuccessfulTodoStep(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  false,
		Todos:    []TodoItem{{Content: "Failed only", Status: "in_progress"}},
	})
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  true,
		Todos: []TodoItem{
			{Content: "Add parser", Status: "in_progress", ActiveForm: "Adding parser"},
			{Content: "Wire parser", Status: "completed"},
			{Content: "Document parser", Status: "pending"},
		},
	})

	for _, step := range []string{"Add parser", "Adding parser", "2"} {
		match, ok := ledger.MatchLatestTodoStep(step)
		if !ok {
			t.Fatalf("latest todo receipt missing for %q", step)
		}
		if !match.Found {
			t.Fatalf("step %q did not match latest todo list", step)
		}
		if step == "2" && match.Content != "Wire parser" {
			t.Fatalf("numeric step matched %q, want Wire parser", match.Content)
		}
	}

	match, ok := ledger.MatchLatestTodoStep("Failed only")
	if !ok {
		t.Fatal("successful todo receipt should exist")
	}
	if match.Found {
		t.Fatal("failed todo_write receipt must not match")
	}
}

func TestLedgerRequiresCompleteStepForNewCompletedTodos(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  true,
		Todos: []TodoItem{
			{Content: "Add parser", Status: "in_progress"},
			{Content: "Already done", Status: "completed"},
		},
	})

	current := []TodoItem{
		{Content: "Add parser", Status: "completed"},
		{Content: "Already done", Status: "completed"},
	}
	missing, hasBaseline := ledger.UnverifiedCompletedTodos(current)
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline")
	}
	if len(missing) != 1 || missing[0].Content != "Add parser" {
		t.Fatalf("missing = %+v, want only Add parser", missing)
	}

	ledger.Record(Receipt{ToolName: "complete_step", Success: false, Step: "Add parser"})
	missing, hasBaseline = ledger.UnverifiedCompletedTodos(current)
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline after failed complete_step")
	}
	if len(missing) != 1 || missing[0].Content != "Add parser" {
		t.Fatalf("failed complete_step should not authorize completion, missing = %+v", missing)
	}

	ledger.Record(Receipt{ToolName: "complete_step", Success: true, Step: "Add parser"})
	missing, hasBaseline = ledger.UnverifiedCompletedTodos(current)
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline after successful complete_step")
	}
	if len(missing) != 0 {
		t.Fatalf("successful complete_step should authorize completion, missing = %+v", missing)
	}
}

func TestLedgerMatchesCompletionByActiveFormAndNumber(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  true,
		Todos: []TodoItem{
			{Content: "Add parser", Status: "in_progress", ActiveForm: "Adding parser"},
			{Content: "Wire parser", Status: "in_progress"},
		},
	})

	current := []TodoItem{
		{Content: "Add parser", Status: "completed", ActiveForm: "Adding parser"},
		{Content: "Wire parser", Status: "in_progress"},
	}
	ledger.Record(Receipt{ToolName: "complete_step", Success: true, Step: "Adding parser"})
	missing, hasBaseline := ledger.UnverifiedCompletedTodos(current)
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline")
	}
	if len(missing) != 0 {
		t.Fatalf("activeForm complete_step should authorize completion, missing = %+v", missing)
	}

	current = []TodoItem{
		{Content: "Add parser", Status: "completed", ActiveForm: "Adding parser"},
		{Content: "Wire parser", Status: "completed"},
	}
	ledger.Record(Receipt{ToolName: "complete_step", Success: true, Step: "2"})
	missing, hasBaseline = ledger.UnverifiedCompletedTodos(current)
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline")
	}
	if len(missing) != 0 {
		t.Fatalf("numeric complete_step should authorize completion, missing = %+v", missing)
	}
}

func TestLedgerNumericCompleteStepDoesNotAuthorizeReplacedTodo(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  true,
		Todos:    []TodoItem{{Content: "Add parser", Status: "in_progress"}},
	})
	ledger.Record(Receipt{ToolName: "complete_step", Success: true, Step: "1"})

	missing, hasBaseline := ledger.UnverifiedCompletedTodos([]TodoItem{
		{Content: "Ship parser", Status: "completed"},
	})
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline")
	}
	if len(missing) != 1 || missing[0].Content != "Ship parser" {
		t.Fatalf("numeric complete_step should not authorize a replaced todo, missing = %+v", missing)
	}
}

func TestLedgerNumericCompleteStepFollowsReorderedSignedTodo(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{
		ToolName: "todo_write",
		Success:  true,
		Todos: []TodoItem{
			{Content: "Add parser", Status: "in_progress"},
			{Content: "Write tests", Status: "pending"},
		},
	})
	ledger.Record(Receipt{ToolName: "complete_step", Success: true, Step: "1"})

	missing, hasBaseline := ledger.UnverifiedCompletedTodos([]TodoItem{
		{Content: "Write tests", Status: "pending"},
		{Content: "Add parser", Status: "completed"},
	})
	if !hasBaseline {
		t.Fatal("expected prior todo_write baseline")
	}
	if len(missing) != 0 {
		t.Fatalf("numeric complete_step should follow the signed todo identity after reorder, missing = %+v", missing)
	}
}

func TestLedgerNoBaselineDoesNotConstrainCompletedTodos(t *testing.T) {
	ledger := NewLedger()
	missing, hasBaseline := ledger.UnverifiedCompletedTodos([]TodoItem{
		{Content: "Add parser", Status: "completed"},
	})

	if hasBaseline {
		t.Fatal("empty ledger should not report a prior todo_write baseline")
	}
	if len(missing) != 0 {
		t.Fatalf("no baseline should not report missing completions, got %+v", missing)
	}
}

// 行空板会话实测的两类 miss:引用相对路径 vs 收据绝对路径;复述命令改引号/空白。
// 宽容匹配要认这些,但凭空声称仍必须拒。
func TestLedgerTolerantPathAndCommandMatching(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Receipt{ToolName: "write_file", Success: true, Write: true,
		Paths: []string{"/Users/x/hardware/esp32_snake_web/unihiker_kaleidoscope/src/main.py"}})
	ledger.Record(Receipt{ToolName: "bash", Success: true,
		Command: `python3 -c 'py_compile.compile("src/main.py", doraise=True)'`})

	// 相对路径(段边界后缀)应命中
	if !ledger.HasSuccessfulWrite([]string{"unihiker_kaleidoscope/src/main.py"}) {
		t.Fatal("相对路径引用应匹配绝对路径收据")
	}
	if !ledger.HasSuccessfulWrite([]string{"src/main.py"}) {
		t.Fatal("更短的段后缀也应匹配")
	}
	// 非段边界的碰瓷不能命中(ain.py 不是路径段)
	if ledger.HasSuccessfulWrite([]string{"ain.py"}) {
		t.Fatal("非路径段后缀不应匹配")
	}
	// 完全无关的路径必拒
	if ledger.HasSuccessfulWrite([]string{"other_project/app.py"}) {
		t.Fatal("凭空声称的路径不应匹配")
	}

	// 命令:引号差异(单↔双)+ 空白压缩应命中
	if !ledger.HasSuccessfulCommand(`python3  -c "py_compile.compile('src/main.py', doraise=True)"`) {
		t.Fatal("引号/空白差异的复述应匹配")
	}
	// 截短复述(归一化后是真实命令的子串,且超过长度护栏)应命中
	if !ledger.HasSuccessfulCommand("py_compile.compile(src/main.py, doraise=True)") {
		t.Fatal("截短复述应匹配")
	}
	// 凭空编造的命令必拒
	if ledger.HasSuccessfulCommand("pytest tests/ -v") {
		t.Fatal("没运行过的命令不应匹配")
	}
}
