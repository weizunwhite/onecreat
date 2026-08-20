package main

// hardwareService 是硬件面板(检测 / 装工具链 / 编译校验 / 烧录 / OTA / 串口监视 /
// 证据链导出)的后端。它通过 stdio JSON-RPC 直接调 reasonix-hardware-mcp 这个独立
// 二进制,不经过会话的 MCP host —— 面板按钮是用户的直接操作,不该等某个标签的 agent。
//
// 它自己持有两把「长操作互斥槽」:装工具链(arduino-cli 写同一 core 目录)和烧录
// (esptool 抢同一串口)各自不能并发。前端的按钮守卫是面板组件内 state,面板随视图
// 切换/收起被卸载时守卫丢失,重挂载后按钮恢复可点 → 会并发起第二次安装/烧录(写坏
// core / 串口抢占)。故在后端串行:进行中直接拒绝第二次并给出说明。
//
// 依赖用函数注入,不认识 App:root 是当前选中的项目文件夹,ctrl 是活动标签的
// controller(面板永远作用于活动标签),serial 是常驻串口监视器(烧录前要先让出串口)。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/hardware/boards"
)

type hardwareService struct {
	rootFn func() string
	ctrlFn func() *control.Controller
	serial *serialService

	// mu 保护下面两个互斥槽。
	mu         sync.Mutex
	installing bool
	flashing   bool
}

func newHardwareService(root func() string, ctrl func() *control.Controller, ser *serialService) *hardwareService {
	return &hardwareService{rootFn: root, ctrlFn: ctrl, serial: ser}
}

// root 是当前选中的项目文件夹。没有注入(单元测试里的零值 service)时退回进程 cwd,
// 与 App.workspaceRoot 的兜底一致。
func (h *hardwareService) root() string {
	if h == nil || h.rootFn == nil {
		wd, _ := os.Getwd()
		return wd
	}
	return h.rootFn()
}

// ctrl 是活动标签的 controller;没有标签或未注入时为 nil,调用方都已按 nil 处理。
func (h *hardwareService) ctrl() *control.Controller {
	if h == nil || h.ctrlFn == nil {
		return nil
	}
	return h.ctrlFn()
}

// HardwareMCPView describes the local hardware MCP binary the desktop can use.
type HardwareMCPView struct {
	Name       string `json:"name"`
	Available  bool   `json:"available"`
	Command    string `json:"command"`
	Source     string `json:"source"`
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Error      string `json:"error,omitempty"`
}

// HardwareBoardSummary is one selectable board for the hardware panel's board
// picker, sourced from the shared data-driven registry (internal/hardware/boards)
// so adding a board to boards.json automatically surfaces it in the UI.
type HardwareBoardSummary struct {
	Value     string `json:"value"` // 板卡 id(传给 MCP 工具)
	Label     string `json:"label"`
	Framework string `json:"framework"`
	Platform  string `json:"platform"`
}

// HardwareBoardList returns every board the registry knows, for the panel's board
// dropdown. No IPC: the desktop links the same boards package the MCP uses.
func (h *hardwareService) BoardList() []HardwareBoardSummary {
	profiles := boards.Profiles()
	out := make([]HardwareBoardSummary, 0, len(profiles))
	for _, b := range profiles {
		out = append(out, HardwareBoardSummary{Value: b.ID, Label: b.Label, Framework: b.DefaultFramework, Platform: b.Platform})
	}
	return out
}

// HardwareDetectView is a desktop-friendly projection of the hardware_detect MCP
// tool output. The drawer can show real local readiness before the user asks the
// agent to build or flash anything.
type HardwareDetectView struct {
	Available         bool                       `json:"available"`
	Workspace         string                     `json:"workspace,omitempty"`
	ProjectDir        string                     `json:"projectDir,omitempty"`
	ProjectTypes      []string                   `json:"projectTypes"`
	CandidateProjects []HardwareProjectCandidate `json:"candidateProjects"`
	SerialPorts       []string                   `json:"serialPorts"`
	Boards            []HardwareBoardView        `json:"boards"`
	Devices           []HardwareDeviceView       `json:"devices"`
	Toolchains        []HardwareToolchainView    `json:"toolchains"`
	Recommendations   []string                   `json:"recommendations"`
	ESPIDFOfficial    map[string]string          `json:"espIdfOfficialMcp,omitempty"`
	Error             string                     `json:"error,omitempty"`
}

type HardwareProjectCandidate struct {
	Dir   string `json:"dir"`
	Kind  string `json:"kind"`
	Entry string `json:"entry,omitempty"`
}

