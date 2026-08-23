package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/project"
	"reasonix/internal/topic"
	"reasonix/internal/workflow"
	"reasonix/internal/workflow/registry"
)

// defaultKetiRoot 是课题库根目录的默认值。
//
// **迁移期默认**：M1 阶段路径写死在这一处，后续（M4 科创工作台）进 config，
// 由工作区/配置决定。改的时候只有这一个常量要动。
const defaultKetiRoot = "/Users/localwork/课题"

// projectsSubDir 是课题根下的在研项目目录名（topic 包内部也认这个名字）。
const projectsSubDir = "01_在研项目"

// workflowCommand backs `reasonix workflow` —— 科创工作流入口。
//
// scan / status 是只读查询（M1）：**不写任何文件**，也不发号。
// gen 是一键生成一种材料（M2）：它会跑模型、会往项目目录里写文件，所以它单独守着
// 一条红线 —— 真实课题目录默认拒绝，`--allow-real` 才放行（06 执行方案 §1 拍板 2）。
func workflowCommand(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "scan":
		return workflowScan(args[1:])
	case "status":
		return workflowStatus(args[1:])
	case "gen":
		return workflowGen(args[1:])
	case "", "help", "-h", "--help":
		workflowUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知的 workflow 子命令 %q\n\n", sub)
		workflowUsage()
		return 2
	}
}

func workflowUsage() {
	fmt.Printf(`reasonix workflow —— 科创工作流

用法：
  reasonix workflow scan   [--root 课题根] [--json]        扫全库：项目清单 + 题库三态计数
  reasonix workflow status <编号或短名> [--root 课题根] [--json]
                                                          看单个项目的材料齐备度
  reasonix workflow gen <编号或短名> <材料名> [flags]      一键生成一种材料（会写文件）
      --engine native|dsh   回合引擎，默认 native
      --mode 快速|深度      档位，默认取注册表 default_mode
      --note "..."          附加说明，附在 prompt 末尾
      --dry-run             只打印装配结果，不跑模型、不写文件
      --allow-real          允许写真实课题目录（默认拒绝）

说明：
  --root  课题库根目录，默认 %s
  --json  输出机器可读格式（给脚本用；人看就别加）

scan / status **全程只读**：不建目录、不改名、不写任何文件。
材料齐备度按注册表的落桶目录 + 扩展名 + 命名关键词三重条件判定，
宁可漏判（报缺失）也不误判；判不了的材料标 ? 而不是假装判过。

gen 的门禁是材料作业专用口径：项目目录内文件读写 allow、bash 只允许 mkdir/ls，
其余（联网、子代理、提问、目录外路径）一律 deny —— 没有 ask，无人值守也不会静默放行。

示例：
  reasonix workflow scan
  reasonix workflow status P26C-020
  reasonix workflow status 飞鸟志 --json
  reasonix workflow gen P26C-020 技术方案 --dry-run
`, defaultKetiRoot)
}

// ---------- scan ----------

// scanJSON 是 `workflow scan --json` 的机器格式。
type scanJSON struct {
	Root       string           `json:"root"`
	Projects   []scanProjectRow `json:"projects"`
	TopicState map[string]int   `json:"topic_states"`
	Warnings   int              `json:"warnings"`
	Skipped    []string         `json:"skipped_dirs,omitempty"`
}

type scanProjectRow struct {
	Code      string `json:"code"`
	ShortName string `json:"short_name"`
	Stage     int    `json:"stage"`
	StageName string `json:"stage_name"`
	Dir       string `json:"dir"`
}

