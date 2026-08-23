package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tamper 把内嵌注册表解成通用 map，让测试改坏其中一处，再序列化回去。
// 用副本篡改而不是改真源，是为了让每条校验都有一个"会红"的用例。
func tamper(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(Raw(), &m); err != nil {
		t.Fatalf("解析内嵌注册表失败: %v", err)
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化篡改副本失败: %v", err)
	}
	return out
}

// materialsOf 取出篡改副本里的材料数组。
func materialsOf(t *testing.T, m map[string]any) []any {
	t.Helper()
	list, ok := m["materials"].([]any)
	if !ok {
		t.Fatalf("materials 不是数组")
	}
	return list
}

// findMaterial 在篡改副本里按类型找一条材料。
func findMaterial(t *testing.T, m map[string]any, materialType string) map[string]any {
	t.Helper()
	for _, raw := range materialsOf(t, m) {
		entry, ok := raw.(map[string]any)
		if ok && entry["type"] == materialType {
			return entry
		}
	}
	t.Fatalf("副本里没有材料 %s", materialType)
	return nil
}

// mustFail 断言篡改后的副本一定加载失败，且错误里点到了关键词。
func mustFail(t *testing.T, data []byte, wantSubstring string) {
	t.Helper()
	_, err := Parse(data)
	if err == nil {
		t.Fatalf("篡改后的注册表本应加载失败，却通过了")
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("错误信息没点到 %q：%v", wantSubstring, err)
	}
}

func TestEmbeddedRegistryIsValid(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("内嵌注册表校验不过: %v", err)
	}
	if reg.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %d，want %d", reg.SchemaVersion, SchemaVersion)
	}
	if len(reg.Stages) != 5 {
		t.Fatalf("阶段数 = %d，want 5（科创一条链五段）", len(reg.Stages))
	}
	// 25 而不是文档常说的 24：v16 注册表实际就是 25 条，
	// 调研报告 §2.1④ 与规划 §4.3 的「24 种」是数错了（详见 02_注册表v17收口说明.md）。
	if len(reg.Materials) != 25 {
		t.Fatalf("材料数 = %d，want 25", len(reg.Materials))
	}
	if len(reg.Aliases.Materials) == 0 {
		t.Fatal("别名表为空，旧客户端发来的旧名会 fail-closed")
	}
}

// 结构体必须能无损承载注册表正文：解出来再序列化回去，和原文语义相同。
// 它抓的是"JSON 里加了字段但 Go 没跟上"和"omitempty 把有值字段吃掉"两类事故。
func TestRegistryJSONRoundTrips(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	encoded, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("回写失败: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(Raw(), &before); err != nil {
		t.Fatalf("解析原文失败: %v", err)
	}
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatalf("解析回写结果失败: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("注册表 round-trip 后内容不一致：有字段被 Go 结构丢掉或改写了")
	}
}

// 枚举合一守卫：材料与阶段条目里出现的每一个枚举字面值，都必须能在 enums 里找到；
// 并且 v16 那份 naming.groups 副本不许复活。
func TestEnumsDefinedOnce(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(Raw(), &raw); err != nil {
		t.Fatalf("解析原文失败: %v", err)
	}

	if naming, ok := raw["naming"].(map[string]any); ok {
		if _, dup := naming["groups"]; dup {
			t.Fatal("naming.groups 又出现了：组别枚举只能定义在 enums.groups")
		}
	}

	// 材料条目里带枚举语义的字段 → 它必须落在哪个枚举里。
	scalarFields := map[string]string{
		"stage":          "stages",
		"kind":           "material_kinds",
		"status":         "material_statuses",
		"default_mode":   "modes",
		"default_engine": "engines",
	}
	listFields := map[string]string{
		"modes":     "modes",
		"decisions": "decisions",
	}
	check := func(where, enumName, value string) {
		t.Helper()
		values, ok := reg.EnumValues(enumName)
		if !ok {
			t.Fatalf("%s 引用了不存在的枚举 %s", where, enumName)
		}
		if !contains(values, value) {
			t.Fatalf("%s 出现了 enums.%s 之外的字面值 %q", where, enumName, value)
		}
	}

	for _, item := range materialsOf(t, raw) {
		entry := item.(map[string]any)
		where := "材料 " + entry["type"].(string)
		for field, enumName := range scalarFields {
			if v, ok := entry[field].(string); ok {
				check(where, enumName, v)
			}
		}
		for field, enumName := range listFields {
			list, ok := entry[field].([]any)
			if !ok {
				continue
			}
			for _, v := range list {
				check(where, enumName, v.(string))
			}
		}
		if outputs, ok := entry["outputs"].(map[string]any); ok {
			if v, ok := outputs["frontmatter_kind"].(string); ok {
				check(where+".outputs", "frontmatter_kinds", v)
			}
		}
		if platform, ok := entry["platform"].(map[string]any); ok {
			if v, ok := platform["upload_via"].(string); ok {
				check(where+".platform", "upload_via", v)
			}
			if v, ok := platform["file_type"].(string); ok {
				for _, ft := range strings.Split(v, "|") {
					check(where+".platform", "platform_file_types", ft)
				}
			}
		}
	}

	stages, _ := raw["stages"].([]any)
	for _, item := range stages {
		entry := item.(map[string]any)
		where := "阶段 " + entry["id"].(string)
		check(where, "stages", entry["id"].(string))
		check(where, "material_kinds", entry["kind"].(string))
	}
}

