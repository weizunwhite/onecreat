// 材料作业状态机：Resolve → Assemble → Run → Verify（假成功检测）→ 重试 1 次 →
// Collect（落桶校验）→ 记账（`.onecreat/workflow.jsonl`）。
//
// 本文件是 M2 领域层的主干。它**不认识** boot / control / engine / agent /
// toolpolicy —— 跑一轮的能力全部经 Runner 这一个接缝拿到（runner.go），
// 所以整条状态机可以拿 FakeRunner 在临时目录里逐条断言。
package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

// 作业拒绝与失败的哨兵错误。用 errors.Is 判定。
var (
	// ErrMissingDeps：硬依赖材料在项目里找不到产物，作业拒绝发起。
	ErrMissingDeps = errors.New("上游材料缺失")
	// ErrConversational：对话型材料不能走一键流水线（要人来回答问题），M3 才做。
	ErrConversational = errors.New("对话型材料不支持一键生成")
	// ErrNoDeliverable：跑完了但没有任何交付物落进目标桶——"假成功"。
	ErrNoDeliverable = errors.New("没有产出交付物文件")
)

// maxAttempts 是一次作业最多跑几轮：首轮 + 失败重试 1 次（调研 01 §4.2 第 2 件）。
const maxAttempts = 2

// retryHint 是重试轮追加在 prompt 尾部的话。
const retryHint = "\n上次运行没有产出任何交付物文件，这次必须把文件写出来。\n"

// maxDepFileBytes 是单份依赖产物注入 prompt 的上限，超了截断并注明。
const maxDepFileBytes = 200 * 1024

// textExts 是可以整段贴进 prompt 的文本扩展名；其余（docx/pptx/xlsx/zip/png…）
// 只报路径，让模型自己用工具去读——把 docx 的二进制塞进 prompt 是纯烧 token。
var textExts = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".json": true, ".yaml": true,
	".yml": true, ".toml": true, ".csv": true, ".html": true, ".htm": true,
	".xml": true, ".svg": true, ".drawio": true,
}

// Deps 是跑一次材料作业需要的外部能力，全部由装配层注入。
type Deps struct {
	Reg    *registry.Registry // 已加载的注册表
	Runner Runner             // 唯一执行接缝
	// SkillBody 按 skill 名取 SKILL.md 正文（装配层用 internal/skill 的 Store 实现）。
	// 必填：skill 正文就是这份材料的做法，拿不到就不该硬跑。
	SkillBody func(name string) (string, error)
	Note      string        // 老师就地填的额外要求，可空
	Vision    VisionChecker // 看图质检，可为 nil（nil 时台账如实记跳过）
	Timeout   time.Duration // 单轮上限，0 表示不设
	// SessionHint 是装配层给的会话线索（会话 id 之类），原样透传进本次作业的每条台账
	// 事件。领域层不认识会话，只搬运；为空则事件里整个字段省略。
	SessionHint string
}

// JobResult 是一次材料作业的完整交代。
//
// Journal 是本次作业写进 `.onecreat/workflow.jsonl` 的那几条事件（同一份内容），
// 落错桶之类的警告以 quality_note 事件的形式在里面，不另设字段。
type JobResult struct {
	Material string   // 归一后的材料类型
	Attempts int      // 实际跑了几轮
	Outputs  []string // 本次写盘的产物，项目根下的相对路径，升序
	Journal  []Event  // 本次作业记的台账事件
	Err      error    // 失败原因；成功为 nil。与函数第二个返回值同一个 error
}

