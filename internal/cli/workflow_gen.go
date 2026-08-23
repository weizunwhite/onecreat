package cli

// `reasonix workflow gen` —— 一键生成一种材料(M2 · W2 的 CLI 半边)。
//
// 这条命令自己**不写 prompt、不判依赖、不记账**:那些是领域层(internal/workflow)
// 的材料作业状态机的事。它只做装配层该做的四件事:
//
//  1. 把"项目编号/短名 + 材料名"解析成一个真实项目目录与一条注册表登记;
//  2. 把材料的 skill 名解析成本机 skill 正文(解析不到就给人话错误 + 扫过的路径);
//  3. 守住 M2 拍板 2:真实课题目录默认拒绝,`--allow-real` 才放行;
//  4. 把 ControllerRunner 交给作业层,把结果打印成人看得懂的样子。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/project"
	"reasonix/internal/skill"
	"reasonix/internal/workflow"
	"reasonix/internal/workflow/registry"
	"reasonix/internal/workspace"
)

// materialTurnTimeout 是单轮会话的墙钟上限。
//
// 一份技术方案/教案的生成动辄要十几轮工具调用,给得太紧会在半路砍掉一轮、留下半份
// 产物;给得太松则模型卡死时没人叫停。20 分钟是按 NASApp 的实测量级取的,以后要调
// 就改这一个常量(注册表里没有这个口径,别把它散到多处)。
const materialTurnTimeout = 20 * time.Minute

