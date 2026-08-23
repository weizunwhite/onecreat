// Package registry 是科创工作流注册表（v17）的唯一真源与加载器。
//
// 注册表正文 workflow_registry.json 用 go:embed 内嵌进二进制：阶段、材料、skill、
// 依赖、输出位置、平台 file_type 与全部枚举都只在那一份 JSON 里定义一次。任何消费侧
// 都必须经 Load() 拿数据，遇到未注册的材料类型一律报错，不得兜底猜测。
//
// 边界（架构铁律，改动前先读 docs/科创工作流迁移/00_架构规划_v1.md §8.0 A1）：
// 本包属于领域层，只依赖标准库；它**不得**被 internal/engine、internal/engine/dsh、
// internal/agent import，也不 import control / toolpolicy / config / tool。
// 反过来它也不认识 Controller——注册表是数据，不是运行时。
package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed workflow_registry.json
var embedded []byte

// SchemaVersion 是本加载器唯一接受的注册表版本号。
const SchemaVersion = 17

// 必须存在的枚举键。少一个就装配失败——枚举合一是 v17 的核心目的。
var requiredEnums = []string{
	"groups",
	"sources",
	"lines",
	"project_stages",
	"teaching_flows",
	"legacy_board_columns",
	"topic_states",
	"thread_statuses",
	"decisions",
	"stages",
	"material_kinds",
	"material_statuses",
	"modes",
	"engines",
	"frontmatter_kinds",
	"upload_via",
	"platform_file_types",
}

// Registry 是注册表正文。字段与 workflow_registry.json 一一对应；
// board / topic_rules / source_of_truth 是说明性区块，这里原样保留不建模。
type Registry struct {
	SchemaVersion      int              `json:"schema_version"`
	Version            string           `json:"version"`
	Updated            string           `json:"updated"`
	UpdatedAt          string           `json:"updated_at"`
	Note               string           `json:"note,omitempty"`
	SourceOfTruth      json.RawMessage  `json:"source_of_truth,omitempty"`
	Enums              map[string]Enum  `json:"enums"`
	Naming             Naming           `json:"naming"`
	Aliases            Aliases          `json:"aliases"`
	Stages             []Stage          `json:"stages"`
	Materials          []Material       `json:"materials"`
	PlatformActions    []PlatformAction `json:"platform_actions,omitempty"`
	SkillsNotMaterials []SkillNote      `json:"skills_not_materials,omitempty"`
	Board              json.RawMessage  `json:"board,omitempty"`
	TopicRules         json.RawMessage  `json:"topic_rules,omitempty"`
}

// Enum 是一个枚举的定义点。Values 是唯一合法取值集合；Detail 给每个取值挂说明，
// 存在时必须与 Values 一一对应（既不多也不少）。
type Enum struct {
	Authority string      `json:"authority,omitempty"`
	Note      string      `json:"note,omitempty"`
	ReadOnly  bool        `json:"read_only,omitempty"`
	Values    []string    `json:"values"`
	Planned   []string    `json:"planned,omitempty"`
	Detail    []EnumEntry `json:"detail,omitempty"`
}

