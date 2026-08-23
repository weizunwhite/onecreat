package cli

// 材料作业的沙箱 e2e:不花真钱、不碰真实课题目录。
//
// 造一个假的 OpenAI 兼容端点(httptest + SSE),脚本化地让"模型"发一次 write_file
// 工具调用,然后把 ControllerRunner 整条链跑通:boot.Build → 门禁 → 工具执行 →
// 事件流 → WrittenFiles。同一套夹具再跑一遍越界写,断言门禁真的挡住了、文件真的
// 没被创建、产物清单里也没有它。
//
// 与 internal/agent 的 cachehit e2e 同一套手法(假 provider + SSE 分片),区别是
// 这里走完整装配路线,验的是"装配层把门禁装对了没有",不是模型循环本身。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/workflow"
)

// ---------- 假模型端点 ----------

type fakeDelta struct {
	Content   string         `json:"content,omitempty"`
	ToolCalls []fakeToolCall `json:"tool_calls,omitempty"`
}

type fakeToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type fakeChoice struct {
	Delta        fakeDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type fakeChunk struct {
	Choices []fakeChoice `json:"choices"`
	Usage   *fakeUsage   `json:"usage,omitempty"`
}

type fakeUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// scriptCall 是脚本里的一次工具调用。
type scriptCall struct {
	name string
	args string
}

// scriptedModel 是脚本化的假模型:第 2k 轮请求发 calls[k] 那次工具调用,
// 发完就在下一轮给最终答复。一个 scriptedModel 可以喂完整条材料链
// (每种材料两轮请求:一轮出工具、一轮收尾)。
type scriptedModel struct {
	mu    sync.Mutex
	round int
	calls []scriptCall
	final string
}

func (m *scriptedModel) handler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	m.mu.Lock()
	round := m.round
	m.round++
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	f, ok := w.(http.Flusher)
	if !ok {
		return
	}
	write := func(c fakeChunk) {
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		f.Flush()
	}
	if idx := round / 2; round%2 == 0 && idx < len(m.calls) {
		tc := fakeToolCall{Index: 0, ID: fmt.Sprintf("call_%d", idx+1), Type: "function"}
		tc.Function.Name = m.calls[idx].name
		tc.Function.Arguments = m.calls[idx].args
		write(fakeChunk{Choices: []fakeChoice{{Delta: fakeDelta{ToolCalls: []fakeToolCall{tc}}}}})
		reason := "tool_calls"
		write(fakeChunk{Choices: []fakeChoice{{FinishReason: &reason}}})
	} else {
		write(fakeChunk{Choices: []fakeChoice{{Delta: fakeDelta{Content: m.final}}}})
		reason := "stop"
		write(fakeChunk{Choices: []fakeChoice{{FinishReason: &reason}}})
	}
	write(fakeChunk{Usage: &fakeUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	f.Flush()
}

// ---------- 沙箱夹具 ----------

// sandboxProject 造一个七目录齐全的沙箱项目,并把进程的配置/家目录都指到临时目录,
// 免得测试读到开发机上的真配置、真 skill、真会话目录。
func sandboxProject(t *testing.T, modelURL string) string {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	state := filepath.Join(base, "state")
	for _, d := range []string{home, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("REASONIX_CONFIG_DIR", state)
	t.Setenv("ONECREAT_TEST_KEY", "sk-sandbox-not-a-real-key")
	// 账号网关的三个 env 必须是空的,否则装配会走平台网关而不是这个假端点。
	t.Setenv("ONECREAT_GATEWAY_URL", "")
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "")

	root := filepath.Join(base, "课题", projectsSubDir, "P26C-020_飞鸟志")
	for _, b := range []string{
		"01_立项定题", "02_平台材料", "03_研发工作区",
		"04_课堂记录", "05_上课照片", "06_项目协作", "07_竞赛提交",
	} {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "project.json"), `{
  "code": "P26C-020",
  "short_name": "飞鸟志",
  "full_name": "校园鸟类声纹多样性监测站",
  "line": "C",
  "stage": 3
}`)
	// 沙箱项目自己的 onecreat.toml:模型指向假端点,顺手关掉会起子进程的两个服务。
	writeFile(t, filepath.Join(root, "onecreat.toml"), fmt.Sprintf(`default_model = "sandbox"

[[providers]]
name = "sandbox"
kind = "openai"
base_url = %q
model = "sandbox-model"
api_key_env = "ONECREAT_TEST_KEY"

[lsp]
enabled = false

[codegraph]
enabled = false
auto_install = false
`, modelURL))
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// recordSink 收下整轮事件,供断言用。
type recordSink struct {
	mu     sync.Mutex
	events []event.Event
}

func (s *recordSink) Emit(e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// toolResult 取某个工具的 ToolResult 事件:errMsg 是记账用的短原因,
// output 是真正回灌给模型的那段文字(门禁的人话理由在这里面)。
func (s *recordSink) toolResult(name string) (errMsg, output string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Kind == event.ToolResult && e.Tool.Name == name {
			return e.Tool.Err, e.Tool.Output, true
		}
	}
	return "", "", false
}

func (s *recordSink) phases() []event.Workflow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Workflow
	for _, e := range s.events {
		if e.Kind == event.WorkflowMaterial {
			out = append(out, e.Workflow)
		}
	}
	return out
}

// ---------- e2e ----------

// TestControllerRunnerWritesIntoBucket:模型发一次 write_file 到 02_平台材料,
// 文件真的落在桶里,WrittenFiles 是项目根下的相对路径,最终回复被采到。
func TestControllerRunnerWritesIntoBucket(t *testing.T) {
	model := &scriptedModel{
		calls: []scriptCall{{
			name: "write_file",
			args: `{"path":"02_平台材料/技术方案.md","content":"# 技术方案\n沙箱产物。"}`,
		}},
		final: "技术方案已写入 02_平台材料/技术方案.md。",
	}
	srv := httptest.NewServer(http.HandlerFunc(model.handler))
	defer srv.Close()

	root := sandboxProject(t, srv.URL)
	sink := &recordSink{}
	runner := &ControllerRunner{Sink: sink}

	res, err := runner.Run(context.Background(), workflow.RunRequest{
		ProjectDir:   root,
		MaterialType: "技术方案",
		Prompt:       "把技术方案写进 02_平台材料。",
		Timeout:      60 * time.Second,
	})
	if err != nil {
		t.Fatalf("材料作业失败:%v", err)
	}

	want := "02_平台材料/技术方案.md"
	if len(res.WrittenFiles) != 1 || res.WrittenFiles[0] != want {
		t.Fatalf("WrittenFiles = %v, want [%s]", res.WrittenFiles, want)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(want))); err != nil {
		t.Fatalf("产物没真的落到桶里:%v", err)
	}
	if !strings.Contains(res.FinalText, "技术方案已写入") {
		t.Fatalf("FinalText = %q,没采到模型最后那段回复", res.FinalText)
	}

	phases := sink.phases()
	if len(phases) != 2 || phases[0].Phase != workflowPhaseStarted || phases[1].Phase != workflowPhaseSucceeded {
		t.Fatalf("workflow_material 事件 = %+v,want started + succeeded", phases)
	}
	if phases[1].Material != "技术方案" {
		t.Fatalf("事件没带材料名:%+v", phases[1])
	}
}