func workflowGen(args []string) int {
	fs := flag.NewFlagSet("workflow gen", flag.ContinueOnError)
	root := fs.String("root", defaultKetiRoot, "课题库根目录")
	engine := fs.String("engine", "native", "回合引擎:native(默认)| dsh")
	mode := fs.String("mode", "", "档位:快速 | 深度(默认取注册表 default_mode)")
	noteFlag := fs.String("note", "", "补充说明,附在 prompt 末尾")
	dryRun := fs.Bool("dry-run", false, "只打印装配结果,不跑模型、不写文件")
	allowReal := fs.Bool("allow-real", false, "允许写真实课题目录(默认拒绝,见 M2 拍板 2)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// 与 status 一致:允许「位置参数在前、flag 在后」。
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "用法:reasonix workflow gen <项目编号或短名> <材料名> [flags]")
		fmt.Fprintln(os.Stderr, "例如:reasonix workflow gen P26C-020 技术方案 --dry-run")
		return 2
	}
	query, materialArg := rest[0], rest[1]
	if err := fs.Parse(rest[2:]); err != nil {
		return 2
	}
	if *engine != "native" && *engine != "dsh" {
		fmt.Fprintf(os.Stderr, "--engine 只能是 native 或 dsh,收到 %q\n", *engine)
		return 2
	}

	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载工作流注册表失败:", err)
		return 1
	}
	materialType := reg.Canonical(materialArg)
	m, ok := reg.Material(materialType)
	if !ok {
		fmt.Fprintf(os.Stderr, "材料类型 %q 没在注册表里登记。可跑 reasonix workflow status <项目> 看全部 %d 种材料。\n",
			materialArg, len(reg.Materials))
		return 1
	}
	if m.Status != "available" {
		fmt.Fprintf(os.Stderr, "材料 %s 在注册表里的状态是 %q,还不能一键生成。\n", m.Type, m.Status)
		return 1
	}

	scan, err := project.Scan(filepath.Join(*root, projectsSubDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读在研项目目录失败:%v\n(--root 指的是课题库根,里面应有 %s/)\n", err, projectsSubDir)
		return 1
	}
	p, err := findProject(scan.Projects, query)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// M2 拍板 2:生成质量定型之前,真实课题目录默认不许写。
	//
	// 判据只看"会不会写":--dry-run 证明不写任何文件,所以它对真实项目放行(只警告
	// 一句)。否则想看一眼装配结果都得敲 --allow-real,那个 flag 很快就会变成肌肉
	// 记忆,红线就守不住了。
	realKeti, whyReal := isRealKetiProject(p.Dir)
	if realKeti && !*allowReal && !*dryRun {
		fmt.Fprintf(os.Stderr, "拒绝在真实课题目录里生成材料:%s\n%s\n", p.Dir, whyReal)
		fmt.Fprintln(os.Stderr, "先在沙箱项目里验收(把项目目录复制一份到 /tmp 之类的地方,--root 指过去);")
		fmt.Fprintln(os.Stderr, "确实要写真实项目,显式加 --allow-real。")
		return 1
	}

	selectedMode, err := resolveMaterialMode(reg, m, *mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	skillBody := skillBodyFunc(p.Dir)
	sk, err := resolveMaterialSkill(p.Dir, m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// 档位注册表里没有对应字段,作业层也不认识它 —— 它是给模型看的一句话要求,
	// 所以并进 Note 走同一条通道,而不是在领域层开一个新字段。
	note := strings.TrimSpace(*noteFlag)
	fullNote := fmt.Sprintf("档位:%s(快速=够用即可,深度=展开论证与细节)", selectedMode)
	if note != "" {
		fullNote += "\n" + note
	}

	if *dryRun {
		printGenDryRun(reg, p, m, sk, selectedMode, fullNote)
		if realKeti && !*allowReal {
			fmt.Println("\n注意  这是真实课题目录,真跑(去掉 --dry-run)会被拒绝,除非显式加 --allow-real。")
		}
		return 0
	}

	fmt.Printf("材料作业  %s %s · %s · 档位 %s · 引擎 %s\n",
		p.Code(), p.ShortName(), m.Type, selectedMode, *engine)
	res, err := workflow.RunMaterial(context.Background(), workflow.Deps{
		Reg:         reg,
		Runner:      &ControllerRunner{Engine: *engine, Sink: agent.NewTextSink(os.Stdout, nil, 80)},
		SkillBody:   skillBody,
		Note:        fullNote,
		Timeout:     materialTurnTimeout,
		SessionHint: "cli:workflow gen",
	}, p.Dir, m.Type)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n材料作业失败:", err)
		printGenResult(p.Dir, res)
		return 1
	}
	printGenResult(p.Dir, res)
	return 0
}

// resolveMaterialMode 定档:命令行 > 注册表 default_mode > 材料的第一个合法档位。
func resolveMaterialMode(reg *registry.Registry, m *registry.Material, want string) (string, error) {
	want = strings.TrimSpace(want)
	legal, _ := reg.EnumValues("modes")
	if want != "" {
		if !containsString(legal, want) {
			return "", fmt.Errorf("--mode %q 不是合法档位,只能是:%s", want, strings.Join(legal, " / "))
		}
		if len(m.Modes) > 0 && !containsString(m.Modes, want) {
			return "", fmt.Errorf("材料 %s 只支持档位 %s,不支持 %q", m.Type, strings.Join(m.Modes, " / "), want)
		}
		return want, nil
	}
	if m.DefaultMode != "" {
		return m.DefaultMode, nil
	}
	if len(m.Modes) > 0 {
		return m.Modes[0], nil
	}
	return "快速", nil
}

// resolveMaterialSkill 把材料登记的 skill 名解析成本机 skill 正文。
//
// 解析不到不猜、不兜底:直接把扫过的技能路径原样列出来,让人一眼看出是"没装这个
// skill"还是"装在了没扫的地方"。
func resolveMaterialSkill(projectDir string, m *registry.Material) (skill.Skill, error) {
	name := strings.TrimSpace(m.Skill)
	if name == "" {
		return skill.Skill{}, fmt.Errorf("材料 %s 在注册表里没登记 skill,无法一键生成", m.Type)
	}
	var custom []string
	if ws, err := workspace.New(projectDir); err == nil {
		if cfg, err := config.LoadIn(ws); err == nil {
			custom = cfg.SkillCustomPaths()
		}
	}
	store := skill.New(skill.Options{ProjectRoot: projectDir, CustomPaths: custom, Stderr: os.Stderr})
	if sk, ok := store.Read(name); ok {
		return sk, nil
	}
	var lines []string
	for _, r := range store.Roots() {
		lines = append(lines, fmt.Sprintf("    %s(%s,%s)", r.Dir, r.Scope, r.Status))
	}
	return skill.Skill{}, fmt.Errorf(
		"材料 %s 需要的 skill %q 不在本机技能路径里。\n  扫描过的路径:\n%s\n"+
			"  装上它(competition-toolkit 插件里的 %s),或把它放进上面任一目录再重跑。",
		m.Type, name, strings.Join(lines, "\n"), name)
}

// skillBodyFunc 造一个"按 skill 名取正文"的回调,交给作业层(workflow.Deps.SkillBody)。
// 它与 resolveMaterialSkill 走同一个 Store,所以 CLI 预检通过的 skill,作业层一定也拿得到。
func skillBodyFunc(projectDir string) func(name string) (string, error) {
	return func(name string) (string, error) {
		sk, err := resolveMaterialSkill(projectDir, &registry.Material{Type: "(作业层请求)", Skill: name})
		if err != nil {
			return "", err
		}
		return sk.Body, nil
	}
}

// isRealKetiProject 判断一个项目目录是不是坐落在真实课题库之下(M2 拍板 2 的判据)。
// 判据只有一条:路径在 defaultKetiRoot 之下 —— 与 M1 的默认 root 同源的那一个常量。
func isRealKetiProject(dir string) (bool, string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, ""
	}
	realRoot := defaultKetiRoot
	if resolved, err := filepath.EvalSymlinks(defaultKetiRoot); err == nil {
		realRoot = resolved
	}
	resolvedDir := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		resolvedDir = resolved
	}
	if under(defaultKetiRoot, abs) || under(realRoot, resolvedDir) {
		return true, fmt.Sprintf("它在真实课题库 %s 之下;M1–M4 期间生成质量还没定型,默认不往真实项目里写(06 执行方案 §1 拍板 2)。", defaultKetiRoot)
	}
	return false, ""
}

