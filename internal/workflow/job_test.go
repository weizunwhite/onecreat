package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

// fakeRunner 是执行层的替身：按轮次回报"这一轮写了哪些文件"（相对项目根），
// 并真的把它们写到盘上，让 Verify / Collect 面对真实目录。
type fakeRunner struct {
	projDir string
	files   [][]string // 第 i 轮写哪些文件；越界当作一个都没写（假成功）
	errs    []error    // 第 i 轮直接报错；越界当作没错
	reqs    []RunRequest
}

func (f *fakeRunner) Run(_ context.Context, req RunRequest) (RunResult, error) {
	i := len(f.reqs)
	f.reqs = append(f.reqs, req)
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	var written []string
	if i < len(f.files) {
		written = f.files[i]
		for _, rel := range written {
			p := filepath.Join(f.projDir, filepath.FromSlash(rel))
			if e := os.MkdirAll(filepath.Dir(p), 0o755); e != nil {
				return RunResult{}, e
			}
			if e := os.WriteFile(p, []byte("假产物"), 0o644); e != nil {
				return RunResult{}, e
			}
		}
	}
	return RunResult{FinalText: "写完了", WrittenFiles: written}, err
}

// jobDeps 造一套单测用的 Deps：skill 正文是可断言的假正文，Vision 不接线。
func jobDeps(t *testing.T, r *fakeRunner) Deps {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Reg:    reg,
		Runner: r,
		SkillBody: func(name string) (string, error) {
			return "【" + name + " 技能正文】", nil
		},
	}
}

// newJob 造项目 + runner，返回项目目录与 runner。
func newJob(t *testing.T, files map[string]string) (string, *fakeRunner) {
	t.Helper()
	p := fakeProject(t, files)
	return p.Dir, &fakeRunner{projDir: p.Dir}
}

func journalKindsOf(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// TestRunMaterialRejectsMissingDeps：硬依赖缺失时作业根本不发起，并明说缺哪些。
func TestRunMaterialRejectsMissingDeps(t *testing.T) {
	dir, r := newJob(t, nil) // 空项目：没有技术方案
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "教案")
	if !errors.Is(err, ErrMissingDeps) {
		t.Fatalf("期望 ErrMissingDeps，实际 %v", err)
	}
	if res == nil || res.Err != err {
		t.Fatalf("JobResult.Err 必须与返回的 error 是同一个：%+v", res)
	}
	if !strings.Contains(err.Error(), "技术方案") {
		t.Errorf("错误里要点名缺的是技术方案：%v", err)
	}
	if len(r.reqs) != 0 {
		t.Errorf("依赖缺失时不许调 Runner，实际调了 %d 次", len(r.reqs))
	}
	if res.Attempts != 0 {
		t.Errorf("Attempts 应为 0，实际 %d", res.Attempts)
	}
	if got := journalKindsOf(res.Journal); len(got) != 1 || got[0] != KindMaterialSkipped {
		t.Errorf("台账应只有一条 material_skipped，实际 %v", got)
	}
	// 台账要真的落盘，不能只在返回值里。
	evs, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != KindMaterialSkipped {
		t.Errorf("盘上台账不对：%+v", evs)
	}
}

// TestRunMaterialRejectsConversational：对话型材料不走一键流水线，提示 M3。
func TestRunMaterialRejectsConversational(t *testing.T) {
	dir, r := newJob(t, nil)
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "立项")
	if !errors.Is(err, ErrConversational) {
		t.Fatalf("期望 ErrConversational，实际 %v", err)
	}
	if res != nil {
		t.Errorf("材料都没受理，不该给 JobResult：%+v", res)
	}
	if !strings.Contains(err.Error(), "M3") {
		t.Errorf("要提示 M3 才做：%v", err)
	}
}

// TestRunMaterialResolvesAlias：旧别名「研究日志」要归一到「研究日志框架」，
// 且交给 Runner 的 MaterialType 是归一后的正式类型。
func TestRunMaterialResolvesAlias(t *testing.T) {
	dir, r := newJob(t, map[string]string{
		"02_平台材料/技术方案_飞鸟志.md": "# 技术方案\n主控用 ESP32。\n",
	})
	r.files = [][]string{{"02_平台材料/飞鸟志_research_log_scaffold.json"}}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "研究日志")
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if res.Material != "研究日志框架" {
		t.Errorf("Material 应归一为研究日志框架，实际 %q", res.Material)
	}
	if r.reqs[0].MaterialType != "研究日志框架" {
		t.Errorf("RunRequest.MaterialType 应是正式类型，实际 %q", r.reqs[0].MaterialType)
	}
	if r.reqs[0].ProjectDir != dir {
		t.Errorf("RunRequest.ProjectDir 不对：%q", r.reqs[0].ProjectDir)
	}
}

