// prompt 装配。
//
// 结构参照 NASApp 的 `engine.ts:253 buildPrompt` + `pipeline.ts` 层层追加
// （调研 01 §4.2），但按 M0 结论**精简**：NASApp 那一大段"本环境说明"补丁
// （`pipeline.ts:379-433`）是在纠正"SKILL.md 按老板 Mac 目录写、App 环境没有那些
// 路径"的错配；OneCreat 就跑在本机的项目目录里、有真的七目录、有 bash 与文件工具，
// 所以那批逐材料补丁**删掉**，换成一段统一的七目录环境说明
// （00_架构规划_v1.md §4.4 / 06_M2执行方案.md §2）。
//
// 本文件是**纯函数**：skill 正文由调用方解析好传进来（本层不 import internal/skill），
// 依赖产物正文也由调用方读好传进来，所以整段装配可以在单测里逐字断言。
package workflow

import (
	"fmt"
	"sort"
	"strings"

	"reasonix/internal/project"
	"reasonix/internal/workflow/registry"
)

// 段落顺序照 NASApp 的经验：**越靠后模型越重视**，所以老师的额外要求排在
// 依赖产物之后、收尾指令之前。
//
// AssemblePrompt 这个函数做什么：把一次材料作业的全部上下文组装成一条 prompt。
//
//   - reg / proj 不能为空；materialType 可以是旧别名，内部归一。
//   - skillBody 是该材料对应 skill 的 SKILL.md 正文（空串表示没拿到，会略去这一段）。
//   - deps 的键是**材料名**，值是那份上游产物的正文（调用方已按需拼好文件名与内容）。
//   - note 是老师就地填的一句话额外要求，可空。
func AssemblePrompt(reg *registry.Registry, proj *project.Project, materialType, skillBody string, deps map[string][]byte, note string) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("注册表为空")
	}
	if proj == nil {
		return "", fmt.Errorf("项目为空")
	}
	canon := reg.Canonical(materialType)
	m, ok := reg.Material(canon)
	if !ok {
		return "", fmt.Errorf("%w: %q", project.ErrUnknownMaterial, materialType)
	}
	bucket := bucketOfOutputs(m)
	if bucket == "" {
		return "", fmt.Errorf("材料 %q 的产物不落在项目七目录里（outputs.dir=%q），本层装配不了", canon, outputsDir(m))
	}

	var b strings.Builder
	// 1. 抬头：点名 skill、点名材料、点名"直接写文件"。
	fmt.Fprintf(&b, "使用 %s 技能，为下面这个学生项目生成「%s」，直接把产物文件写进项目目录。\n", m.Skill, canon)

	// 2. 项目信息（project.json 摘要，不是全文——全文里有分配、时间戳等与生成无关的字段）。
	b.WriteString("\n## 项目信息\n\n")
	b.WriteString(projectSummary(reg, proj))

	// 3. 依赖产物全文：硬依赖在前、软依赖在后，按注册表声明顺序，剩下的按名字排序兜底。
	for _, name := range depOrder(reg, m, deps) {
		body := strings.TrimRight(string(deps[name]), "\n")
		if strings.TrimSpace(body) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## 本项目已有的「%s」（完整内容，请以它为主要依据）\n\n", name)
		b.WriteString(body)
		b.WriteString("\n")
	}

	// 4. skill 正文。
	if strings.TrimSpace(skillBody) != "" {
		fmt.Fprintf(&b, "\n## %s 技能正文（按它的方法与格式做）\n\n", m.Skill)
		b.WriteString(strings.TrimRight(skillBody, "\n"))
		b.WriteString("\n")
	}

	// 5. 环境与落桶：一段统一说明，替代 NASApp 的逐材料补丁。
	b.WriteString("\n## 运行环境与产物落点\n\n")
	b.WriteString(environmentNote(bucket, m))

	// 6. 老师的额外要求（本次有效，优先满足）。
	if strings.TrimSpace(note) != "" {
		b.WriteString("\n## 本次生成的额外要求（老师就地填的，仅本次有效，优先满足）\n\n")
		b.WriteString(strings.TrimSpace(note))
		b.WriteString("\n")
	}

	// 7. 收尾指令：headless 作业没有人可以问，问了就是卡死。
	b.WriteString("\n注意：不要问我任何问题，缺的信息你自己合理假设，直接生成文件。\n")
	return b.String(), nil
}

// projectSummary 把 project.json 的关键字段写成几行人话。
func projectSummary(reg *registry.Registry, p *project.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- 项目编号：%s\n", orDash(p.Code()))
	fmt.Fprintf(&b, "- 项目短名：%s\n", orDash(p.ShortName()))
	fmt.Fprintf(&b, "- 课题全名：%s\n", orDash(p.FullName()))
	fmt.Fprintf(&b, "- 业务线：%s\n", orDash(p.Line()))
	if s := p.Student(); s != "" {
		fmt.Fprintf(&b, "- 学生：%s\n", s)
	}
	if n, ok := p.Stage(); ok {
		name := StageName(reg, n)
		if name == "" {
			fmt.Fprintf(&b, "- 项目当前段位：%d\n", n)
		} else {
			fmt.Fprintf(&b, "- 项目当前段位：%d（%s）\n", n, name)
		}
	}
	return b.String()
}

// environmentNote 是那段统一的七目录环境说明。
//
// 它要回答模型三个问题：我在哪、产物往哪写、什么不许干。
func environmentNote(bucket string, m *registry.Material) string {
	var b strings.Builder
	b.WriteString("- 你的当前工作目录**就是这个项目的根目录**，本机真实存在，可以直接用文件工具与 bash 读写。\n")
	b.WriteString("- 项目根下是固定的七目录，名称逐字如下，不许自造第八个顶层目录、不许改名：\n")
	for _, one := range project.Buckets {
		b.WriteString("  - " + one + "\n")
	}
	fmt.Fprintf(&b, "- 本次产物**必须**写进 `%s/` 下面（可以在它下面建子目录）。\n", bucket)
	if m.Outputs != nil {
		if len(m.Outputs.Exts) > 0 {
			fmt.Fprintf(&b, "- 交付物扩展名限定为：%s。\n", strings.Join(m.Outputs.Exts, "、"))
		}
		if m.Outputs.Naming != "" {
			fmt.Fprintf(&b, "- 文件命名约定：%s\n", m.Outputs.Naming)
		}
	}
	b.WriteString("- 不要改动项目根的 project.json，不要搬动或改名已有文件，只新增你这次的产物。\n")
	b.WriteString("- 交付物必须是**真的落盘文件**：只在回复里贴内容不算完成。\n")
	return b.String()
}

// depOrder 定出依赖产物的注入顺序：硬依赖（注册表声明序）→ 软依赖（注册表声明序）
// → 其余键（按名字排序）。顺序固定，prompt 才可逐字断言。
func depOrder(reg *registry.Registry, m *registry.Material, deps map[string][]byte) []string {
	var out []string
	seen := map[string]bool{}
	take := func(name string) {
		canon := reg.Canonical(name)
		for _, key := range []string{name, canon} {
			if _, ok := deps[key]; ok && !seen[key] {
				seen[key] = true
				out = append(out, key)
				return
			}
		}
	}
	for _, d := range m.Deps {
		take(d)
	}
	for _, d := range m.SoftDeps {
		take(d)
	}
	var rest []string
	for k := range deps {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func outputsDir(m *registry.Material) string {
	if m.Outputs == nil {
		return ""
	}
	return m.Outputs.Dir
}

func orDash(s string) string {
	if s == "" {
		return "（未填）"
	}
	return s
}