// HardwareInstallStepView 是一键安装里单个工具的安装结果(对应 MCP 的 toolInstallStep)。
type HardwareInstallStepView struct {
	Tool    string `json:"tool"`
	Action  string `json:"action"` // already_present | installed | failed | skipped
	OK      bool   `json:"ok"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// HardwareInstallToolchainView 是「一键安装核心工具链」按钮的结果。
type HardwareInstallToolchainView struct {
	Available  bool                      `json:"available"`
	Steps      []HardwareInstallStepView `json:"steps"`
	AllOK      bool                      `json:"allOK"`
	ManagedDir string                    `json:"managedDir,omitempty"`
	NextStep   string                    `json:"nextStep,omitempty"`
	Error      string                    `json:"error,omitempty"`
}

// HardwareEvidenceStatusView is a compact projection of hardware_evidence_status
// for the drawer. It makes the local/real-hardware verification boundary visible
// without asking the model to summarize it first.
type HardwareEvidenceStatusView struct {
	Available          bool     `json:"available"`
	ProjectDir         string   `json:"projectDir,omitempty"`
	Platform           string   `json:"platform,omitempty"`
	Board              string   `json:"board,omitempty"`
	EvidenceFile       string   `json:"evidenceFile,omitempty"`
	RecordCount        int      `json:"recordCount"`
	CurrentRecordCount int      `json:"currentRecordCount"`
	StaleRecordCount   int      `json:"staleRecordCount"`
	Status             string   `json:"status"`
	Summary            string   `json:"summary"`
	MissingGroups      []string `json:"missingGroups"`
	Recommendations    []string `json:"recommendations"`
	Error              string   `json:"error,omitempty"`
}

type HardwareToolchainView struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type HardwareBoardView struct {
	Port       string `json:"port"`
	Protocol   string `json:"protocol,omitempty"`
	BoardName  string `json:"boardName,omitempty"`
	FQBN       string `json:"fqbn,omitempty"`
	Core       string `json:"core,omitempty"`
	Properties string `json:"properties,omitempty"`
}

type HardwareDeviceView struct {
	Port        string `json:"port"`
	Description string `json:"description,omitempty"`
	HWID        string `json:"hwid,omitempty"`
}

// HardwareMCP reports whether the bundled or locally built hardware MCP server is
// available and whether the current session already has it connected.
func (h *hardwareService) MCP() HardwareMCPView {
	cmd, source, err := resolveHardwareMCP()
	view := HardwareMCPView{Name: "hardware", Command: cmd, Source: source, Available: err == nil}
	if err != nil {
		view.Error = err.Error()
	}
	ctrl := h.ctrl()
	if ctrl == nil {
		return view
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == "hardware" {
			view.Configured = true
			view.Connected = true
			return view
		}
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == "hardware" {
			view.Configured = true
			view.Error = f.Error
			return view
		}
	}
	return view
}

// HardwareDetect runs the bundled MCP's detection tool directly so the hardware
// drawer can display local toolchain and serial-port status even before the
// server is connected to the current agent session.
func (h *hardwareService) Detect() HardwareDetectView {
	view := HardwareDetectView{
		Available:         false,
		ProjectTypes:      []string{},
		CandidateProjects: []HardwareProjectCandidate{},
		SerialPorts:       []string{},
		Boards:            []HardwareBoardView{},
		Devices:           []HardwareDeviceView{},
		Toolchains:        []HardwareToolchainView{},
		Recommendations:   []string{},
	}
	command, _, err := resolveHardwareMCP()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	text, err := callHardwareMCPTool(command, "hardware_detect", map[string]any{"project_dir": h.root()}, 20*time.Second)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		view.Error = "hardware_detect returned invalid JSON: " + err.Error()
		return view
	}
	normalizeHardwareDetectView(&view)
	view.Available = true
	return view
}

// HardwareInstallToolchain 一键安装核心硬件工具链(arduino-cli + 板卡 core)。
// 给学生/老师打包后,缺工具时点一下就能从零把编译/烧录环境备齐。
// 首次会联网下载 core,耗时可能几分钟,所以用长超时;前端按钮自己转圈。
func (h *hardwareService) InstallToolchain(cores []string) HardwareInstallToolchainView {
	view := HardwareInstallToolchainView{Steps: []HardwareInstallStepView{}}
	// 与 HardwareInstallCore 同一把互斥锁:三个安装入口并发写同一个 arduino-cli(.download.tmp
	// → rename)会互相写坏。被占用时拒绝并给一致话术。
	if !h.begin(&h.installing) {
		view.Error = "已有工具链安装正在进行,请等它完成再试。"
		return view
	}
	defer h.end(&h.installing)
	command, _, err := resolveHardwareMCP()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	args := map[string]any{}
	if len(cores) > 0 {
		args["cores"] = cores
	}
	text, err := callHardwareMCPTool(command, "hardware_install_toolchain", args, 20*time.Minute)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		view.Error = "hardware_install_toolchain returned invalid JSON: " + err.Error()
		return view
	}
	if view.Steps == nil {
		view.Steps = []HardwareInstallStepView{}
	}
	view.Available = true
	return view
}

// HardwareInstallArduinoCLI 只装 arduino-cli 本体,返回单步结果。GUI 分步进度用:
// 前端先调这个,再逐个调 HardwareInstallCore,中间刷新进度,用户能看到正在装哪一步。
func (h *hardwareService) InstallArduinoCLI() HardwareInstallStepView {
	// 分步安装第一步实际调用的就是这里,必须与 HardwareInstallCore 共用同一把互斥锁——否则
	// 面板卸载→重挂载丢前端守卫后重复点击,两个下载并发写同一 .download.tmp,rename 出损坏的
	// arduino-cli,之后所有编译烧录静默失败。
	if !h.begin(&h.installing) {
		return HardwareInstallStepView{Action: "failed", Message: "已有工具链安装正在进行,请等它完成再试。"}
	}
	defer h.end(&h.installing)
	return hardwareInstallStep("hardware_install_arduino_cli", nil)
}

// HardwareInstallCore 装单个板卡 core(已装秒跳过),返回单步结果。GUI 分步进度用。
// beginHardwareOp 试图占用一个硬件长操作槽(install 或 flash)。已被占用返回 false,
// 调用方应据此拒绝并提示;成功返回 true,调用方必须 defer endHardwareOp 释放。
func (h *hardwareService) begin(flag *bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if *flag {
		return false
	}
	*flag = true
	return true
}

func (h *hardwareService) end(flag *bool) {
	h.mu.Lock()
	*flag = false
	h.mu.Unlock()
}

func (h *hardwareService) InstallCore(core string) HardwareInstallStepView {
	if !h.begin(&h.installing) {
		return HardwareInstallStepView{Action: "failed", Message: "已有工具链安装正在进行,请等它完成再试。"}
	}
	defer h.end(&h.installing)
	return hardwareInstallStep("hardware_install_core", map[string]any{"core": core})
}

// hardwareInstallStep 调一个返回单步 JSON 的安装工具,解析成 HardwareInstallStepView。
func hardwareInstallStep(tool string, args map[string]any) HardwareInstallStepView {
	step := HardwareInstallStepView{}
	command, _, err := resolveHardwareMCP()
	if err != nil {
		step.Action = "failed"
		step.Message = err.Error()
		return step
	}
	if args == nil {
		args = map[string]any{}
	}
	text, err := callHardwareMCPTool(command, tool, args, 20*time.Minute)
	if err != nil {
		step.Action = "failed"
		step.Message = err.Error()
		return step
	}
	if err := json.Unmarshal([]byte(text), &step); err != nil {
		step.Action = "failed"
		step.Message = tool + " returned invalid JSON: " + err.Error()
	}
	return step
}

// HardwareEvidenceStatus runs hardware_evidence_status directly so the drawer can
// show whether the current project has only local validation or real-device proof.
func (h *hardwareService) EvidenceStatus() HardwareEvidenceStatusView {
	view := HardwareEvidenceStatusView{
		MissingGroups:   []string{},
		Recommendations: []string{},
	}
	command, _, err := resolveHardwareMCP()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	text, err := callHardwareMCPTool(command, "hardware_evidence_status", map[string]any{"project_dir": h.root()}, 20*time.Second)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		view.Error = "hardware_evidence_status returned invalid JSON: " + err.Error()
		return view
	}
	normalizeHardwareEvidenceStatusView(&view)
	view.Available = true
	return view
}

// evidenceRecordMirror 反序列化 tests/hardware_evidence.jsonl 的每一行。
// 真源是 hardware MCP 的 hardware_evidence_record；这里只读出来汇总成可读文本。
type evidenceRecordMirror struct {
	TimestampUTC  string `json:"timestampUtc"`
	Platform      string `json:"platform"`
	Board         string `json:"board"`
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	Summary       string `json:"summary"`
	Command       string `json:"command"`
	Port          string `json:"port"`
	OutputExcerpt string `json:"outputExcerpt"`
}

// HardwareEvidenceExport 把 tests/hardware_evidence.jsonl 里的真机验证记录汇总成
// 一段学生可直接粘进研究日志/论文的 Markdown——目的是让竞赛材料用「真实采集的
// 编译/烧录/串口/部署证据」，而不是凭记忆编数字（这条是项目红线）。
// 返回空字符串表示还没有任何验证记录。
func (h *hardwareService) EvidenceExport(projectDir string) (string, error) {
	dir := h.resolveProjectDir(projectDir)
	path := filepath.Join(dir, "tests", "hardware_evidence.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // 还没有证据文件，不算错误
		}
		return "", err
	}
	records := make([]evidenceRecordMirror, 0, 16)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec evidenceRecordMirror
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Stage != "" {
			records = append(records, rec)
		}
	}
	if len(records) == 0 {
		return "", nil
	}
	return renderEvidenceMarkdown(records), nil
}

// evidenceStageLabel 把英文阶段名翻成学生能懂的中文。
func evidenceStageLabel(stage string) string {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "compile", "build", "validate":
		return "编译/语法"
	case "upload", "flash":
		return "烧录"
	case "monitor", "serial":
		return "串口/运行日志"
	case "ssh", "ssh_deploy", "deploy":
		return "真机部署"
	case "mpremote":
		return "MicroPython 部署"
	default:
		return stage
	}
}

func renderEvidenceMarkdown(records []evidenceRecordMirror) string {
	var b strings.Builder
	b.WriteString("# 真机验证证据（onecreat 自动导出）\n\n")
	b.WriteString("> 本文件由 onecreat 从真实的编译 / 烧录 / 串口 / 部署记录（tests/hardware_evidence.jsonl）自动汇总，")
	b.WriteString("可作为研究日志、论文的原始验证依据。请勿手工编造数据。\n\n")
	fmt.Fprintf(&b, "共 %d 条验证记录。\n", len(records))
	for i, rec := range records {
		fmt.Fprintf(&b, "\n## %d. 【%s】%s\n", i+1, evidenceStageLabel(rec.Stage), strings.TrimSpace(rec.Status))
		if t := strings.TrimSpace(rec.TimestampUTC); t != "" {
			fmt.Fprintf(&b, "- 时间（UTC）：%s\n", t)
		}
		plat := strings.TrimSpace(rec.Platform)
		if board := strings.TrimSpace(rec.Board); board != "" {
			plat = strings.TrimSpace(plat + " / " + board)
		}
		if plat != "" {
			fmt.Fprintf(&b, "- 平台 / 板卡：%s\n", plat)
		}
		if p := strings.TrimSpace(rec.Port); p != "" {
			fmt.Fprintf(&b, "- 端口：%s\n", p)
		}
		if s := strings.TrimSpace(rec.Summary); s != "" {
			fmt.Fprintf(&b, "- 结果：%s\n", s)
		}
		if c := strings.TrimSpace(rec.Command); c != "" {
			fmt.Fprintf(&b, "- 命令：`%s`\n", c)
		}
		if o := strings.TrimSpace(rec.OutputExcerpt); o != "" {
			// 串口输出本身可能含 ```（罕见但会发生），用动态围栏避免提前闭合代码块、
			// 破坏导出文档，同时原样保留输出内容。
			fence := codeFence(o)
			fmt.Fprintf(&b, "- 输出片段：\n\n%s\n%s\n%s\n", fence, o, fence)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeFence 返回一段比 content 里最长连续反引号还长一位的反引号围栏（至少 3 个），
// 保证 Markdown 代码块不会被内容里的 ``` 提前闭合。
func codeFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// HardwareBoardFactsView 是「写代码前」要硬注入 prompt 的板卡事实串。
// Found=false 表示当前板卡没有可注入的确定事实（自定义板/检测到的裸板），
// 前端据此跳过注入、回退到原有「让模型自己调工具」的流程。
type HardwareBoardFactsView struct {
	Found bool   `json:"found"`
	Facts string `json:"facts"`
}

// boardProfileMirror / moduleSpecMirror 只反序列化我们要渲染的字段子集。
// 真源仍是 hardware MCP 的 catalog（platform-api.json / sensor-catalog.json /
// builtInBoardProfiles）——这里不复制数据，只解析它的 JSON 输出。
type boardProfileMirror struct {
	Profile *struct {
		Label                string   `json:"label"`
		ArduinoFQBN          string   `json:"arduinoFqbn"`
		LogicVoltage         string   `json:"logicVoltage"`
		PowerNotes           string   `json:"powerNotes"`
		RecommendedProtocols []string `json:"recommendedProtocols"`
		DefaultPins          []struct {
			Name  string   `json:"name"`
			Pins  []string `json:"pins"`
			Notes string   `json:"notes"`
		} `json:"defaultPins"`
		RiskyPins []struct {
			Name  string   `json:"name"`
			Pins  []string `json:"pins"`
			Notes string   `json:"notes"`
		} `json:"riskyPins"`
		CommonFailures []string `json:"commonFailures"`
		TeachingNotes  []string `json:"teachingNotes"`
	} `json:"profile"`
}

type moduleSpecMirror struct {
	Modules []struct {
		Matched  bool     `json:"matched"`
		Kind     string   `json:"kind"`
		Function string   `json:"function"`
		Imports  []string `json:"imports"`
		Gotchas  []string `json:"gotchas"`
		Snippet  string   `json:"snippet"`
	} `json:"modules"`
}

// HardwareBoardFacts 在「写代码前」确定性地取出已选板卡的校验事实，拼成一段文本
// 供 HardwarePanel 直接注入 prompt——不再依赖弱模型自觉去调 board_profile /
// module_spec（项目实测：flash 裸写国产生态必幻觉，只有把事实压进上下文才对）。
// 事实来自两个已有 MCP 工具（单一真源）：
//   - hardware_board_profile：电平、默认/风险引脚、推荐协议、常见失败、教学提示
//   - hardware_module_spec（板卡名当 module 查）：冷门平台 API 的正确 import、
//     gotchas、最小示例（ESP32 LEDC / 行空板 pinpong / MaixCAM K230 maix.*）——
//     这正是 flash 最容易编错库名和 API 的地方。
//
// boardFactsCache 缓存按板卡查到的事实。catalog 内嵌在 MCP 二进制里、运行期不变,
// 同一板卡反复点「写代码」没必要反复拉起 MCP 子进程(每次两个调用、各 15s 超时,
// MCP 卡顿时按钮会冻很久)。只缓存「两个调用都成功」的结果——MCP 暂时性失败
// 不能被记成永久没有事实。
var (
	boardFactsMu    sync.Mutex
	boardFactsCache = map[string]HardwareBoardFactsView{}
)

func (h *hardwareService) BoardFacts(board, platform string) HardwareBoardFactsView {
	board = strings.TrimSpace(board)
	// 自定义板和「检测到的裸板」没有 catalog 事实，直接跳过
	if board == "" || strings.EqualFold(board, "custom") || strings.HasPrefix(board, "detected:") {
		return HardwareBoardFactsView{}
	}
	if strings.TrimSpace(platform) == "" {
		platform = "auto"
	}
	cacheKey := strings.ToLower(board) + "|" + strings.ToLower(platform)
	boardFactsMu.Lock()
	if v, ok := boardFactsCache[cacheKey]; ok {
		boardFactsMu.Unlock()
		return v
	}
	boardFactsMu.Unlock()

	command, _, err := resolveHardwareMCP()
	if err != nil {
		return HardwareBoardFactsView{}
	}

	// 两个 MCP 调用互不依赖,并行跑:最坏等待从 30s 降到 15s。
	var (
		profileSection, apiSection string
		profileErr, apiErr         error
		wg                         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		// 板卡 profile：电平 / 引脚 / 协议 / 常见失败
		text, e := callHardwareMCPTool(command, "hardware_board_profile",
			map[string]any{"board": board, "platform": platform}, 15*time.Second)
		profileErr = e
		if e != nil {
			return
		}
		var bp boardProfileMirror
		if json.Unmarshal([]byte(text), &bp) == nil && bp.Profile != nil {
			profileSection = renderBoardProfileFacts(bp)
		}
	}()
	go func() {
		defer wg.Done()
		// 平台 API：把板卡名当 module 查，命中 platform_api 就拿到正确 import/坑/示例
		text, e := callHardwareMCPTool(command, "hardware_module_spec",
			map[string]any{"modules": []string{board}, "board": board, "platform": platform}, 15*time.Second)
		apiErr = e
		if e != nil {
			return
		}
		var ms moduleSpecMirror
		if json.Unmarshal([]byte(text), &ms) == nil {
			apiSection = renderPlatformAPIFacts(ms)
		}
	}()
	wg.Wait()

	sections := make([]string, 0, 2)
	if profileSection != "" {
		sections = append(sections, profileSection)
	}
	if apiSection != "" {
		sections = append(sections, apiSection)
	}
	view := HardwareBoardFactsView{}
	if len(sections) > 0 {
		view = HardwareBoardFactsView{Found: true, Facts: strings.Join(sections, "\n")}
	}
	// 两个调用都成功才缓存(含「确实没有该板事实」的合法空结果)
	if profileErr == nil && apiErr == nil {
		boardFactsMu.Lock()
		boardFactsCache[cacheKey] = view
		boardFactsMu.Unlock()
	}
	return view
}

// renderBoardProfileFacts 把板卡 profile 渲染成紧凑、学生可读的中文事实块。
func renderBoardProfileFacts(bp boardProfileMirror) string {
	p := bp.Profile
	var b strings.Builder
	title := p.Label
	if title == "" {
		title = "目标板卡"
	}
	if p.ArduinoFQBN != "" {
		fmt.Fprintf(&b, "板卡：%s（FQBN %s）\n", title, p.ArduinoFQBN)
	} else {
		fmt.Fprintf(&b, "板卡：%s\n", title)
	}
	if p.LogicVoltage != "" {
		line := "逻辑电平：" + p.LogicVoltage
		if p.PowerNotes != "" {
			line += "。" + p.PowerNotes
		}
		b.WriteString(line + "\n")
	}
	if len(p.RecommendedProtocols) > 0 {
		fmt.Fprintf(&b, "推荐通信：%s\n", strings.Join(p.RecommendedProtocols, "、"))
	}
	for _, pin := range p.DefaultPins {
		if len(pin.Pins) == 0 {
			continue
		}
		seg := "默认引脚 " + pin.Name + "：" + strings.Join(pin.Pins, "/")
		if pin.Notes != "" {
			seg += "（" + pin.Notes + "）"
		}
		b.WriteString(seg + "\n")
	}
	for _, pin := range p.RiskyPins {
		seg := "⚠️ 风险引脚 " + pin.Name
		if len(pin.Pins) > 0 {
			seg += " " + strings.Join(pin.Pins, "/")
		}
		if pin.Notes != "" {
			seg += "：" + pin.Notes
		}
		b.WriteString(seg + "\n")
	}
	if len(p.CommonFailures) > 0 {
		fmt.Fprintf(&b, "常见失败：%s\n", strings.Join(p.CommonFailures, "；"))
	}
	if len(p.TeachingNotes) > 0 {
		fmt.Fprintf(&b, "教学提示：%s\n", strings.Join(p.TeachingNotes, "；"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPlatformAPIFacts 从 module_spec 结果里挑出 platform_api（冷门平台软件写法），
// 渲染成「正确 import + 坑 + 最小示例」——这是防 flash 幻觉最关键的一段。
func renderPlatformAPIFacts(ms moduleSpecMirror) string {
	for _, m := range ms.Modules {
		if !m.Matched || m.Kind != "platform_api" {
			continue
		}
		var b strings.Builder
		name := m.Function
		if name == "" {
			name = "平台 API"
		}
		fmt.Fprintf(&b, "平台 API（%s）——务必照此写，别凭记忆猜库名/方法：\n", name)
		if len(m.Imports) > 0 {
			fmt.Fprintf(&b, "正确 import：%s\n", strings.Join(m.Imports, "；"))
		}
		if len(m.Gotchas) > 0 {
			fmt.Fprintf(&b, "注意：%s\n", strings.Join(m.Gotchas, "；"))
		}
		if strings.TrimSpace(m.Snippet) != "" {
			fmt.Fprintf(&b, "最小示例：\n%s\n", strings.TrimSpace(m.Snippet))
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return ""
}

// HardwareRunInput is the shared input for the one-click compile/upload/monitor
// buttons in HardwarePanel. Fields are optional unless required by the underlying
// MCP tool — the dispatch picks the right tool by Platform.
type HardwareRunInput struct {
	ProjectDir  string `json:"projectDir"`
	Platform    string `json:"platform"`
	Board       string `json:"board,omitempty"`
	Port        string `json:"port,omitempty"`
	Seconds     int    `json:"seconds,omitempty"`
	Baud        int    `json:"baud,omitempty"`        // 看串口的波特率;0 = 默认 115200
	Address     string `json:"address,omitempty"`     // OTA WiFi 烧录:板子地址(IP 或 mDNS 名)
	OTAPassword string `json:"otaPassword,omitempty"` // OTA WiFi 烧录:ArduinoOTA 口令
}

// HardwarePublishInput 是「发布固件到远程服务器」(③ 云端拉取)的入参。
// 服务器配置(ssh/目录/URL)留空时跳过发布(不再有内置 NAS 默认值,去内网化后各人填自己
// 的 NAS/VPS,见 HardwarePublishFirmware)。
type HardwarePublishInput struct {
	ProjectDir  string `json:"projectDir"`
	Board       string `json:"board,omitempty"`
	ProjectName string `json:"projectName"`
	Version     string `json:"version"`
	SSHHost     string `json:"sshHost,omitempty"`
	RemoteDir   string `json:"remoteDir,omitempty"`
	BaseURL     string `json:"baseURL,omitempty"`
}

// HardwareRunResult is the normalized result the frontend renders into the
// one-click action UI. RootCause + FixHint come from hardware_project_validate's
// error-distillation; they are empty on success or when an underlying tool doesn't
// distill (upload/monitor). Output is the truncated command output for the drawer.
type HardwareRunResult struct {
	Status    string `json:"status"`
	Kind      string `json:"kind,omitempty"` // 验证子类(如 python_syntax)，前端据此区分「真编译」与「仅语法检查」
	Summary   string `json:"summary"`
	Output    string `json:"output,omitempty"`
	RootCause string `json:"rootCause,omitempty"`
	FixHint   string `json:"fixHint,omitempty"`
	NextStep  string `json:"nextStep,omitempty"`
	Error     string `json:"error,omitempty"`
	Command   string `json:"command,omitempty"`
}

// HardwareValidate runs hardware_project_validate. Returns the first failed
// validationResult, otherwise the last one. The frontend's "编译" button calls it.
func (h *hardwareService) Validate(input HardwareRunInput) HardwareRunResult {
	command, err := h.requireMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	args := map[string]any{
		"project_dir":     h.resolveProjectDir(input.ProjectDir),
		"timeout_seconds": 180,
	}
	if input.Platform != "" {
		args["platform"] = input.Platform
	}
	if input.Board != "" {
		args["board"] = input.Board
	}
	text, err := callHardwareMCPTool(command, "hardware_project_validate", args, 200*time.Second)
	if err != nil {
		return HardwareRunResult{Status: "failed", Summary: "编译/验证调用失败", Error: err.Error()}
	}
	var report struct {
		Summary         string             `json:"summary"`
		Results         []validationResult `json:"results"`
		Recommendations []string           `json:"recommendations"`
	}
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		return HardwareRunResult{Status: "failed", Summary: "无法解析验证结果", Error: err.Error()}
	}
	pick := pickValidationResult(report.Results)
	res := HardwareRunResult{
		Status:    coalesceStatus(pick.Status, "skipped"),
		Kind:      pick.Kind,
		Summary:   strings.TrimSpace(report.Summary),
		Output:    truncateOutput(pick.Output, 4096),
		RootCause: pick.RootCause,
		FixHint:   pick.FixHint,
		NextStep:  pick.NextStep,
		Error:     pick.Error,
		Command:   pick.Command,
	}
	// py_compile 只是语法检查，不是真编译(不解析 import/API)。别让「N passed」
	// 的绿勾被学生当成「已验证」——把摘要降级成诚实表述。
	if res.Kind == "python_syntax" && res.Status == "passed" {
		res.Summary = "仅语法检查通过（py_compile），API / 真机未验证"
	}
	if res.Summary == "" {
		res.Summary = res.Status
	}
	return res
}

// HardwareUpload dispatches to the platform-appropriate upload MCP tool.
// Arduino/PlatformIO/ESP-IDF/MicroPython are wired here; SSH-deployed Python
// platforms (Unihiker / MaixCAM / RPi) fall back to "use chat" since they need
// host + remote_path the student hasn't entered yet.
func (h *hardwareService) Upload(input HardwareRunInput) HardwareRunResult {
	if !h.begin(&h.flashing) {
		return HardwareRunResult{Status: "skipped", Summary: "烧录进行中", NextStep: "已有一次烧录正在进行,请等它完成再试。"}
	}
	defer h.end(&h.flashing)
	command, err := h.requireMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	// 烧录前先关掉串口监视器:它占着串口,不关上传会被它卡住(串口同一时刻只能一个进程用)。
	h.serial.Close()
	projectDir := h.resolveProjectDir(input.ProjectDir)
	switch input.Platform {
	case "arduino":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "在硬件面板选择上传端口后再点烧录。"}
		}
		args := map[string]any{
			"sketch_dir":      projectDir,
			"port":            input.Port,
			"timeout_seconds": 120,
		}
		// FQBN 与「编译」同源(manifest.board 优先于 UI 下拉),避免编译与烧录目标芯片不一致。
		if fqbn := resolveFlashFQBN(projectDir, input.Board); fqbn != "" {
			args["fqbn"] = fqbn
		}
		return runHardwareSimple(command, "arduino_upload", args, 150*time.Second, "Arduino 烧录")
	case "platformio":
		args := map[string]any{
			"project_dir":     projectDir,
			"targets":         []string{"upload"},
			"timeout_seconds": 180,
		}
		if input.Port != "" {
			args["upload_port"] = input.Port
		}
		return runHardwareSimple(command, "platformio_run", args, 220*time.Second, "PlatformIO 烧录")
	case "esp_idf":
		args := map[string]any{
			"project_dir":     projectDir,
			"action":          "flash",
			"timeout_seconds": 180,
		}
		if input.Port != "" {
			args["port"] = input.Port
		}
		return runHardwareSimple(command, "esp_idf_run", args, 220*time.Second, "ESP-IDF 烧录")
	case "micropython":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "选择 MicroPython 设备端口后再点烧录。"}
		}
		args := map[string]any{
			"port":            input.Port,
			"project_dir":     projectDir,
			"timeout_seconds": 60,
		}
		return runHardwareSimple(command, "mpremote_run", args, 80*time.Second, "MicroPython 部署")
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return HardwareRunResult{
			Status:   "skipped",
			Summary:  "该平台需要 SSH 部署,先在对话里完成",
			NextStep: "Unihiker / MaixCAM / 树莓派项目用 SSH 烧录:在对话框输入 ssh 主机和路径,让 AI 调用 ssh_deploy_run。",
		}
	default:
		return HardwareRunResult{Status: "failed", Summary: "未知平台", Error: "unsupported platform: " + input.Platform}
	}
}

// HardwareOTAUpload 通过 WiFi(OTA)把固件烧给已经跑着 ArduinoOTA 的板子,不用 USB。
// 走 arduino-cli 网络口(espota);面向 arduino/esp32 工程。
func (h *hardwareService) OTAUpload(input HardwareRunInput) HardwareRunResult {
	if !h.begin(&h.flashing) {
		return HardwareRunResult{Status: "skipped", Summary: "烧录进行中", NextStep: "已有一次烧录正在进行,请等它完成再试。"}
	}
	defer h.end(&h.flashing)
	command, err := h.requireMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	if strings.TrimSpace(input.Address) == "" {
		return HardwareRunResult{Status: "skipped", Summary: "缺少板子地址", NextStep: "填入板子的 WiFi 地址(IP 或 esp32-onecreat.local)后再点 WiFi 烧录。"}
	}
	projectDir := h.resolveProjectDir(input.ProjectDir)
	fqbn := resolveFlashFQBN(projectDir, input.Board) // manifest.board 优先,与「编译」一致
	if fqbn == "" {
		fqbn = "esp32:esp32:esp32" // OTA 以 ESP32 为主,板卡缺省时兜底
	}
	args := map[string]any{
		"sketch_dir":      projectDir,
		"fqbn":            fqbn,
		"address":         strings.TrimSpace(input.Address),
		"timeout_seconds": 180,
	}
	if pwd := strings.TrimSpace(input.OTAPassword); pwd != "" {
		args["password"] = pwd
	}
	return runHardwareSimple(command, "arduino_ota_upload", args, 200*time.Second, "WiFi 烧录(OTA)")
}

// HardwarePublishFirmware 把固件发布到远程固件服务器(③ 云端拉取),板子自己来拉。
// 服务器配置(SSH 目标 / 远程目录 / 公网 URL)由前端面板提供,各人填自己的 NAS/VPS。
func (h *hardwareService) PublishFirmware(input HardwarePublishInput) HardwareRunResult {
	// 用 hwFlashing 槽(而非新增槽):发布同样要先编译 sketch(sketch_dir+fqbn),与烧录共享
	// 本地 arduino-cli 构建产物;两个"编译+部署"并发会互相写坏本地构建目录,也会让两次发布
	// 交错写远端 tmp。同槽把 USB 烧录/OTA 烧录/远程发布一起串行,是最贴合的安全边界。
	if !h.begin(&h.flashing) {
		return HardwareRunResult{Status: "skipped", Summary: "有硬件操作进行中", NextStep: "已有一次烧录或发布正在进行,请等它完成再试。"}
	}
	defer h.end(&h.flashing)
	command, err := h.requireMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	if strings.TrimSpace(input.ProjectName) == "" || strings.TrimSpace(input.Version) == "" {
		return HardwareRunResult{Status: "skipped", Summary: "缺少项目名或版本号", NextStep: "填项目名和版本号(如 1.0.2)后再发布。"}
	}
	// 用与脚手架相同的 sanitizeProjectName:脚手架把 sanitize 后的项目名烤进板子的
	// FW_VERSION_URL(baseURL/<sanitized>/version.txt),这里发布也必须落到同一路径,
	// 否则中文/含空格/首字符是数字的项目名会被脚手架改写、而发布用原名建目录 → 板子
	// 永远轮询到 404,升级静默失败。
	projectName := sanitizeProjectName(input.ProjectName)
	if projectName == "" {
		return HardwareRunResult{Status: "skipped", Summary: "项目名不合法", NextStep: "项目名请用英文字母/数字/下划线(要和新建 OTA 项目时填的一致)。"}
	}
	sshHost := strings.TrimSpace(input.SSHHost)
	remoteDir := strings.TrimSpace(input.RemoteDir)
	baseURL := strings.TrimSpace(input.BaseURL)
	if sshHost == "" || remoteDir == "" || baseURL == "" {
		return HardwareRunResult{Status: "skipped", Summary: "未配置固件服务器", NextStep: "在「发布固件」里填好 服务器URL / SSH目标 / 远程目录(填你自己的 NAS 或 VPS)后再发布。"}
	}
	projectDir := h.resolveProjectDir(input.ProjectDir)
	fqbn := resolveFlashFQBN(projectDir, input.Board) // manifest.board 优先,与「编译」一致
	if fqbn == "" {
		fqbn = "esp32:esp32:esp32"
	}
	args := map[string]any{
		"project_name":    projectName,
		"version":         strings.TrimSpace(input.Version),
		"ssh_host":        sshHost,
		"remote_dir":      remoteDir,
		"base_url":        baseURL,
		"sketch_dir":      projectDir,
		"fqbn":            fqbn,
		"timeout_seconds": 300,
	}
	return runHardwareSimple(command, "firmware_publish", args, 320*time.Second, "发布固件(远程)")
}

func firstNonEmptyStr(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// HardwareMonitor dispatches to the platform-appropriate serial-monitor MCP tool
// for a short sampling window. The frontend's "看串口" button calls it.
func (h *hardwareService) Monitor(input HardwareRunInput) HardwareRunResult {
	// 与烧录争同一串口:占 hwFlashing 槽。烧录进行中拒绝采样——否则采样子进程会撞正在写 flash 的
	// esptool,轻则 busy 失败、重则打断写入把板子写成半砖。后端兜底(前端按钮守卫在面板重挂载后会丢,
	// 正是 D1 互斥要防的场景);此前 HardwareMonitor 漏了这道槽。
	if !h.begin(&h.flashing) {
		return HardwareRunResult{Status: "skipped", Summary: "串口忙", NextStep: "有烧录正在进行、占用着串口,请等它完成再看串口。"}
	}
	defer h.end(&h.flashing)
	command, err := h.requireMCP()
	if err != nil {
		return HardwareRunResult{Status: "failed", Error: err.Error()}
	}
	// 采样前先关掉常驻串口监视器:它占着串口,不关 MCP 采样子进程会撞占用失败(与 HardwareUpload 同款)。
	h.serial.Close()
	seconds := input.Seconds
	if seconds <= 0 {
		seconds = 8
	}
	if seconds > 30 {
		seconds = 30 // 别让前端按钮一按等半分钟
	}
	// 波特率跟随面板选择(曾写死 115200:9600 的板子点「看串口」永远采不到,
	// 但常驻串口监视器又能看到——两条链路矛盾,学生难自查)。
	baud := input.Baud
	if baud <= 0 {
		baud = 115200
	}
	switch input.Platform {
	case "arduino":
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "选择串口后再点查看串口。"}
		}
		args := map[string]any{
			"port":            input.Port,
			"seconds":         seconds,
			"baud":            baud,
			"timeout_seconds": seconds + 5,
		}
		return runHardwareSimple(command, "arduino_monitor_sample", args, time.Duration(seconds+10)*time.Second, "串口采样")
	case "platformio":
		// PlatformIO 的 `pio run -t monitor` 是交互式终端,onecreat 用程序方式(非 TTY)
		// spawn 它会在 termios.tcgetattr 崩溃(Operation not supported by device)。
		// 「看串口」本质是按波特率读串口,与构建系统无关 —— 直接复用已修好的
		// arduino_monitor_sample 端口级读取(arduino-cli monitor),稳。
		if input.Port == "" {
			return HardwareRunResult{Status: "skipped", Summary: "缺少串口", NextStep: "在上方「串口」里选择开发板端口后再点查看串口。"}
		}
		args := map[string]any{
			"port":            input.Port,
			"seconds":         seconds,
			"baud":            baud,
			"timeout_seconds": seconds + 5,
		}
		return runHardwareSimple(command, "arduino_monitor_sample", args, time.Duration(seconds+10)*time.Second, "串口采样")
	case "esp_idf":
		args := map[string]any{
			"project_dir":     h.resolveProjectDir(input.ProjectDir),
			"action":          "monitor",
			"timeout_seconds": seconds + 5,
		}
		if input.Port != "" {
			args["port"] = input.Port
		}
		if input.Baud > 0 {
			args["baud"] = input.Baud
		}
		return runHardwareSimple(command, "esp_idf_run", args, time.Duration(seconds+10)*time.Second, "ESP-IDF 串口")
	default:
		return HardwareRunResult{
			Status:   "skipped",
			Summary:  "该平台暂无一键串口",
			NextStep: "Python/MicroPython/Unihiker/MaixCAM 项目请在对话框里让 AI 执行 SSH 或 mpremote 调试。",
		}
	}
}

func (h *hardwareService) requireMCP() (string, error) {
	command, _, err := resolveHardwareMCP()
	if err != nil {
		return "", err
	}
	return command, nil
}

// resolveHardwareProjectDir defaults an unset hardware project directory to the
// open project's root.
func (h *hardwareService) resolveProjectDir(dir string) string {
	if d := strings.TrimSpace(dir); d != "" {
		return d
	}
	return h.root()
}

// validationResult mirrors cmd/reasonix-hardware-mcp's structure for unmarshalling.
type validationResult struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Status    string `json:"status"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	RootCause string `json:"rootCause,omitempty"`
	FixHint   string `json:"fixHint,omitempty"`
	NextStep  string `json:"nextStep,omitempty"`
}

// pickValidationResult picks the first failure (so the UI surfaces a problem),
// or the last result when everything passed/skipped (so we still show something).
func pickValidationResult(results []validationResult) validationResult {
	if len(results) == 0 {
		return validationResult{Status: "skipped"}
	}
	for _, r := range results {
		if r.Status == "failed" {
			return r
		}
	}
	return results[len(results)-1]
}

func coalesceStatus(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncateOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n…(已截断)"
}

// runHardwareSimple wraps "call MCP tool + return result" for upload/monitor where
// the underlying tool doesn't return structured validation results — we treat the
// raw text as the output and let the model help if it fails.
func runHardwareSimple(command, tool string, args map[string]any, timeout time.Duration, label string) HardwareRunResult {
	text, err := callHardwareMCPTool(command, tool, args, timeout)
	if err != nil {
		return HardwareRunResult{
			Status:  "failed",
			Summary: label + "失败",
			Error:   err.Error(),
			Output:  truncateOutput(text, 4096),
		}
	}
	return HardwareRunResult{
		Status:  "passed",
		Summary: label + "完成",
		Output:  truncateOutput(text, 4096),
	}
}

// arduinoFQBNFromBoard maps short board ids the frontend uses to arduino-cli FQBNs.
// 委托给共享数据驱动注册表(internal/hardware/boards),与 MCP 的 arduinoFQBN 同源,
// 彻底消除「编译走一处映射、烧录走另一处」的孪生漂移——以前要手工对齐两份 switch。
func arduinoFQBNFromBoard(board string) string {
	return boards.ArduinoFQBN(board)
}

// manifestBoard 读取项目 hardware_manifest.json 的 board 字段(读不到/无该字段返回空)。
func manifestBoard(projectDir string) string {
	if strings.TrimSpace(projectDir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(projectDir, "hardware_manifest.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Board string `json:"board"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Board)
}

