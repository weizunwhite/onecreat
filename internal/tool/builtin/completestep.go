package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/evidence"
	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(completeStep{}) }

// completeStep records an evidence-backed completion of one step of an approved
// plan. Like todo_write it has no host side effects — the claim and its evidence
// live in the call's args, which a frontend renders as a signed-off step. Its
// reason for existing is the enforcement in Execute: a completion with no evidence
// is rejected, so the model can't flip a step to "done" without showing why it is
// done (the verification it ran, the diff/files it changed, or a manual check).
// It complements todo_write — todo_write keeps the list moving (one item
// in_progress), complete_step is the formal sign-off of a finished step.
type completeStep struct{}

type stepEvidence struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Command string   `json:"command,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// validEvidenceKinds are the evidence forms a completion may cite. "checkpoint"
// (main's fourth kind) is omitted — v2 has no checkpoint system.
var validEvidenceKinds = map[string]bool{
	"verification": true, // a command/test was run; cite it and its outcome
	"diff":         true, // a concrete code change; cite what changed
	"files":        true, // files created/edited/inspected; cite the paths
	"manual":       true, // a manual check; cite what was confirmed and how
}

func (completeStep) Name() string { return "complete_step" }

func (completeStep) Description() string {
	return "Record the evidence-backed completion of ONE step of an approved plan. Call it as you finish each step instead of silently moving on: it signs the step off with PROOF it is done — the verification you ran (command + result), the diff/files you changed, or a manual check. A completion with no evidence is REJECTED, so don't claim a step is done until you can show why. Keep the task list moving with todo_write (set the next step in_progress); use complete_step for the formal, evidenced sign-off of the finished one. Fields: `step` (which step — its title or number, matching the task list), `result` (what is now true/changed), `evidence` (≥1 item, each with `kind` = verification|diff|files|manual and a `summary`, plus optional `command`/`paths`), and optional `notes`."
}

func (completeStep) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "step":{"type":"string","description":"Which plan step this completes — its title or number, matching the task list."},
  "result":{"type":"string","description":"What is now true or changed as a result of finishing this step."},
  "evidence":{
    "type":"array",
    "minItems":1,
    "description":"Proof the step is done. At least one item is required.",
    "items":{
      "type":"object",
      "properties":{
        "kind":{"type":"string","enum":["verification","diff","files","manual"],"description":"verification = a command/test was run; diff = a concrete code change; files = files created/edited/inspected; manual = a manual check."},
        "summary":{"type":"string","description":"The evidence itself: the test result, what the diff does, or what was confirmed."},
        "command":{"type":"string","description":"The command run, for verification evidence (e.g. \"go test ./...\")."},
        "paths":{"type":"array","items":{"type":"string"},"description":"Files this evidence refers to."}
      },
      "required":["kind","summary"]
    }
  },
  "notes":{"type":"string","description":"Optional caveats, follow-ups, or anything deferred."}
},
"required":["step","result","evidence"]
}`)
}

// ReadOnly is true: complete_step only records a claim (no filesystem or process
// effect), so it never needs approval and stays available alongside todo_write.
func (completeStep) ReadOnly() bool { return true }