// EnumEntry 是枚举里单个取值的说明。
type EnumEntry struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
	No    int    `json:"no,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Naming 是文件与目录的命名契约。
type Naming struct {
	Note string `json:"note,omitempty"`
	// Groups 是 v16 遗留的组别副本。v17 把组别收进 enums.groups，
	// 这里必须为空——留这个字段就是为了在它复活时校验报错。
	Groups                   []string   `json:"groups,omitempty"`
	GroupsRef                string     `json:"groups_ref"`
	DateFormat               string     `json:"date_format"`
	DatePattern              string     `json:"date_pattern,omitempty"`
	TopicFile                string     `json:"topic_file"`
	UnestablishedDir         string     `json:"unestablished_dir"`
	FrontmatterRequired      []string   `json:"frontmatter_required"`
	TopicKey                 string     `json:"topic_key"`
	TopicKeyAlgorithm        *Algorithm `json:"topic_key_algorithm,omitempty"`
	UnestablishedTopicCharse *Charset   `json:"unestablished_topic_charset,omitempty"`
}

// Algorithm 是一段有序的归一化步骤说明（人读为主，实现照它写）。
type Algorithm struct {
	Note  string   `json:"note,omitempty"`
	Steps []string `json:"steps"`
}

// Charset 是未立项目录名的字符集收口规则。
type Charset struct {
	Note          string   `json:"note,omitempty"`
	Steps         []string `json:"steps"`
	MaxLength     int      `json:"max_length"`
	Fallback      string   `json:"fallback"`
	FolderPattern string   `json:"folder_pattern,omitempty"`
}

// Aliases 是旧名到新名的兼容表：只做输入兼容，展示与产出一律用目标类型。
type Aliases struct {
	Note      string            `json:"note,omitempty"`
	Materials map[string]string `json:"materials"`
}

// Stage 是科创一条链五段之一。
type Stage struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Order       int      `json:"order"`
	Kind        string   `json:"kind"`
	AppTab      string   `json:"app_tab"`
	M4Root      string   `json:"m4_root,omitempty"`
	NASRoot     string   `json:"nas_root,omitempty"`
	Bucket      string   `json:"bucket,omitempty"`
	Entries     []string `json:"entries,omitempty"`
	RecordsFrom []string `json:"records_from,omitempty"`
	Board       string   `json:"board,omitempty"`
	Actions     []string `json:"actions,omitempty"`
}

// Material 是一个工作流节点（24 种材料之一）。
type Material struct {
	Type          string    `json:"type"`
	Stage         string    `json:"stage"`
	Kind          string    `json:"kind"`
	Skill         string    `json:"skill"`
	SkillPlugin   string    `json:"skill_plugin,omitempty"`
	SkillMissing  bool      `json:"skill_missing,omitempty"`
	ModeHint      string    `json:"mode_hint,omitempty"`
	Modes         []string  `json:"modes,omitempty"`
	DefaultMode   string    `json:"default_mode,omitempty"`
	DefaultEngine string    `json:"default_engine,omitempty"`
	Deps          []string  `json:"deps"`
	SoftDeps      []string  `json:"soft_deps,omitempty"`
	Outputs       *Outputs  `json:"outputs,omitempty"`
	Collab        *Collab   `json:"collab,omitempty"`
	Decisions     []string  `json:"decisions,omitempty"`
	Platform      *Platform `json:"platform,omitempty"`
	Status        string    `json:"status"`
	Notes         string    `json:"notes,omitempty"`
	App           *AppHint  `json:"app,omitempty"`
}

// Outputs 是材料的产物落点与命名。
type Outputs struct {
	Naming          string   `json:"naming,omitempty"`
	Dir             string   `json:"dir,omitempty"`
	AppDir          string   `json:"app_dir,omitempty"`
	Exts            []string `json:"exts,omitempty"`
	FrontmatterKind string   `json:"frontmatter_kind,omitempty"`
	Note            string   `json:"note,omitempty"`
}

// Collab 是协作投放段的推送落点。
type Collab struct {
	PushDir string `json:"push_dir"`
}

// Platform 是材料与教师平台的对接口径。FileType 可以是 null（不上传），
// 也可以用 | 分隔表示一次产出多个 file_type。
type Platform struct {
	FileType  *string `json:"file_type"`
	UploadVia string  `json:"upload_via"`
	Preflight string  `json:"preflight,omitempty"`
	Note      string  `json:"note,omitempty"`
}

// AppHint 是客户端列表页的图标与副标题。
type AppHint struct {
	Icon     string `json:"icon,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
}