func workflowScan(args []string) int {
	fs := flag.NewFlagSet("workflow scan", flag.ContinueOnError)
	root := fs.String("root", defaultKetiRoot, "课题库根目录")
	jsonOut := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载工作流注册表失败：", err)
		return 1
	}
	res, err := project.Scan(filepath.Join(*root, projectsSubDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读在研项目目录失败：%v\n（--root 指的是课题库根，里面应有 %s/）\n", err, projectsSubDir)
		return 1
	}
	index, err := topic.Scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "扫描题库失败：", err)
		return 1
	}

	states := map[string]int{}
	for _, c := range index.Cards {
		states[c.State]++
	}
	rows := make([]scanProjectRow, 0, len(res.Projects))
	for _, p := range res.Projects {
		row := scanProjectRow{Code: p.Code(), ShortName: p.ShortName(), Dir: p.Dir}
		if n, ok := p.Stage(); ok {
			row.Stage, row.StageName = n, workflow.StageName(reg, n)
		}
		rows = append(rows, row)
	}

	if *jsonOut {
		out := scanJSON{Root: *root, Projects: rows, TopicState: states, Warnings: len(index.Warnings)}
		for _, s := range res.Skipped {
			out.Skipped = append(out.Skipped, s.Dir+"（"+s.Reason+"）")
		}
		return printJSON(out)
	}

	fmt.Printf("课题库  %s\n\n", *root)
	fmt.Printf("在研项目 %d 个\n", len(rows))
	fmt.Printf("  %s %s %s\n", padRight("编号", 12), padRight("短名", 18), "段位")
	for _, r := range rows {
		stage := "（stage 读不出）"
		if r.Stage > 0 {
			stage = fmt.Sprintf("%d %s", r.Stage, r.StageName)
		}
		fmt.Printf("  %s %s %s\n", padRight(r.Code, 12), padRight(r.ShortName, 18), stage)
	}
	for _, s := range res.Skipped {
		fmt.Printf("  跳过 %s：%s\n", s.Dir, s.Reason)
	}

	fmt.Printf("\n题库题卡 %d 张\n", len(index.Cards))
	for _, st := range []string{topic.StateEstablished, topic.StateUnestablished, topic.StateCandidate} {
		fmt.Printf("  %s %d\n", padRight(topicStateLabel(st), 18), states[st])
	}
	fmt.Printf("\n扫描 warning %d 条", len(index.Warnings))
	if len(index.Warnings) > 0 {
		fmt.Print("（题库里格式不规范的条目，只提示不阻断）")
	}
	fmt.Println()
	return 0
}

func topicStateLabel(state string) string {
	switch state {
	case topic.StateEstablished:
		return "established 已立项"
	case topic.StateUnestablished:
		return "unestablished 未立项"
	case topic.StateCandidate:
		return "candidate 候选"
	}
	return state
}

// ---------- status ----------

// statusJSON 是 `workflow status --json` 的机器格式。
type statusJSON struct {
	Code           string           `json:"code"`
	ShortName      string           `json:"short_name"`
	FullName       string           `json:"full_name"`
	Line           string           `json:"line"`
	Stage          int              `json:"stage"`
	StageName      string           `json:"stage_name"`
	Dir            string           `json:"dir"`
	BucketFiles    map[string]int   `json:"bucket_files"`
	MissingBuckets []string         `json:"missing_buckets,omitempty"`
	EvidenceLedger bool             `json:"evidence_ledger"`
	LedgerPaths    []string         `json:"evidence_ledger_paths,omitempty"`
	Materials      []statusMaterial `json:"materials"`
	Unknown        []string         `json:"unknown"`
}

type statusMaterial struct {
	Type    string   `json:"type"`
	Stage   string   `json:"stage"`
	Kind    string   `json:"kind"`
	Present bool     `json:"present"`
	Paths   []string `json:"paths,omitempty"`
	Rule    string   `json:"rule"`
}

func workflowStatus(args []string) int {
	fs := flag.NewFlagSet("workflow status", flag.ContinueOnError)
	root := fs.String("root", defaultKetiRoot, "课题库根目录")
	jsonOut := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// 允许 `status P26C-020 --json` 这种"编号在前、flag 在后"的顺序：
	// 先吃掉第一个位置参数，剩下的再让 flag 包解析一次。
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "请给一个项目编号或短名，例如：reasonix workflow status P26C-020")
		return 2
	}
	query := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}

	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载工作流注册表失败：", err)
		return 1
	}
	res, err := project.Scan(filepath.Join(*root, projectsSubDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读在研项目目录失败：%v\n（--root 指的是课题库根，里面应有 %s/）\n", err, projectsSubDir)
		return 1
	}
	p, err := findProject(res.Projects, query)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := workflow.ProjectStatus(reg, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "判定材料齐备度失败：", err)
		return 1
	}

	if *jsonOut {
		out := statusJSON{
			Code: st.Code, ShortName: st.ShortName, FullName: st.FullName, Line: st.Line,
			Stage: st.Stage, StageName: st.StageName, Dir: st.Dir,
			BucketFiles: st.BucketFiles, MissingBuckets: st.MissingBuckets,
			EvidenceLedger: st.EvidenceLedger, LedgerPaths: st.EvidenceLedgerPaths,
			Unknown: st.Unknown,
		}
		for _, m := range st.Materials {
			out.Materials = append(out.Materials, statusMaterial{
				Type: m.Type, Stage: m.Stage, Kind: m.Kind,
				Present: m.Present, Paths: m.Paths, Rule: m.Rule,
			})
		}
		return printJSON(out)
	}
	printStatusText(reg, st)
	return 0
}