// RunMaterial 这个函数做什么：为一个在研项目跑一种材料的一键生成作业。
//
// 返回值口径：作业没成功时第二个返回值非 nil，且与 JobResult.Err 是同一个 error。
// 连材料都解析不出来（未注册 / 对话型 / 项目读不了）时第一个返回值为 nil。
func RunMaterial(ctx context.Context, d Deps, projDir, materialType string) (*JobResult, error) {
	// ---- Resolve：材料、种类、项目、依赖 ----
	if d.Reg == nil {
		return nil, fmt.Errorf("注册表为空")
	}
	if d.Runner == nil {
		return nil, fmt.Errorf("Runner 为空")
	}
	if d.SkillBody == nil {
		return nil, fmt.Errorf("SkillBody 未提供：拿不到 skill 正文就不跑，不许裸跑")
	}
	canon := d.Reg.Canonical(materialType)
	m, ok := d.Reg.Material(canon)
	if !ok {
		return nil, fmt.Errorf("%w: %q", project.ErrUnknownMaterial, materialType)
	}
	if m.Kind != "oneclick" {
		return nil, fmt.Errorf("%w：材料「%s」是 %s，要人来回答问题，M3 的对话型线程才做",
			ErrConversational, canon, m.Kind)
	}
	bucket := bucketOfOutputs(m)
	if bucket == "" {
		return nil, fmt.Errorf("材料「%s」的产物不落在项目七目录里，本层跑不了", canon)
	}

	proj, err := project.Load(projDir)
	if err != nil {
		return nil, fmt.Errorf("读项目失败：%w", err)
	}
	st, err := ProjectStatus(d.Reg, proj)
	if err != nil {
		return nil, fmt.Errorf("盘点项目材料失败：%w", err)
	}

	res := &JobResult{Material: canon}
	rec := &recorder{projDir: projDir, sessionHint: d.SessionHint, res: res}

	// 硬依赖：缺一个就拒绝发起，并明说缺哪些——先生成上游，别硬跑出一份没依据的材料。
	if missing := missingDeps(d.Reg, st, m.Deps); len(missing) > 0 {
		res.Err = fmt.Errorf("%w：材料「%s」需要先有 %s", ErrMissingDeps, canon, strings.Join(missing, "、"))
		rec.add(Event{Kind: KindMaterialSkipped, Material: canon, Reason: res.Err.Error()})
		return res, res.Err
	}

	// 依赖产物正文：硬依赖必然 Present（上面刚判过），软依赖存在才注入。
	deps := map[string][]byte{}
	for _, name := range append(append([]string{}, m.Deps...), m.SoftDeps...) {
		dep := d.Reg.Canonical(name)
		ms, found := findMaterial(st, dep)
		if !found || !ms.Present {
			continue
		}
		if body := readDepBody(projDir, ms); len(body) > 0 {
			deps[dep] = body
		}
	}

	skillBody, err := d.SkillBody(m.Skill)
	if err != nil {
		res.Err = fmt.Errorf("取 skill「%s」正文失败：%w", m.Skill, err)
		rec.add(Event{Kind: KindMaterialFailed, Material: canon, Reason: res.Err.Error()})
		return res, res.Err
	}

	// ---- Assemble ----
	prompt, err := AssemblePrompt(d.Reg, proj, canon, skillBody, deps, d.Note)
	if err != nil {
		res.Err = fmt.Errorf("组 prompt 失败：%w", err)
		rec.add(Event{Kind: KindMaterialFailed, Material: canon, Reason: res.Err.Error()})
		return res, res.Err
	}
	rec.add(Event{Kind: KindMaterialStarted, Material: canon})

	// ---- Run + Verify + 重试 1 次 ----
	var (
		written []string
		lastErr error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		p := prompt
		if attempt > 1 {
			p += retryHint
		}
		res.Attempts = attempt
		out, runErr := d.Runner.Run(ctx, RunRequest{
			ProjectDir:   projDir,
			MaterialType: canon,
			Prompt:       p,
			Timeout:      d.Timeout,
		})
		written = out.WrittenFiles
		if runErr != nil {
			lastErr = fmt.Errorf("第 %d 轮运行失败：%w", attempt, runErr)
			continue
		}
		// 假成功检测：模型说完成了不算数，要有真的交付物落进目标桶。
		if len(deliverables(projDir, bucket, extsOf(m), written)) == 0 {
			lastErr = fmt.Errorf("第 %d 轮%w", attempt, ErrNoDeliverable)
			continue
		}
		lastErr = nil
		break
	}

	// ---- Collect：产物归位校验（只记警告，M2 不搬文件） ----
	res.Outputs = collect(rec, canon, bucket, projDir, written)

	if lastErr != nil {
		res.Err = lastErr
		rec.add(Event{
			Kind: KindMaterialFailed, Material: canon,
			Outputs: res.Outputs, Retries: res.Attempts - 1, Reason: lastErr.Error(),
		})
		return res, res.Err
	}

	// ---- 质量四件套的另两件：看图质检 hook + 图表组装说明 ----
	runVisionCheck(ctx, d.Vision, rec, canon, projDir, res.Outputs)
	if extra := writeAssemblyNote(rec, canon, projDir, bucket, res.Outputs); extra != "" {
		res.Outputs = append(res.Outputs, extra)
		sort.Strings(res.Outputs)
	}

	rec.add(Event{
		Kind: KindMaterialSucceeded, Material: canon,
		Outputs: res.Outputs, Retries: res.Attempts - 1,
	})
	if rec.err != nil {
		res.Err = fmt.Errorf("产物已生成，但台账写入失败：%w", rec.err)
		return res, res.Err
	}
	return res, nil
}