// PlatformAction 是不产材料、只与平台交互的动作。
type PlatformAction struct {
	Type    string   `json:"type"`
	Stage   string   `json:"stage"`
	Via     string   `json:"via"`
	Outputs *Outputs `json:"outputs,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// SkillNote 记录 skill 仓里存在、但**故意不登记为材料**的 skill 及理由。
// 它的作用是让"注册表 vs skill 仓"的差集有据可查，而不是靠人记。
type SkillNote struct {
	Skill   string `json:"skill"`
	Plugin  string `json:"plugin"`
	Purpose string `json:"purpose"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// Load 读内嵌注册表并校验。校验不过返回错误——fail-closed，绝不返回半个注册表。
func Load() (*Registry, error) {
	return Parse(embedded)
}

// Parse 解析并校验一份注册表正文。测试用它加载篡改后的副本。
func Parse(data []byte) (*Registry, error) {
	var reg Registry
	dec := json.NewDecoder(bytes.NewReader(data))
	// 拒绝未建模的字段：JSON 里加了键而 Go 结构没跟上，属于"改了真源没改消费侧"，
	// 必须在装配期红，而不是被静默丢掉。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("注册表 JSON 解析失败: %w", err)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// Raw 返回内嵌注册表的原始字节，供 round-trip 测试比对。
func Raw() []byte {
	out := make([]byte, len(embedded))
	copy(out, embedded)
	return out
}

// Material 按类型查材料；未注册返回 false（调用方必须报错，不许兜底猜测）。
func (r *Registry) Material(materialType string) (*Material, bool) {
	for i := range r.Materials {
		if r.Materials[i].Type == materialType {
			return &r.Materials[i], true
		}
	}
	return nil, false
}

// Stage 按 id 查阶段。
func (r *Registry) Stage(id string) (*Stage, bool) {
	for i := range r.Stages {
		if r.Stages[i].ID == id {
			return &r.Stages[i], true
		}
	}
	return nil, false
}

// EnumValues 返回某个枚举的合法取值；枚举不存在返回 false。
func (r *Registry) EnumValues(name string) ([]string, bool) {
	e, ok := r.Enums[name]
	if !ok {
		return nil, false
	}
	out := make([]string, len(e.Values))
	copy(out, e.Values)
	return out, true
}

// Canonical 把可能是旧名的材料类型归一到正式类型。
func (r *Registry) Canonical(materialType string) string {
	if target, ok := r.Aliases.Materials[materialType]; ok {
		return target
	}
	return materialType
}

// ResolveSkill 把材料类型（可以是旧别名）解析成 skill 名；未注册返回 false。
func (r *Registry) ResolveSkill(materialType string) (string, bool) {
	m, ok := r.Material(r.Canonical(materialType))
	if !ok {
		return "", false
	}
	return m.Skill, true
}

// MaterialOrder 返回一个满足依赖关系的材料生成顺序（deps + soft_deps 都算边）。
// 同层内按注册表出现顺序，保证结果稳定可比。
func (r *Registry) MaterialOrder() ([]string, error) {
	return topoSort(r.Materials)
}

// Validate 是注册表的全部机械约束。任何一条不满足都直接失败。
func (r *Registry) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("注册表 schema_version 必须是 %d，实际 %d", SchemaVersion, r.SchemaVersion)
	}
	if err := r.validateEnums(); err != nil {
		return err
	}
	if err := r.validateNaming(); err != nil {
		return err
	}
	if err := r.validateStages(); err != nil {
		return err
	}
	if err := r.validateMaterials(); err != nil {
		return err
	}
	if err := r.validateAliases(); err != nil {
		return err
	}
	if _, err := topoSort(r.Materials); err != nil {
		return err
	}
	return r.validateSkillNotes()
}

func (r *Registry) validateEnums() error {
	if len(r.Enums) == 0 {
		return fmt.Errorf("注册表缺 enums 块：v17 要求所有枚举只在 enums 里定义一次")
	}
	for _, name := range requiredEnums {
		if _, ok := r.Enums[name]; !ok {
			return fmt.Errorf("注册表 enums 缺必备枚举: %s", name)
		}
	}
	for _, name := range sortedKeys(r.Enums) {
		e := r.Enums[name]
		if len(e.Values) == 0 {
			return fmt.Errorf("枚举 %s 的 values 为空", name)
		}
		seen := map[string]bool{}
		for _, v := range e.Values {
			if v == "" {
				return fmt.Errorf("枚举 %s 含空取值", name)
			}
			if seen[v] {
				return fmt.Errorf("枚举 %s 取值重复: %s", name, v)
			}
			seen[v] = true
		}
		if len(e.Detail) == 0 {
			continue
		}
		covered := map[string]bool{}
		for _, d := range e.Detail {
			if !seen[d.Value] {
				return fmt.Errorf("枚举 %s 的 detail 出现 values 之外的取值: %s", name, d.Value)
			}
			if covered[d.Value] {
				return fmt.Errorf("枚举 %s 的 detail 取值重复: %s", name, d.Value)
			}
			covered[d.Value] = true
		}
		if len(covered) != len(e.Values) {
			return fmt.Errorf("枚举 %s 的 detail 没有覆盖全部取值（%d/%d）", name, len(covered), len(e.Values))
		}
	}
	return nil
}