// TestSoftDepInjectedOnlyWhenPresent：硬依赖必注入，软依赖存在才注入。
//
// 研究日志框架：deps=[技术方案]、soft_deps=[教案]。
func TestSoftDepInjectedOnlyWhenPresent(t *testing.T) {
	base := map[string]string{
		"02_平台材料/技术方案_飞鸟志.md": "# 技术方案\n主控用 ESP32。\n",
	}

	// ① 软依赖不存在：prompt 里只有技术方案那一段。
	dir, r := newJob(t, base)
	r.files = [][]string{{"02_平台材料/飞鸟志_research_log_scaffold.json"}}
	if _, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "研究日志框架"); err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	p := r.reqs[0].Prompt
	if !strings.Contains(p, "本项目已有的「技术方案」") || !strings.Contains(p, "主控用 ESP32") {
		t.Errorf("硬依赖没注入：\n%s", p)
	}
	if strings.Contains(p, "本项目已有的「教案」") {
		t.Errorf("软依赖不存在时不该注入：\n%s", p)
	}

	// ② 软依赖存在：两段都在。
	withSoft := map[string]string{}
	for k, v := range base {
		withSoft[k] = v
	}
	withSoft["02_平台材料/教案_第1课.md"] = "# 教案第1课\n第一节课做声音采集。\n"
	dir2, r2 := newJob(t, withSoft)
	r2.files = [][]string{{"02_平台材料/飞鸟志_research_log_scaffold.json"}}
	if _, err := RunMaterial(context.Background(), jobDeps(t, r2), dir2, "研究日志框架"); err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	p2 := r2.reqs[0].Prompt
	if !strings.Contains(p2, "本项目已有的「教案」") || !strings.Contains(p2, "第一节课做声音采集") {
		t.Errorf("软依赖存在时应注入：\n%s", p2)
	}
	// 硬依赖排在软依赖前面（越靠后越重视是给老师的额外要求留的位置）。
	if strings.Index(p2, "「技术方案」") > strings.Index(p2, "「教案」") {
		t.Errorf("硬依赖应排在软依赖前面：\n%s", p2)
	}
}

// TestRetryAfterFakeSuccess：第一轮零交付物判失败，重试一次并在 prompt 尾部追加提示。
func TestRetryAfterFakeSuccess(t *testing.T) {
	dir, r := newJob(t, nil) // 技术方案没有硬依赖
	r.files = [][]string{
		nil, // 第一轮：说完成了但一个文件没写
		{"02_平台材料/技术方案_飞鸟志.md"},
	}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "技术方案")
	if err != nil {
		t.Fatalf("第二轮成功了不该报错：%v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts 应为 2，实际 %d", res.Attempts)
	}
	if len(r.reqs) != 2 {
		t.Fatalf("应跑两轮，实际 %d", len(r.reqs))
	}
	if strings.Contains(r.reqs[0].Prompt, "上次运行没有产出") {
		t.Errorf("首轮 prompt 不该带重试提示")
	}
	if !strings.HasSuffix(r.reqs[1].Prompt, retryHint) {
		t.Errorf("重试轮 prompt 尾部要追加提示，实际结尾：%q",
			r.reqs[1].Prompt[max(0, len(r.reqs[1].Prompt)-80):])
	}
	if len(res.Outputs) != 1 || res.Outputs[0] != "02_平台材料/技术方案_飞鸟志.md" {
		t.Errorf("Outputs 不对：%v", res.Outputs)
	}
	got := journalKindsOf(res.Journal)
	if len(got) != 2 || got[0] != KindMaterialStarted || got[1] != KindMaterialSucceeded {
		t.Errorf("台账应是 started→succeeded，实际 %v", got)
	}
	if res.Journal[1].Retries != 1 {
		t.Errorf("成功事件应记 retries=1，实际 %d", res.Journal[1].Retries)
	}
}

// TestFakeSuccessTwiceFails：两轮都零交付物 → 整个作业失败，台账记 material_failed。
func TestFakeSuccessTwiceFails(t *testing.T) {
	dir, r := newJob(t, nil)
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "技术方案")
	if !errors.Is(err, ErrNoDeliverable) {
		t.Fatalf("期望 ErrNoDeliverable，实际 %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts 应为 2，实际 %d", res.Attempts)
	}
	got := journalKindsOf(res.Journal)
	if len(got) != 2 || got[1] != KindMaterialFailed {
		t.Errorf("台账应是 started→failed，实际 %v", got)
	}
	if res.Journal[1].Retries != 1 {
		t.Errorf("失败事件应记 retries=1，实际 %d", res.Journal[1].Retries)
	}
}

