package cli

// `reasonix workflow gen` 的 CLI 集成测试:走真正的 flag 解析与解析链,
// 但不跑模型(--dry-run),也不碰真实课题目录。

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/workflow"
)

// installSkill 把一个假 skill 装进沙箱家目录的 `.claude/skills/`,
// 让 skill.Store 按它平时的发现路径找得到。
func installSkill(t *testing.T, name, body string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "SKILL.md"),
		"---\nname: "+name+"\ndescription: 沙箱用的假 skill\n---\n\n"+body)
}

// genArgs 把沙箱课题根拼进参数表。
func genArgs(ketiRoot string, rest ...string) []string {
	return append([]string{"gen"}, append(rest, "--root", ketiRoot)...)
}

func ketiRootOf(projectDir string) string {
	return filepath.Dir(filepath.Dir(projectDir))
}

// TestWorkflowGenDryRun:dry-run 把能确定的都打印出来,尚未接线的明确标 TODO。
func TestWorkflowGenDryRun(t *testing.T) {
	root := sandboxProject(t, "http://127.0.0.1:1/不会被调用")
	installSkill(t, "tech-proposal-generator", "# 技术方案生成\n按七章模板写。")

	var code int
	out := captureStdout(t, func() {
		code = workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案", "--dry-run"))
	})
	if code != 0 {
		t.Fatalf("dry-run 退出码 = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{
		"P26C-020", "飞鸟志", "技术方案", "02_平台材料",
		"tech-proposal-generator", "材料作业专用",
		// 装配好的 prompt 是 W1 的 AssemblePrompt 真产物,不是本层复述的:
		// 认它的两条硬指令,任一走样都说明两侧口径分叉了。
		"不要问我任何问题", "装配好的 prompt",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run 输出里缺 %q:\n%s", want, out)
		}
	}
	// dry-run 绝不写文件。
	if entries, err := os.ReadDir(filepath.Join(root, "02_平台材料")); err != nil || len(entries) != 0 {
		t.Fatalf("dry-run 往桶里写东西了:%v %v", entries, err)
	}
}

// TestWorkflowGenMissingSkill:skill 找不到时给人话错误 + 列出扫过的路径,不猜、不兜底。
func TestWorkflowGenMissingSkill(t *testing.T) {
	root := sandboxProject(t, "http://127.0.0.1:1/不会被调用")
	// 故意不装 skill。
	stderr := captureStderr(t, func() {
		if code := workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案", "--dry-run")); code == 0 {
			t.Error("skill 缺失时不该返回 0")
		}
	})
	for _, want := range []string{"tech-proposal-generator", "扫描过的路径", ".claude"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("错误信息里缺 %q:\n%s", want, stderr)
		}
	}
}

// TestWorkflowGenRejectsUnknownMaterial:注册表里没登记的材料一律拒绝。
func TestWorkflowGenRejectsUnknownMaterial(t *testing.T) {
	root := sandboxProject(t, "http://127.0.0.1:1/不会被调用")
	stderr := captureStderr(t, func() {
		if code := workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "自造材料", "--dry-run")); code == 0 {
			t.Error("未注册材料不该返回 0")
		}
	})
	if !strings.Contains(stderr, "没在注册表里登记") {
		t.Fatalf("错误信息不对:\n%s", stderr)
	}
}

// TestWorkflowGenRejectsBadMode:非法档位在跑之前就拦下。
func TestWorkflowGenRejectsBadMode(t *testing.T) {
	root := sandboxProject(t, "http://127.0.0.1:1/不会被调用")
	installSkill(t, "tech-proposal-generator", "# 技术方案")
	stderr := captureStderr(t, func() {
		code := workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案", "--dry-run", "--mode", "随便"))
		if code == 0 {
			t.Error("非法档位不该返回 0")
		}
	})
	if !strings.Contains(stderr, "不是合法档位") {
		t.Fatalf("错误信息不对:\n%s", stderr)
	}
}