// resolveFlashFQBN 解析「烧录/发布」用的 FQBN,优先级与「编译」(validateArduinoProject)
// 一致:项目 manifest.board > UI 下拉 board。这样同一项目编译和烧录目标芯片必然相同——
// 否则 manifest 定 esp32s3、UI 停在默认板时,编译按 S3、烧录按 UI,烧的是错芯片的固件。
// 两者都为空时返回空,调用方保持各自兜底。
func resolveFlashFQBN(projectDir, uiBoard string) string {
	board := strings.TrimSpace(uiBoard)
	if mb := manifestBoard(projectDir); mb != "" {
		board = mb
	}
	if board == "" {
		return ""
	}
	return arduinoFQBNFromBoard(board)
}

type hardwareMCPRPCResponse struct {
	Error  *hardwareMCPRPCError `json:"error,omitempty"`
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

type hardwareMCPRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func callHardwareMCPTool(command, name string, args map[string]any, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	if err := json.NewEncoder(stdin).Encode(req); err != nil {
		_ = cmd.Process.Kill()
		return "", err
	}
	_ = stdin.Close()
	err = cmd.Wait()
	// 这些错误会原样显示在硬件面板的红色提示条上,读者是老师/学生——
	// 中文说清楚"怎么了、下一步做什么",开发者细节放在句尾括号里。
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("硬件操作超时(%s 内未完成)。编译和首次下载工具可能因网络慢而超时——检查网络后点重试;若是烧录超时,拔插一次 USB 线再试", timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("硬件助手执行出错,请重试一次;若反复出现,把括号里的信息发给老师/技术支持(%s)", msg)
	}
	line := strings.TrimSpace(firstOutputLine(stdout.String()))
	if line == "" {
		return "", fmt.Errorf("硬件助手没有返回结果,请重试一次;若反复出现请重启应用")
	}
	var resp hardwareMCPRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return "", fmt.Errorf("硬件助手返回了无法解析的内容,请重试一次;若反复出现请重启应用(JSON-RPC 解析失败: %v)", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("硬件助手内部错误,请重试一次(RPC %d: %s)", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result.IsError {
		return "", fmt.Errorf("%s", firstTextContent(resp.Result.Content))
	}
	text := firstTextContent(resp.Result.Content)
	if text == "" {
		return "", fmt.Errorf("硬件助手返回了空结果,请重试一次;若反复出现请重启应用")
	}
	return text, nil
}

func firstOutputLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func firstTextContent(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	for _, item := range content {
		if item.Type == "text" {
			return item.Text
		}
	}
	return ""
}

func normalizeHardwareDetectView(view *HardwareDetectView) {
	if view.ProjectTypes == nil {
		view.ProjectTypes = []string{}
	}
	if view.CandidateProjects == nil {
		view.CandidateProjects = []HardwareProjectCandidate{}
	}
	if view.SerialPorts == nil {
		view.SerialPorts = []string{}
	}
	if view.Boards == nil {
		view.Boards = []HardwareBoardView{}
	}
	if view.Devices == nil {
		view.Devices = []HardwareDeviceView{}
	}
	if view.Toolchains == nil {
		view.Toolchains = []HardwareToolchainView{}
	}
	if view.Recommendations == nil {
		view.Recommendations = []string{}
	}
}

func normalizeHardwareEvidenceStatusView(view *HardwareEvidenceStatusView) {
	if view.MissingGroups == nil {
		view.MissingGroups = []string{}
	}
	if view.Recommendations == nil {
		view.Recommendations = []string{}
	}
}

func resolveHardwareMCP() (command, source string, err error) {
	// 新名优先,回退旧名:老安装的 .app 内 / PATH 上可能仍是 reasonix-hardware-mcp(读旧)。
	bins := []string{"onecreat-hardware-mcp", "reasonix-hardware-mcp"}
	if override := strings.TrimSpace(os.Getenv("REASONIX_HARDWARE_MCP")); override != "" {
		if executable(override) {
			return override, "REASONIX_HARDWARE_MCP", nil
		}
		return "", "REASONIX_HARDWARE_MCP", fmt.Errorf("REASONIX_HARDWARE_MCP points to a missing or non-executable file: %s", override)
	}
	if exe, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exe)
		// Dev 模式优先回溯到 repo 根的 bin/(make build 的产物)。wails dev 的 bundle 名随
		// outputfilename(onecreat-desktop;旧版 reasonix-desktop),production 是 onecreat.app
		// 走下面 exe-based 路径,这段不命中。
		if strings.Contains(exeDir, "onecreat-desktop.app") || strings.Contains(exeDir, "reasonix-desktop.app") {
			for _, bin := range bins {
				// .../desktop/build/bin/<name>.app/Contents/MacOS → 回溯 6 层到 repo 根
				devCandidate := filepath.Join(exeDir, "..", "..", "..", "..", "..", "..", "bin", bin)
				if executable(devCandidate) {
					return filepath.Clean(devCandidate), "dev bin", nil
				}
			}
		}
		for _, bin := range bins {
			for _, candidate := range []string{
				filepath.Join(exeDir, bin),
				filepath.Join(exeDir, bin+".exe"),
				filepath.Join(exeDir, "..", "Resources", bin),
				filepath.Join(exeDir, "..", "Resources", bin+".exe"),
			} {
				if executable(candidate) {
					return filepath.Clean(candidate), "app bundle", nil
				}
			}
		}
	}
	for _, bin := range bins {
		if p, e := exec.LookPath(bin); e == nil {
			return p, "PATH", nil
		}
	}
	if cwd, e := os.Getwd(); e == nil {
		for _, bin := range bins {
			for _, candidate := range []string{
				filepath.Join(cwd, "bin", bin),
				filepath.Join(cwd, "..", "bin", bin),
				filepath.Join(cwd, "..", "..", "bin", bin),
			} {
				if executable(candidate) {
					return filepath.Clean(candidate), "workspace bin", nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("硬件助手未就绪:找不到 reasonix-hardware-mcp 程序。请重启应用;若仍出现,重新安装 OneCreat(开发环境则运行 make build,或设置 REASONIX_HARDWARE_MCP 指向该程序)")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if goruntime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