// recorder 把台账事件同时记进 JobResult.Journal 和磁盘台账，
// 保证"返回值里说记了"与"文件里真的有"是同一份。
type recorder struct {
	projDir     string
	sessionHint string // 装配层给的会话线索，盖在每条事件上
	res         *JobResult
	err         error // 第一次写盘失败的原因
}

func (r *recorder) add(ev Event) {
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	if ev.SessionHint == "" {
		ev.SessionHint = r.sessionHint
	}
	r.res.Journal = append(r.res.Journal, ev)
	if err := Append(r.projDir, ev); err != nil && r.err == nil {
		r.err = err
	}
}

// missingDeps 找出硬依赖里还没有产物的那些。
//
// 判定沿用 ProjectStatus（已知"宁漏勿误"，漏判导致的重复生成可接受，
// 见 06_M2执行方案.md §2）。依赖材料在 Status 里根本没有条目（产题/评题那类
// 产物不在项目目录里的）也算缺——本层判不了就不许假装齐备。
func missingDeps(reg *registry.Registry, st *Status, deps []string) []string {
	var missing []string
	for _, name := range deps {
		dep := reg.Canonical(name)
		ms, found := findMaterial(st, dep)
		if !found || !ms.Present {
			missing = append(missing, dep)
		}
	}
	return missing
}

func findMaterial(st *Status, typ string) (MaterialStatus, bool) {
	for _, ms := range st.Materials {
		if ms.Type == typ {
			return ms, true
		}
	}
	return MaterialStatus{}, false
}

// readDepBody 把一份上游材料的全部产物拼成注入用的正文。
//
// 文本文件贴全文（超长截断并注明），二进制文件只报路径让模型自己读。
func readDepBody(projDir string, ms MaterialStatus) []byte {
	var b strings.Builder
	for _, rel := range ms.Paths {
		fmt.Fprintf(&b, "### %s\n\n", rel)
		ext := strings.ToLower(filepath.Ext(rel))
		if !textExts[ext] {
			fmt.Fprintf(&b, "（%s 是二进制文件，正文没有内联；需要时用文件工具自己读这个路径。）\n\n", ext)
			continue
		}
		data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(rel)))
		if err != nil {
			fmt.Fprintf(&b, "（这份文件读不出来：%v）\n\n", err)
			continue
		}
		if len(data) > maxDepFileBytes {
			data = data[:maxDepFileBytes]
			b.Write(data)
			b.WriteString("\n（内容过长，以上为前 200KB，其余请自行读原文件。）\n\n")
			continue
		}
		b.Write(data)
		b.WriteString("\n\n")
	}
	return []byte(b.String())
}