// findProject 按编号或短名找项目。编号不分大小写；短名先精确、再唯一子串匹配，
// 匹配到多个就把候选列出来让人重说一次——绝不替人猜一个。
func findProject(projects []*project.Project, query string) (*project.Project, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("项目编号或短名为空")
	}
	for _, p := range projects {
		if strings.EqualFold(p.Code(), q) || p.ShortName() == q {
			return p, nil
		}
	}
	var hits []*project.Project
	for _, p := range projects {
		if strings.Contains(p.ShortName(), q) || strings.Contains(strings.ToUpper(p.Code()), strings.ToUpper(q)) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("没找到项目 %q（在研项目共 %d 个，可先跑 reasonix workflow scan 看清单）", q, len(projects))
	default:
		var names []string
		for _, p := range hits {
			names = append(names, p.Code()+" "+p.ShortName())
		}
		return nil, fmt.Errorf("%q 匹配到多个项目，请说得更具体：%s", q, strings.Join(names, "、"))
	}
}

func printStatusText(reg *registry.Registry, st *workflow.Status) {
	fmt.Printf("项目  %s %s", st.Code, st.ShortName)
	if st.FullName != "" {
		fmt.Printf("（%s）", st.FullName)
	}
	fmt.Println()
	fmt.Printf("业务线 %s", st.Line)
	if st.Student != "" {
		fmt.Printf(" · 学生 %s", st.Student)
	}
	fmt.Println()
	if st.StageOK {
		fmt.Printf("段位  %d %s\n", st.Stage, st.StageName)
	} else {
		fmt.Println("段位  （project.json 的 stage 读不出合法值）")
	}
	fmt.Printf("目录  %s\n", st.Dir)

	fmt.Println("\n七目录文件计数")
	for _, b := range project.Buckets {
		n, ok := st.BucketFiles[b]
		if !ok {
			fmt.Printf("  %s （目录不存在）\n", padRight(b, 14))
			continue
		}
		fmt.Printf("  %s %d\n", padRight(b, 14), n)
	}

	if st.EvidenceLedger {
		fmt.Printf("\n证据台账  有：%s\n", strings.Join(st.EvidenceLedgerPaths, "、"))
	} else {
		fmt.Println("\n证据台账  无（02_平台材料 下没有 *_evidence_ledger.json）")
	}

	fmt.Println("\n材料齐备度  ✓ 已有 · ✗ 缺失 · ? 判不了")
	byType := map[string]workflow.MaterialStatus{}
	for _, m := range st.Materials {
		byType[m.Type] = m
	}
	unknown := map[string]bool{}
	for _, t := range st.Unknown {
		unknown[t] = true
	}
	have, miss := 0, 0
	for _, stage := range reg.Stages {
		fmt.Printf("  [%s %s]\n", stage.ID, stage.Label)
		for _, m := range reg.Materials {
			if m.Stage != stage.ID {
				continue
			}
			if unknown[m.Type] {
				fmt.Printf("    ? %s  产物不在项目目录里（正本在题库，本命令判不了）\n", padRight(m.Type, 14))
				continue
			}
			ms := byType[m.Type]
			if ms.Present {
				have++
				fmt.Printf("    ✓ %s  %s\n", padRight(m.Type, 14), strings.Join(ms.Paths, "、"))
				continue
			}
			miss++
			fmt.Printf("    ✗ %s\n", m.Type)
		}
	}
	fmt.Printf("\n汇总  已有 %d · 缺失 %d · 判不了 %d（共 %d 种材料）\n",
		have, miss, len(st.Unknown), len(reg.Materials))
	fmt.Println("提示  判定只认「注册表落桶 + 扩展名 + 命名关键词」三重条件，宁可漏判也不误判；")
	fmt.Println("      产物落错桶或改了名会被判成缺失，加 --json 可看每条的判定规则。")
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