// TestControllerRunnerDeniesOutsideWorkspace:越界写必须被门禁拒掉 ——
// 工具结果带错误、文件不存在、产物清单里也没有它。
//
// 这条是拍板 1 的端到端版:headless 会话里没有人能答 ask,所以越界只能是 deny。
func TestControllerRunnerDeniesOutsideWorkspace(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "越界产物.md")
	model := &scriptedModel{
		calls: []scriptCall{{
			name: "write_file",
			args: fmt.Sprintf(`{"path":%q,"content":"不该写成功"}`, outside),
		}},
		final: "写不了,我停下来说明情况。",
	}
	srv := httptest.NewServer(http.HandlerFunc(model.handler))
	defer srv.Close()

	root := sandboxProject(t, srv.URL)
	sink := &recordSink{}
	runner := &ControllerRunner{Sink: sink}

	res, err := runner.Run(context.Background(), workflow.RunRequest{
		ProjectDir:   root,
		MaterialType: "技术方案",
		Prompt:       "把技术方案写到项目外面去。",
		Timeout:      60 * time.Second,
	})
	if err != nil {
		t.Fatalf("越界被拒不该让整轮报错(拒绝会回灌给模型):%v", err)
	}
	if len(res.WrittenFiles) != 0 {
		t.Fatalf("WrittenFiles = %v,被拒的调用不该算进产物", res.WrittenFiles)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("工作区外的文件被创建了:%s", outside)
	}
	errMsg, output, ok := sink.toolResult("write_file")
	if !ok {
		t.Fatal("没收到 write_file 的 ToolResult 事件")
	}
	if errMsg == "" {
		t.Fatal("被拒的工具调用必须带错误标记,否则前端会渲染成成功")
	}
	if !strings.Contains(output, "材料作业门禁") {
		t.Fatalf("回灌给模型的结果里没有门禁的拒绝理由:%q", output)
	}
}