// deliverables 挑出"落进目标桶且扩展名合法"的产物——假成功检测就看它空不空。
func deliverables(projDir, bucket string, exts, written []string) []string {
	var out []string
	prefix := bucket + "/"
	for _, w := range written {
		rel, inside := relInProject(projDir, w)
		if !inside || !strings.HasPrefix(rel, prefix) {
			continue
		}
		if !extMatches(filepath.Base(rel), exts) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// collect 把本轮写过的文件逐个做归位校验。
//
// 两道检查是**分工**的，不能只用一道：
//   - project.Classify 管**落点合法性**（路径穿越 / 隐藏路径 / 根散文件 / 一级目录不是
//     七目录 / 机制目录出现在不该出现的桶下）。注意它带 materialType 时走的是注册表
//     outputs.dir，返回的是"这份材料的正本**应该**在哪"，不是"这个文件**实际**在哪"，
//     所以它一个人判不出落错桶。
//   - 实际前缀桶 vs 材料目标桶，才是落错桶的判据。
//
// **不搬动文件**：M2 只记警告（quality_note），自动归位是后面的事
// （06_M2执行方案.md §2 依赖注入口径下方的分工）。
func collect(rec *recorder, material, bucket, projDir string, written []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range written {
		rel, inside := relInProject(projDir, w)
		if !inside {
			rec.add(Event{
				Kind: KindQualityNote, Material: material,
				Reason: fmt.Sprintf("产物 %s 落在项目目录之外，未纳入本次产物清单", w),
			})
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)

		if _, err := project.Classify(material, rel); err != nil {
			rec.add(Event{
				Kind: KindQualityNote, Material: material, Outputs: []string{rel},
				Reason: fmt.Sprintf("产物落点不合规：%v（M2 不自动搬动，请人工确认）", err),
			})
			continue
		}
		if actual := actualBucket(rel); actual != bucket {
			rec.add(Event{
				Kind: KindQualityNote, Material: material, Outputs: []string{rel},
				Reason: fmt.Sprintf("产物落错桶：实际落在 %s，本材料正本应在 %s（M2 不自动搬动，请人工确认）",
					actual, bucket),
			})
		}
	}
	sort.Strings(out)
	return out
}

// runVisionCheck 是看图质检的 hook 点。
//
// 没有 PNG 就不用看；Vision 没接线（M2 的常态）就在台账里如实记一条跳过，
// 绝不假装质检过了。
func runVisionCheck(ctx context.Context, v VisionChecker, rec *recorder, material, projDir string, outputs []string) {
	var pngs []string
	for _, rel := range outputs {
		if strings.EqualFold(filepath.Ext(rel), ".png") {
			pngs = append(pngs, filepath.Join(projDir, filepath.FromSlash(rel)))
		}
	}
	if len(pngs) == 0 {
		return
	}
	if v == nil {
		rec.add(Event{
			Kind: KindQualityNote, Material: material,
			Reason: fmt.Sprintf("看图质检跳过：vision 未配置（本次有 %d 张 PNG 未经质检）", len(pngs)),
		})
		return
	}
	note, err := v.Check(ctx, pngs)
	if err != nil {
		rec.add(Event{
			Kind: KindQualityNote, Material: material,
			Reason: fmt.Sprintf("看图质检失败：%v", err),
		})
		return
	}
	if strings.TrimSpace(note) != "" {
		rec.add(Event{Kind: KindQualityNote, Material: material, Reason: "看图质检：" + note})
	}
}

// writeAssemblyNote 需要时在目标桶下落一份《图表组装说明.txt》，返回它的相对路径。
// 不需要或写不进去时返回空串（写不进去只记警告，不推翻已经生成好的产物）。
func writeAssemblyNote(rec *recorder, material, projDir, bucket string, outputs []string) string {
	body, need := AssemblyNote(outputs)
	if !need {
		return ""
	}
	rel := bucket + "/" + AssemblyNoteFile
	path := filepath.Join(projDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		rec.add(Event{Kind: KindQualityNote, Material: material,
			Reason: fmt.Sprintf("图表组装说明没写成：%v", err)})
		return ""
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		rec.add(Event{Kind: KindQualityNote, Material: material,
			Reason: fmt.Sprintf("图表组装说明没写成：%v", err)})
		return ""
	}
	rec.add(Event{Kind: KindQualityNote, Material: material, Outputs: []string{rel},
		Reason: "产物里有 docx + drawio 但没有 PNG，已生成图表组装说明，需人工导出图片插入文档"})
	return rel
}

// relInProject 把执行层报回来的路径归一成项目根下的相对路径（/ 分隔）。
// 绝对路径与相对路径都收；跳出项目根的返回 inside=false。
func relInProject(projDir, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projDir, filepath.FromSlash(p))
	}
	rel, err := filepath.Rel(projDir, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// actualBucket 取相对路径的一级目录，也就是这个文件**实际**落在哪个桶。
func actualBucket(rel string) string {
	if i := strings.Index(rel, "/"); i > 0 {
		return rel[:i]
	}
	return rel
}

func extsOf(m *registry.Material) []string {
	if m.Outputs == nil {
		return nil
	}
	return m.Outputs.Exts
}