// TestVerifyIgnoresWrongExtAndOutsideFiles：扩展名不合法、或落在项目目录外的文件，
// 都不算交付物——假成功检测不能被这些糊弄过去。
func TestVerifyIgnoresWrongExtAndOutsideFiles(t *testing.T) {
	dir, r := newJob(t, nil)
	outside := filepath.Join(t.TempDir(), "外面的东西.md")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 技术方案只收 .md/.docx；.txt 不算，项目外的 .md 也不算。
	r.files = [][]string{
		{"02_平台材料/技术方案_草稿.txt", outside},
		{"02_平台材料/技术方案_草稿.txt", outside},
	}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "技术方案")
	if !errors.Is(err, ErrNoDeliverable) {
		t.Fatalf("期望 ErrNoDeliverable，实际 %v", err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0] != "02_平台材料/技术方案_草稿.txt" {
		t.Errorf("项目外的文件不该进 Outputs：%v", res.Outputs)
	}
	if !hasQualityNote(res.Journal, "落在项目目录之外") {
		t.Errorf("项目外产物要记 quality_note：%+v", res.Journal)
	}
}

// TestCollectWarnsOnWrongBucket：交付物落对了桶，但顺手写在别的桶里的文件要记警告，
// 且 M2 不搬动它。
func TestCollectWarnsOnWrongBucket(t *testing.T) {
	dir, r := newJob(t, nil)
	stray := "03_研发工作区/技术方案_草稿.md"
	r.files = [][]string{{"02_平台材料/技术方案_飞鸟志.md", stray}}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "技术方案")
	if err != nil {
		t.Fatalf("有合格交付物就该成功：%v", err)
	}
	if !hasQualityNote(res.Journal, "落错桶") {
		t.Fatalf("应记落错桶警告：%+v", res.Journal)
	}
	if !hasQualityNote(res.Journal, "02_平台材料") {
		t.Errorf("警告里要写清正本该在哪：%+v", res.Journal)
	}
	// 不搬动：文件还在原地。
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(stray))); err != nil {
		t.Errorf("M2 不该搬动落错桶的文件：%v", err)
	}
	if len(res.Outputs) != 2 {
		t.Errorf("Outputs 要如实记两个文件：%v", res.Outputs)
	}
}

// TestCollectWarnsOnIllegalPath：落点根本不合规（一级目录不是七目录）要记警告。
func TestCollectWarnsOnIllegalPath(t *testing.T) {
	dir, r := newJob(t, nil)
	r.files = [][]string{{"02_平台材料/技术方案_飞鸟志.md", "临时/乱放.md"}}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "技术方案")
	if err != nil {
		t.Fatalf("有合格交付物就该成功：%v", err)
	}
	if !hasQualityNote(res.Journal, "落点不合规") {
		t.Errorf("应记落点不合规警告：%+v", res.Journal)
	}
}

// TestVisionSkippedWhenNotConfigured：产物含 PNG 而 Vision 没接线时，
// 台账要如实记跳过，不许假装质检过了。
func TestVisionSkippedWhenNotConfigured(t *testing.T) {
	dir, r := newJob(t, nil)
	r.files = [][]string{{"03_研发工作区/系统架构图.drawio", "03_研发工作区/系统架构图.png"}}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "图表")
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if !hasQualityNote(res.Journal, "vision 未配置") {
		t.Errorf("应记看图质检跳过：%+v", res.Journal)
	}
}

// TestAssemblyNoteWrittenIntoBucket：docx + drawio 无 PNG 时，作业要在目标桶下
// 落一份《图表组装说明.txt》并记账。
func TestAssemblyNoteWrittenIntoBucket(t *testing.T) {
	dir, r := newJob(t, map[string]string{
		"02_平台材料/技术方案_飞鸟志.md": "# 技术方案\n",
	})
	r.files = [][]string{{
		"07_竞赛提交/原始资料_实验记录表.docx",
		"07_竞赛提交/原始资料_数据流图.drawio",
	}}
	res, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "原始资料")
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	rel := "07_竞赛提交/" + AssemblyNoteFile
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("组装说明没写出来：%v", err)
	}
	if !strings.Contains(string(body), "原始资料_数据流图.drawio") {
		t.Errorf("组装说明要列出 drawio 文件名：\n%s", body)
	}
	if !containsStr(res.Outputs, rel) {
		t.Errorf("组装说明要进 Outputs：%v", res.Outputs)
	}
	if !hasQualityNote(res.Journal, "图表组装说明") {
		t.Errorf("组装说明要记账：%+v", res.Journal)
	}
}