func TestRejectsWrongSchemaVersion(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		m["schema_version"] = 16
	}), "schema_version")
}

func TestRejectsMissingRequiredEnum(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		delete(m["enums"].(map[string]any), "groups")
	}), "缺必备枚举")
}

func TestRejectsRevivedNamingGroups(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		m["naming"].(map[string]any)["groups"] = []any{"小学低", "初中"}
	}), "naming.groups")
}

func TestRejectsEnumDetailOutsideValues(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		lines := m["enums"].(map[string]any)["lines"].(map[string]any)
		detail := lines["detail"].([]any)
		detail[0].(map[string]any)["value"] = "X"
		lines["detail"] = detail
	}), "detail")
}

func TestRejectsDuplicateStageID(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		stages := m["stages"].([]any)
		stages[1].(map[string]any)["id"] = "source"
		m["stages"] = stages
	}), "阶段 id 重复")
}

func TestRejectsNonContiguousStageOrder(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		stages := m["stages"].([]any)
		stages[2].(map[string]any)["order"] = 9
		m["stages"] = stages
	}), "order")
}

func TestRejectsDuplicateMaterialType(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "教案")["type"] = "技术方案"
	}), "材料 type 重复")
}

func TestRejectsMaterialInUnknownStage(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "教案")["stage"] = "不存在的阶段"
	}), "不是已注册阶段")
}

func TestRejectsDanglingDep(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "教案")["deps"] = []any{"根本没有这份材料"}
	}), "deps 指向未注册材料")
}

func TestRejectsDanglingSoftDep(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "论文")["soft_deps"] = []any{"幽灵材料"}
	}), "soft_deps 指向未注册材料")
}

func TestRejectsDependencyCycle(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		// 技术方案 ← 教案；再让技术方案依赖教案就成了环。
		findMaterial(t, m, "技术方案")["deps"] = []any{"教案"}
	}), "成环")
}

func TestRejectsUnknownAliasTarget(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		aliases := m["aliases"].(map[string]any)["materials"].(map[string]any)
		aliases["选题"] = "已经删掉的材料"
	}), "别名")
}

func TestRejectsLiteralOutsideEnums(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "课件")["status"] = "coming" // v16 的旧取值，v17 已收口成三值
	}), "material_statuses")
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "立项")["default_engine"] = "gpt"
	}), "engines")
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "技术方案")["platform"].(map[string]any)["file_type"] = "没登记的类型"
	}), "platform_file_types")
	mustFail(t, tamper(t, func(m map[string]any) {
		findMaterial(t, m, "快速产题")["outputs"].(map[string]any)["frontmatter_kind"] = "随便写"
	}), "frontmatter_kinds")
}

func TestRejectsSkillBothRegisteredAndExcluded(t *testing.T) {
	mustFail(t, tamper(t, func(m map[string]any) {
		notes := m["skills_not_materials"].([]any)
		notes[0].(map[string]any)["skill"] = "tech-proposal-generator"
		m["skills_not_materials"] = notes
	}), "skills_not_materials")
}

func TestResolveSkillFollowsAliases(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	cases := map[string]string{
		"快速选题包": "topic-express",
		"选题":    "topic-mining",
		"评题":    "topic-scoring",
		"研究日志":  "project-research-log",
		"技术方案":  "tech-proposal-generator",
	}
	for input, want := range cases {
		got, ok := reg.ResolveSkill(input)
		if !ok {
			t.Fatalf("ResolveSkill(%q) 解析不出来", input)
		}
		if got != want {
			t.Fatalf("ResolveSkill(%q) = %q，want %q", input, got, want)
		}
	}
	if _, ok := reg.ResolveSkill("查无此材料"); ok {
		t.Fatal("未注册类型必须 fail-closed，不许兜底")
	}
}