func (r *Registry) validateNaming() error {
	if len(r.Naming.Groups) > 0 {
		return fmt.Errorf("naming.groups 是 v16 的组别副本，v17 已收进 enums.groups，不许复活")
	}
	if r.Naming.GroupsRef != "enums.groups" {
		return fmt.Errorf("naming.groups_ref 必须指向 enums.groups，实际 %q", r.Naming.GroupsRef)
	}
	if r.Naming.TopicFile == "" || r.Naming.UnestablishedDir == "" || r.Naming.DateFormat == "" {
		return fmt.Errorf("naming 缺 topic_file / unestablished_dir / date_format")
	}
	if len(r.Naming.FrontmatterRequired) == 0 {
		return fmt.Errorf("naming.frontmatter_required 为空")
	}
	if r.Naming.TopicKey == "" {
		return fmt.Errorf("naming.topic_key 为空")
	}
	return nil
}

func (r *Registry) validateStages() error {
	if len(r.Stages) == 0 {
		return fmt.Errorf("注册表 stages 为空")
	}
	declared, _ := r.EnumValues("stages")
	allowedKinds := r.enumSet("material_kinds")
	seen := map[string]bool{}
	for i, s := range r.Stages {
		if s.ID == "" {
			return fmt.Errorf("第 %d 个阶段没有 id", i+1)
		}
		if seen[s.ID] {
			return fmt.Errorf("阶段 id 重复: %s", s.ID)
		}
		seen[s.ID] = true
		if s.Order != i+1 {
			return fmt.Errorf("阶段 %s 的 order 应为 %d，实际 %d（order 必须从 1 开始连续）", s.ID, i+1, s.Order)
		}
		if s.Label == "" {
			return fmt.Errorf("阶段 %s 没有 label", s.ID)
		}
		if !allowedKinds[s.Kind] {
			return fmt.Errorf("阶段 %s 的 kind %q 不在 enums.material_kinds 内", s.ID, s.Kind)
		}
	}
	if len(declared) != len(r.Stages) {
		return fmt.Errorf("enums.stages 有 %d 个取值，stages 块有 %d 个阶段", len(declared), len(r.Stages))
	}
	for _, id := range declared {
		if !seen[id] {
			return fmt.Errorf("enums.stages 声明的阶段 %s 在 stages 块里没有定义", id)
		}
	}
	return nil
}

func (r *Registry) validateMaterials() error {
	if len(r.Materials) == 0 {
		return fmt.Errorf("注册表 materials 为空")
	}
	stageIDs := map[string]bool{}
	for _, s := range r.Stages {
		stageIDs[s.ID] = true
	}
	kinds := r.enumSet("material_kinds")
	statuses := r.enumSet("material_statuses")
	modes := r.enumSet("modes")
	engines := r.enumSet("engines")
	fmKinds := r.enumSet("frontmatter_kinds")
	uploads := r.enumSet("upload_via")
	fileTypes := r.enumSet("platform_file_types")
	decisions := r.enumSet("decisions")

	types := map[string]bool{}
	for _, m := range r.Materials {
		if m.Type == "" {
			return fmt.Errorf("有材料没有 type")
		}
		if types[m.Type] {
			return fmt.Errorf("材料 type 重复: %s", m.Type)
		}
		types[m.Type] = true
	}
	for _, m := range r.Materials {
		if !stageIDs[m.Stage] {
			return fmt.Errorf("材料 %s 的 stage %q 不是已注册阶段", m.Type, m.Stage)
		}
		if !kinds[m.Kind] {
			return fmt.Errorf("材料 %s 的 kind %q 不在 enums.material_kinds 内", m.Type, m.Kind)
		}
		if !statuses[m.Status] {
			return fmt.Errorf("材料 %s 的 status %q 不在 enums.material_statuses 内", m.Type, m.Status)
		}
		if m.Skill == "" {
			return fmt.Errorf("材料 %s 没有 skill", m.Type)
		}
		for _, mode := range m.Modes {
			if !modes[mode] {
				return fmt.Errorf("材料 %s 的 mode %q 不在 enums.modes 内", m.Type, mode)
			}
		}
		if m.DefaultMode != "" {
			if !modes[m.DefaultMode] {
				return fmt.Errorf("材料 %s 的 default_mode %q 不在 enums.modes 内", m.Type, m.DefaultMode)
			}
			if len(m.Modes) > 0 && !contains(m.Modes, m.DefaultMode) {
				return fmt.Errorf("材料 %s 的 default_mode %q 不在自己的 modes 里", m.Type, m.DefaultMode)
			}
		}
		if m.DefaultEngine != "" && !engines[m.DefaultEngine] {
			return fmt.Errorf("材料 %s 的 default_engine %q 不在 enums.engines 内", m.Type, m.DefaultEngine)
		}
		if m.Outputs != nil && m.Outputs.FrontmatterKind != "" && !fmKinds[m.Outputs.FrontmatterKind] {
			return fmt.Errorf("材料 %s 的 outputs.frontmatter_kind %q 不在 enums.frontmatter_kinds 内", m.Type, m.Outputs.FrontmatterKind)
		}
		if m.Platform != nil {
			if !uploads[m.Platform.UploadVia] {
				return fmt.Errorf("材料 %s 的 platform.upload_via %q 不在 enums.upload_via 内", m.Type, m.Platform.UploadVia)
			}
			if m.Platform.FileType != nil {
				for _, ft := range strings.Split(*m.Platform.FileType, "|") {
					if !fileTypes[ft] {
						return fmt.Errorf("材料 %s 的 platform.file_type %q 不在 enums.platform_file_types 内", m.Type, ft)
					}
				}
			}
		}
		for _, d := range m.Decisions {
			if !decisions[d] {
				return fmt.Errorf("材料 %s 的 decision %q 不在 enums.decisions 内", m.Type, d)
			}
		}
		for _, dep := range m.Deps {
			if !types[dep] {
				return fmt.Errorf("材料 %s 的 deps 指向未注册材料: %s", m.Type, dep)
			}
			if dep == m.Type {
				return fmt.Errorf("材料 %s 的 deps 指向自己", m.Type)
			}
		}
		for _, dep := range m.SoftDeps {
			if !types[dep] {
				return fmt.Errorf("材料 %s 的 soft_deps 指向未注册材料: %s", m.Type, dep)
			}
			if dep == m.Type {
				return fmt.Errorf("材料 %s 的 soft_deps 指向自己", m.Type)
			}
		}
	}
	return nil
}