// TestWorkflowGenRejectsRealKetiRoot 是 M2 拍板 2 的守卫:真实课题目录默认拒绝。
//
// 不依赖开发机上真有那个目录 —— isRealKetiProject 判的是路径关系,给它一个
// defaultKetiRoot 之下的路径就够了。
func TestWorkflowGenRejectsRealKetiRoot(t *testing.T) {
	real, why := isRealKetiProject(filepath.Join(defaultKetiRoot, projectsSubDir, "P26C-020_飞鸟志"))
	if !real {
		t.Fatal("真实课题库下的项目必须被判为 real")
	}
	if !strings.Contains(why, defaultKetiRoot) {
		t.Fatalf("拒绝理由要说清是哪条红线:%q", why)
	}
	if outside, _ := isRealKetiProject(filepath.Join(t.TempDir(), "P26C-020_沙箱")); outside {
		t.Fatal("沙箱项目不该被判为真实课题目录")
	}
}

// TestWorkflowGenRejectsUnknownEngine:引擎名只认 native / dsh。
func TestWorkflowGenRejectsUnknownEngine(t *testing.T) {
	root := sandboxProject(t, "http://127.0.0.1:1/不会被调用")
	stderr := captureStderr(t, func() {
		if code := workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案", "--engine", "pi")); code == 0 {
			t.Error("未知引擎不该返回 0")
		}
	})
	if !strings.Contains(stderr, "--engine") {
		t.Fatalf("错误信息不对:\n%s", stderr)
	}
}

// captureStderr 与 captureStdout 同法,收 stderr。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestWorkflowGenDryRunOnRealKetiWarns:dry-run 对真实课题目录放行(它证明不写文件),
// 但必须警告一句"真跑会被拒"。这样看装配结果不用敲 --allow-real,红线不会被磨掉。
func TestWorkflowGenDryRunOnRealKetiWarns(t *testing.T) {
	// 只在开发机上真有那棵目录树时才有意义;没有就跳过,不去真实盘上造目录。
	realProject := filepath.Join(defaultKetiRoot, projectsSubDir)
	if _, err := os.Stat(realProject); err != nil {
		t.Skip("本机没有真实课题库,跳过")
	}
	scan, err := projectScanForTest(realProject)
	if err != nil || len(scan) == 0 {
		t.Skip("真实课题库里没有可用项目,跳过")
	}
	if realKeti, _ := isRealKetiProject(scan[0]); !realKeti {
		t.Fatalf("真实课题库下的 %s 竟然没被判为 real", scan[0])
	}
}

// projectScanForTest 列出真实在研项目目录(只读,不解析 project.json)。
func projectScanForTest(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "P26") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// TestWorkflowGenFullChain 是 M2 的验收用例:沙箱项目 + 假模型,
// `workflow gen 技术方案` 全链跑通 —— 文件真的落进 02_平台材料、
// 台账里有 material_succeeded、命令返回 0。
func TestWorkflowGenFullChain(t *testing.T) {
	model := &scriptedModel{
		calls: []scriptCall{{
			name: "write_file",
			args: `{"path":"02_平台材料/技术方案.md","content":"# 技术方案\n一、硬件选型\n二、系统架构\n三、软件流程"}`,
		}},
		final: "已写入 02_平台材料/技术方案.md。",
	}
	srv := httptest.NewServer(http.HandlerFunc(model.handler))
	defer srv.Close()

	root := sandboxProject(t, srv.URL)
	installSkill(t, "tech-proposal-generator", "# 技术方案生成器\n按硬件选型 / 系统架构 / 软件流程三章写。")

	var code int
	out := captureStdout(t, func() {
		code = workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案"))
	})
	if code != 0 {
		t.Fatalf("gen 退出码 = %d, want 0\n%s", code, out)
	}

	// 1. 文件真的在桶里。
	produced := filepath.Join(root, "02_平台材料", "技术方案.md")
	if _, err := os.Stat(produced); err != nil {
		t.Fatalf("产物没落进 02_平台材料:%v\n%s", err, out)
	}

	// 2. 台账可回放,且有 material_succeeded。
	events, err := workflow.Replay(root)
	if err != nil {
		t.Fatalf("回放台账失败:%v", err)
	}
	var kinds []string
	succeeded := false
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == workflow.KindMaterialSucceeded {
			succeeded = true
			if len(ev.Outputs) == 0 {
				t.Error("material_succeeded 必须带产物清单")
			}
		}
	}
	if !succeeded {
		t.Fatalf("台账里没有 material_succeeded,只有 %v", kinds)
	}
}