// printGenDryRun 打印装配结果,并把领域层真正的 AssemblePrompt 产物原样打出来
// ——不是本层复述一份,复述迟早和真跑那份分叉。
//
// 一处刻意的不完整:**依赖产物正文不注入**。那段是作业层在 RunMaterial 里读盘拼进去
// 的(readDepBody 有文本/二进制与截断口径),装配层自己再读一遍就成了第二份口径,
// prompt 会和真跑那份悄悄分叉。所以这里只列"会注入哪几份文件",正文留给真跑。
func printGenDryRun(reg *registry.Registry, p *project.Project, m *registry.Material,
	sk skill.Skill, mode, note string) {
	fmt.Printf("[dry-run] 材料作业装配结果(不跑模型、不写文件)\n\n")
	fmt.Printf("项目    %s %s\n", p.Code(), p.ShortName())
	fmt.Printf("目录    %s\n", p.Dir)
	fmt.Printf("材料    %s(阶段 %s · 类型 %s · 档位 %s)\n", m.Type, m.Stage, m.Kind, mode)
	if m.Outputs != nil {
		fmt.Printf("落桶    %s", m.Outputs.Dir)
		if len(m.Outputs.Exts) > 0 {
			fmt.Printf("  扩展名 %s", strings.Join(m.Outputs.Exts, "/"))
		}
		fmt.Println()
	}
	fmt.Printf("skill   %s  正文 %d 字符\n        %s\n", sk.Name, len(sk.Body), sk.Path)
	fmt.Printf("台账    %s\n", workflow.JournalPath(p.Dir))
	fmt.Printf("单轮上限 %s\n", materialTurnTimeout)

	// 依赖:硬依赖缺失会让作业层直接拒绝发起,这里先按 M1 的齐备度判定给出现状。
	if st, err := workflow.ProjectStatus(reg, p); err == nil {
		present := map[string][]string{}
		for _, ms := range st.Materials {
			if ms.Present {
				present[ms.Type] = ms.Paths
			}
		}
		printDeps("硬依赖", m.Deps, present)
		printDeps("软依赖", m.SoftDeps, present)
	}

	prompt, err := workflow.AssemblePrompt(reg, p, m.Type, sk.Body, nil, note)
	if err != nil {
		fmt.Printf("\n装配 prompt 失败:%v\n", err)
		return
	}
	fmt.Printf("\n----- 装配好的 prompt(%d 字符;依赖产物正文由作业层在真跑时注入)-----\n%s\n-----\n",
		len(prompt), prompt)

	fmt.Println("\n门禁    材料作业专用:项目目录内文件读写 allow · bash 仅 mkdir/ls · 其余一律 deny(无 ask)")
}

func printDeps(label string, deps []string, present map[string][]string) {
	if len(deps) == 0 {
		fmt.Printf("%s  (无)\n", label)
		return
	}
	fmt.Printf("%s\n", label)
	for _, d := range deps {
		if paths, ok := present[d]; ok {
			fmt.Printf("  ✓ %s  %s\n", d, strings.Join(paths, "、"))
			continue
		}
		fmt.Printf("  ✗ %s（缺失）\n", d)
	}
}

// printGenResult 把作业结果打印成人看得懂的样子:产物清单、跑了几轮、台账里的提醒。
func printGenResult(projDir string, res *workflow.JobResult) {
	if res == nil {
		return
	}
	fmt.Printf("\n材料  %s · 跑了 %d 轮\n", res.Material, res.Attempts)
	if len(res.Outputs) == 0 {
		fmt.Println("产物  (无)")
	} else {
		fmt.Println("产物")
		for _, o := range res.Outputs {
			fmt.Printf("  %s\n", o)
		}
	}
	for _, ev := range res.Journal {
		switch ev.Kind {
		case workflow.KindQualityNote:
			fmt.Printf("质检  %s\n", ev.Reason)
		case workflow.KindMaterialFailed, workflow.KindMaterialSkipped:
			fmt.Printf("台账  %s:%s\n", ev.Kind, ev.Reason)
		}
	}
	fmt.Printf("台账  %s(%d 条事件)\n", workflow.JournalPath(projDir), len(res.Journal))
}

func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

func countLines(s string) int { return len(strings.Split(s, "\n")) }