func TestMaterialOrderPutsDepsFirst(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	order, err := reg.MaterialOrder()
	if err != nil {
		t.Fatalf("拓扑排序失败: %v", err)
	}
	if len(order) != len(reg.Materials) {
		t.Fatalf("排序结果 %d 条，材料 %d 条", len(order), len(reg.Materials))
	}
	pos := map[string]int{}
	for i, name := range order {
		pos[name] = i
	}
	for _, m := range reg.Materials {
		for _, dep := range append(append([]string{}, m.Deps...), m.SoftDeps...) {
			if pos[dep] >= pos[m.Type] {
				t.Fatalf("依赖 %s 排在了 %s 后面", dep, m.Type)
			}
		}
	}
}

// 并行期防漂移守卫：NASApp 的 v16 注册表还在跑，v17 必须与它对得上，
// 免得两边各改各的。M5 数据切换、NASApp 侧退役之后，这个测试可以删掉。
func TestNoDriftFromNASAppV16(t *testing.T) {
	const v16Path = "/Users/localwork/04_project/NASApp/brain/registry/workflow_registry.json"
	data, err := os.ReadFile(v16Path)
	if err != nil {
		t.Skipf("NASApp v16 注册表不在本机，跳过防漂移比对: %v", err)
	}
	var v16 struct {
		Materials []struct {
			Type  string   `json:"type"`
			Stage string   `json:"stage"`
			Kind  string   `json:"kind"`
			Skill string   `json:"skill"`
			Deps  []string `json:"deps"`
		} `json:"materials"`
	}
	if err := json.Unmarshal(data, &v16); err != nil {
		t.Fatalf("解析 v16 注册表失败: %v", err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载 v17 失败: %v", err)
	}
	if len(v16.Materials) != len(reg.Materials) {
		t.Fatalf("材料条数漂移：v16 %d 条，v17 %d 条", len(v16.Materials), len(reg.Materials))
	}
	for _, old := range v16.Materials {
		got, ok := reg.Material(old.Type)
		if !ok {
			t.Fatalf("v16 的材料 %s 在 v17 里没有了", old.Type)
		}
		if got.Stage != old.Stage {
			t.Errorf("材料 %s 的 stage 漂移：v16 %s，v17 %s", old.Type, old.Stage, got.Stage)
		}
		if got.Kind != old.Kind {
			t.Errorf("材料 %s 的 kind 漂移：v16 %s，v17 %s", old.Type, old.Kind, got.Kind)
		}
		if got.Skill != old.Skill {
			t.Errorf("材料 %s 的 skill 漂移：v16 %s，v17 %s", old.Type, old.Skill, got.Skill)
		}
		oldDeps := old.Deps
		if oldDeps == nil {
			oldDeps = []string{}
		}
		newDeps := got.Deps
		if newDeps == nil {
			newDeps = []string{}
		}
		if !reflect.DeepEqual(oldDeps, newDeps) {
			t.Errorf("材料 %s 的 deps 漂移：v16 %v，v17 %v", old.Type, oldDeps, newDeps)
		}
	}
}

// skill 仓对齐守卫：注册表登记的每个 skill 都得在 oneup-skills 里有 SKILL.md，
// 且落在注册表标注的那个插件下。skill 仓不在本机时跳过（它不在 OneCreat 仓内）。
func TestSkillsExistInOneupSkills(t *testing.T) {
	const skillRepo = "/Users/zunwei/code/oneup-skills"
	if _, err := os.Stat(skillRepo); err != nil {
		t.Skipf("skill 仓不在本机，跳过对齐检查: %v", err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	exists := func(plugin, skill string) bool {
		if plugin != "" {
			_, err := os.Stat(filepath.Join(skillRepo, plugin, "skills", skill, "SKILL.md"))
			return err == nil
		}
		hits, _ := filepath.Glob(filepath.Join(skillRepo, "*", "skills", skill, "SKILL.md"))
		return len(hits) > 0
	}
	for _, m := range reg.Materials {
		if m.SkillMissing {
			t.Logf("材料 %s 的 skill %s 被标记为缺失，跳过", m.Type, m.Skill)
			continue
		}
		if !exists(m.SkillPlugin, m.Skill) {
			t.Errorf("材料 %s 的 skill %s 在 %s/%s/skills/ 下找不到 SKILL.md（要么补 skill，要么标 skill_missing）",
				m.Type, m.Skill, skillRepo, m.SkillPlugin)
		}
	}
	for _, n := range reg.SkillsNotMaterials {
		if !exists(n.Plugin, n.Skill) {
			t.Errorf("skills_not_materials 记的 skill %s 在 %s/%s/skills/ 下找不到 SKILL.md",
				n.Skill, skillRepo, n.Plugin)
		}
	}
}