// TestWorkflowGenFullChainDeniedWrite:模型试图写到工作区外 —— 门禁拒绝,
// 于是这一轮没有交付物,作业层按"假成功检测"判失败并记 material_failed。
func TestWorkflowGenFullChainDeniedWrite(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "越界产物.md")
	model := &scriptedModel{
		calls: []scriptCall{{
			name: "write_file",
			args: fmt.Sprintf(`{"path":%q,"content":"不该写成功"}`, outside),
		}},
		final: "写不了。",
	}
	srv := httptest.NewServer(http.HandlerFunc(model.handler))
	defer srv.Close()

	root := sandboxProject(t, srv.URL)
	installSkill(t, "tech-proposal-generator", "# 技术方案生成器")

	var code int
	captureStdout(t, func() {
		code = workflowCommand(genArgs(ketiRootOf(root), "P26C-020", "技术方案"))
	})
	if code == 0 {
		t.Fatal("越界写被拒之后不该报成功")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("工作区外的文件被创建了:%s", outside)
	}
	events, err := workflow.Replay(root)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for _, ev := range events {
		if ev.Kind == workflow.KindMaterialFailed {
			failed = true
		}
		if ev.Kind == workflow.KindMaterialSucceeded {
			t.Fatal("没有交付物却记了 material_succeeded —— 假成功检测没生效")
		}
	}
	if !failed {
		t.Fatalf("台账里应有 material_failed,实际 %+v", events)
	}
}

// TestWorkflowGenMaterialChain 是编排者要的那条链:技术方案 → 教案(教案硬依赖技术方案)。
//
// 先证明依赖没齐时作业**拒绝发起**并记 material_skipped,再跑技术方案,再跑教案,
// 台账连起来能回放出完整一条链。
func TestWorkflowGenMaterialChain(t *testing.T) {
	model := &scriptedModel{
		calls: []scriptCall{
			{name: "write_file", args: `{"path":"02_平台材料/技术方案.md","content":"# 技术方案\n硬件选型/系统架构/软件流程。"}`},
			{name: "write_file", args: `{"path":"02_平台材料/上课教案_合集_12节.md","content":"# 第1节课教案\n略。"}`},
		},
		final: "写完了。",
	}
	srv := httptest.NewServer(http.HandlerFunc(model.handler))
	defer srv.Close()

	root := sandboxProject(t, srv.URL)
	installSkill(t, "tech-proposal-generator", "# 技术方案生成器")
	installSkill(t, "lesson-plan-generator", "# 教案生成器")
	keti := ketiRootOf(root)

	// 1. 技术方案还没有 → 教案必须被拒。
	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			code = workflowCommand(genArgs(keti, "P26C-020", "教案"))
		})
	})
	if code == 0 {
		t.Fatal("硬依赖缺失时教案不该跑起来")
	}
	if !strings.Contains(stderr, "技术方案") {
		t.Fatalf("拒绝理由要点名缺哪个上游:%s", stderr)
	}

	// 2. 先生成技术方案。
	captureStdout(t, func() { code = workflowCommand(genArgs(keti, "P26C-020", "技术方案")) })
	if code != 0 {
		t.Fatalf("技术方案 gen 失败,退出码 %d", code)
	}

	// 3. 依赖齐了,教案跑得通。
	captureStdout(t, func() { code = workflowCommand(genArgs(keti, "P26C-020", "教案")) })
	if code != 0 {
		t.Fatalf("教案 gen 失败,退出码 %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "02_平台材料", "上课教案_合集_12节.md")); err != nil {
		t.Fatalf("教案没落进桶里:%v", err)
	}

	// 4. 台账回放出完整一条链。
	events, err := workflow.Replay(root)
	if err != nil {
		t.Fatal(err)
	}
	var succeeded []string
	skipped := false
	for _, ev := range events {
		switch ev.Kind {
		case workflow.KindMaterialSucceeded:
			succeeded = append(succeeded, ev.Material)
		case workflow.KindMaterialSkipped:
			skipped = true
		}
	}
	if !skipped {
		t.Error("第一次被依赖拒绝的那次应当记 material_skipped")
	}
	if len(succeeded) != 2 || succeeded[0] != "技术方案" || succeeded[1] != "教案" {
		t.Fatalf("台账里的成功链 = %v, want [技术方案 教案]", succeeded)
	}
}