func (r *Registry) validateAliases() error {
	for _, alias := range sortedKeys(r.Aliases.Materials) {
		target := r.Aliases.Materials[alias]
		if _, ok := r.Material(target); !ok {
			return fmt.Errorf("别名 %s 指向未注册材料: %s", alias, target)
		}
		if _, ok := r.Material(alias); ok {
			return fmt.Errorf("别名 %s 与正式材料同名，会遮蔽正式类型", alias)
		}
	}
	return nil
}

func (r *Registry) validateSkillNotes() error {
	for _, n := range r.SkillsNotMaterials {
		if n.Skill == "" || n.Reason == "" {
			return fmt.Errorf("skills_not_materials 条目缺 skill 或 reason")
		}
		for _, m := range r.Materials {
			if m.Skill == n.Skill {
				return fmt.Errorf("skill %s 既被登记为材料 %s 的 skill，又出现在 skills_not_materials 里", n.Skill, m.Type)
			}
		}
	}
	return nil
}

func (r *Registry) enumSet(name string) map[string]bool {
	out := map[string]bool{}
	for _, v := range r.Enums[name].Values {
		out[v] = true
	}
	return out
}

// topoSort 对材料的依赖图做拓扑排序；有环就返回错误并点名环上的材料。
// deps 与 soft_deps 都算边——软依赖也是"先做谁"的顺序约束，成环同样是配置错误。
func topoSort(materials []Material) ([]string, error) {
	order := make([]string, 0, len(materials))
	// 0=未访问 1=访问中 2=已完成
	state := map[string]int{}
	index := map[string]*Material{}
	for i := range materials {
		index[materials[i].Type] = &materials[i]
	}
	var visit func(t string, path []string) error
	visit = func(t string, path []string) error {
		switch state[t] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("材料依赖成环: %s", strings.Join(append(path, t), " → "))
		}
		state[t] = 1
		m := index[t]
		if m != nil {
			deps := append(append([]string{}, m.Deps...), m.SoftDeps...)
			for _, dep := range deps {
				if index[dep] == nil {
					continue // 悬空依赖由 validateMaterials 报，这里只管环
				}
				if err := visit(dep, append(path, t)); err != nil {
					return err
				}
			}
		}
		state[t] = 2
		order = append(order, t)
		return nil
	}
	for i := range materials {
		if err := visit(materials[i].Type, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