func (completeStep) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Step     string         `json:"step"`
		Result   string         `json:"result"`
		Evidence []stepEvidence `json:"evidence"`
		Notes    string         `json:"notes"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Step) == "" {
		return "", fmt.Errorf("step 必填——写明你正在完成计划里的哪一步")
	}
	if strings.TrimSpace(p.Result) == "" {
		return "", fmt.Errorf("result 必填——说明这一步完成后什么已成为事实")
	}
	if len(p.Evidence) == 0 {
		return "", fmt.Errorf("至少需要 1 条 evidence——不能没有证据就标记完成(运行过的校验、本轮的代码改动,或人工确认)")
	}
	kinds := make([]string, 0, len(p.Evidence))
	for i, e := range p.Evidence {
		if !validEvidenceKinds[e.Kind] {
			return "", fmt.Errorf("evidence %d:kind %q 无效(只能是 verification|diff|files|manual)", i+1, e.Kind)
		}
		if strings.TrimSpace(e.Summary) == "" {
			return "", fmt.Errorf("evidence %d:summary 必填——证据内容写在 summary 里,不能只给 kind", i+1)
		}
		kinds = append(kinds, e.Kind)
	}

	hostVerified, manualUnverified, err := verifyStepEvidence(ctx, p.Evidence)
	if err != nil {
		return "", err
	}
	todoMatch, hasTodo, err := verifyTodoStep(ctx, p.Step)
	if err != nil {
		return "", err
	}
	hostStatus := ""
	if _, ok := evidence.FromContext(ctx); ok {
		hostStatus = fmt.Sprintf(" 主机校验:已验证 %d,人工/未验证 %d。", hostVerified, manualUnverified)
	}
	todoStatus := ""
	if hasTodo {
		todoStatus = fmt.Sprintf(" 对应 todo 第 %d 项。", todoMatch.Index)
	}
	return fmt.Sprintf("步骤 %q 已签收:%d 条 evidence [%s]。%s 接着用 todo_write 把下一步标为 in_progress。",
		p.Step, len(p.Evidence), strings.Join(kinds, ", "), hostStatus+todoStatus), nil
}

func verifyStepEvidence(ctx context.Context, items []stepEvidence) (hostVerified int, manualUnverified int, err error) {
	ledger, ok := evidence.FromContext(ctx)
	if !ok {
		return 0, 0, nil
	}
	for i, e := range items {
		switch e.Kind {
		case "verification":
			command := strings.TrimSpace(e.Command)
			if command == "" {
				return 0, 0, fmt.Errorf("evidence %d:verification 证据必须带 command(你本轮真实运行过的命令或调用过的工具名)", i+1)
			}
			if !ledger.HasSuccessfulCommand(command) {
				return 0, 0, fmt.Errorf("evidence %d:verification command %q 在本轮没有匹配到成功的执行记录——command 只能填本轮真实运行成功的 bash 命令或调用过的工具名,不能凭空声称", i+1, command)
			}
			hostVerified++
		case "diff":
			if len(e.Paths) == 0 {
				return 0, 0, fmt.Errorf("evidence %d:diff 证据必须带 paths(本轮改了哪些文件)", i+1)
			}
			if !ledger.HasSuccessfulWrite(e.Paths) {
				return 0, 0, fmt.Errorf("evidence %d:diff 的 paths 在本轮没有成功写入记录——只能引用本轮真实改过的文件", i+1)
			}
			hostVerified++
		case "files":
			if len(e.Paths) == 0 {
				return 0, 0, fmt.Errorf("evidence %d:files 证据必须带 paths(涉及哪些文件)", i+1)
			}
			if !ledger.HasSuccessfulReadOrWrite(e.Paths) {
				return 0, 0, fmt.Errorf("evidence %d:files 的 paths 在本轮没有成功读/写记录——只能引用本轮真实读过或改过的文件", i+1)
			}
			hostVerified++
		case "manual":
			manualUnverified++
		}
	}
	return hostVerified, manualUnverified, nil
}

func verifyTodoStep(ctx context.Context, step string) (evidence.TodoStepMatch, bool, error) {
	ledger, ok := evidence.FromContext(ctx)
	if !ok {
		return evidence.TodoStepMatch{}, false, nil
	}
	match, hasTodo := ledger.MatchLatestTodoStep(step)
	if !hasTodo {
		return evidence.TodoStepMatch{}, false, nil
	}
	if !match.Found {
		return evidence.TodoStepMatch{}, true, fmt.Errorf("step %q 在本轮 todo_write 列表中找不到对应项——step 要与任务列表条目的文字对应", step)
	}
	switch match.Status {
	case "in_progress", "completed":
		return match, true, nil
	case "":
		return evidence.TodoStepMatch{}, true, fmt.Errorf("step %q 对应 todo 第 %d 项(%q),但它还是 pending——先用 todo_write 把它标为 in_progress 再签收", step, match.Index, match.Content)
	default:
		return evidence.TodoStepMatch{}, true, fmt.Errorf("step %q 对应 todo 第 %d 项(%q),但状态是 %q;complete_step 只接受 in_progress 或 completed", step, match.Index, match.Content, match.Status)
	}
}