// TestControllerRunnerRejectsBadInput:输入不合法时不装配、不跑模型。
func TestControllerRunnerRejectsBadInput(t *testing.T) {
	runner := &ControllerRunner{}
	if _, err := runner.Run(context.Background(), workflow.RunRequest{
		ProjectDir: filepath.Join(t.TempDir(), "不存在"), Prompt: "x",
	}); err == nil {
		t.Fatal("项目目录不存在时必须报错")
	}
	if _, err := runner.Run(context.Background(), workflow.RunRequest{
		ProjectDir: t.TempDir(), Prompt: "   ",
	}); err == nil {
		t.Fatal("prompt 为空时必须报错,不能跑空轮")
	}
}

// TestControllerRunnerRealModel 是**门控**的真模型 e2e:默认跳过,只有显式
// ONECREAT_WORKFLOW_E2E=1 才跑,而且只在临时沙箱项目里跑,绝不碰真实课题目录。
//
// 它用的是本机既有配置的默认模型(key 从环境 / .env 解析,全程不打印),验的是
// "真模型 + 材料作业门禁"能不能把一份产物写进 02_平台材料。跑一次要花钱,所以
// 它不在常规 go test 里。
func TestControllerRunnerRealModel(t *testing.T) {
	if os.Getenv("ONECREAT_WORKFLOW_E2E") != "1" {
		t.Skip("真模型 e2e 默认跳过;要跑就 ONECREAT_WORKFLOW_E2E=1 go test ./internal/cli/ -run RealModel")
	}
	// 沙箱项目:七目录 + project.json,但不改 HOME/配置目录 —— 本机的真配置与
	// ~/.env 正是这条用例要用的凭证来源。
	root := filepath.Join(t.TempDir(), "P26C-999_沙箱")
	for _, b := range []string{
		"01_立项定题", "02_平台材料", "03_研发工作区",
		"04_课堂记录", "05_上课照片", "06_项目协作", "07_竞赛提交",
	} {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "project.json"),
		`{"code":"P26C-999","short_name":"沙箱","full_name":"沙箱验收项目","line":"C","stage":3}`)

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("读不到本机配置,跳过:%v", err)
	}
	if err := cfg.Validate(cfg.DefaultModel); err != nil {
		// Validate 只会提到环境变量名,不会带出 key 本身。
		t.Skipf("默认模型不可用,跳过:%v", err)
	}

	runner := &ControllerRunner{}
	res, err := runner.Run(context.Background(), workflow.RunRequest{
		ProjectDir:   root,
		MaterialType: "技术方案",
		Prompt: "用 write_file 在 02_平台材料/技术方案.md 里写一份 200 字以内的技术方案提纲," +
			"主题是校园鸟类声纹监测。不要问任何问题,缺信息就合理假设。写完直接结束。",
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("真模型材料作业失败:%v", err)
	}
	if len(res.WrittenFiles) == 0 {
		t.Fatalf("真模型没写出任何产物;最后一段回复:%s", res.FinalText)
	}
	for _, f := range res.WrittenFiles {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Fatalf("产物 %s 记了账却不在盘上:%v", f, err)
		}
	}
	t.Logf("真模型产物:%v", res.WrittenFiles)
}