// TestPromptCarriesEnvironmentAndSkill：prompt 必须带 skill 正文、七目录说明、
// 落桶指令与"不要问我任何问题"的收尾。
func TestPromptCarriesEnvironmentAndSkill(t *testing.T) {
	dir, r := newJob(t, nil)
	r.files = [][]string{{"02_平台材料/技术方案_飞鸟志.md"}}
	d := jobDeps(t, r)
	d.Note = "这次要偏重结构设计"
	if _, err := RunMaterial(context.Background(), d, dir, "技术方案"); err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	p := r.reqs[0].Prompt
	for _, want := range []string{
		"【tech-proposal-generator 技能正文】",
		"P26C-020",
		"01_立项定题",
		"07_竞赛提交",
		"必须**写进 `02_平台材料/`",
		"这次要偏重结构设计",
		"不要问我任何问题",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺少 %q：\n%s", want, p)
		}
	}
	// 额外要求排在收尾指令之前、依赖之后。
	if strings.Index(p, "这次要偏重结构设计") > strings.Index(p, "不要问我任何问题") {
		t.Errorf("额外要求应排在收尾指令之前")
	}
}

// TestRunMaterialRefusesUnknownMaterial：没注册的材料 fail-closed。
func TestRunMaterialRefusesUnknownMaterial(t *testing.T) {
	dir, r := newJob(t, nil)
	if _, err := RunMaterial(context.Background(), jobDeps(t, r), dir, "毕业论文"); !errors.Is(err, project.ErrUnknownMaterial) {
		t.Fatalf("期望 ErrUnknownMaterial，实际 %v", err)
	}
}

// TestRunMaterialRequiresSkillBody：拿不到 skill 正文就不许裸跑。
func TestRunMaterialRequiresSkillBody(t *testing.T) {
	dir, r := newJob(t, nil)
	d := jobDeps(t, r)
	d.SkillBody = nil
	if _, err := RunMaterial(context.Background(), d, dir, "技术方案"); err == nil {
		t.Fatal("SkillBody 为 nil 时应拒绝")
	}
	d2 := jobDeps(t, r)
	d2.SkillBody = func(string) (string, error) { return "", errors.New("skill 不存在") }
	res, err := RunMaterial(context.Background(), d2, dir, "技术方案")
	if err == nil {
		t.Fatal("skill 正文取不到时应失败")
	}
	if len(r.reqs) != 0 {
		t.Errorf("skill 都没拿到不该调 Runner")
	}
	if got := journalKindsOf(res.Journal); len(got) != 1 || got[0] != KindMaterialFailed {
		t.Errorf("台账应记一条 material_failed，实际 %v", got)
	}
}

func hasQualityNote(evs []Event, want string) bool {
	for _, e := range evs {
		if e.Kind == KindQualityNote && strings.Contains(e.Reason, want) {
			return true
		}
	}
	return false
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestSessionHintStampedOnEveryEvent：装配层给的会话线索要盖在本次作业的每条台账
// 事件上（含落盘的那份）；不给时字段整个省略，不落出空字段。
func TestSessionHintStampedOnEveryEvent(t *testing.T) {
	dir, r := newJob(t, nil)
	r.files = [][]string{nil, {"02_平台材料/技术方案_飞鸟志.md"}}
	d := jobDeps(t, r)
	d.SessionHint = "sess-20260823-001"
	res, err := RunMaterial(context.Background(), d, dir, "技术方案")
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if len(res.Journal) == 0 {
		t.Fatal("台账不能是空的")
	}
	for i, ev := range res.Journal {
		if ev.SessionHint != "sess-20260823-001" {
			t.Errorf("第 %d 条事件没盖上会话线索：%+v", i, ev)
		}
	}
	// 落盘的那份也要有。
	evs, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range evs {
		if ev.SessionHint != "sess-20260823-001" {
			t.Errorf("盘上第 %d 条事件没盖上会话线索：%+v", i, ev)
		}
	}

	// 不给会话线索时，字段在 JSONL 里整个省略。
	dir2, r2 := newJob(t, nil)
	r2.files = [][]string{{"02_平台材料/技术方案_飞鸟志.md"}}
	if _, err := RunMaterial(context.Background(), jobDeps(t, r2), dir2, "技术方案"); err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	raw, err := os.ReadFile(JournalPath(dir2))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_hint", "receipts_summary"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("领域层不填的 %s 不该出现在台账里：\n%s", key, raw)
		}
	}
}
