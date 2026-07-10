package main

// 硬件模块「地面真值」目录:把已校验的库/引脚/协议/坑喂给模型,
// 避免弱模型凭记忆猜出错的库名、引脚和 API。数据在 data/ 下随二进制 embed。
// sensor-catalog.json 覆盖元器件接线;platform-api.json 补冷门平台的软件 API。

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"reasonix/internal/hardware/boards"
)

//go:embed data/sensor-catalog.json
var sensorCatalogRaw []byte

//go:embed data/platform-api.json
var platformAPIRaw []byte

//go:embed data/commands
var bundledCommandsFS embed.FS

// --- 数据结构(对应 JSON)---

type moduleLibraries struct {
	Arduino    []string `json:"arduino"`
	PlatformIO []string `json:"platformio"`
}

type moduleEntry struct {
	Category       string                    `json:"category"`
	Function       string                    `json:"function"`
	Protocol       string                    `json:"protocol"`
	PinsRequired   map[string]any            `json:"pins_required"`
	DefaultPins    map[string]map[string]any `json:"default_pins"`
	Libraries      moduleLibraries           `json:"libraries"`
	Voltage        string                    `json:"voltage"`
	Range          string                    `json:"range,omitempty"`
	Resolution     string                    `json:"resolution,omitempty"`
	Precision      string                    `json:"precision,omitempty"`
	Notes          string                    `json:"notes"`
	Difficulty     string                    `json:"difficulty,omitempty"`
	UsedInProjects []string                  `json:"used_in_projects,omitempty"`
}

type sensorCatalogFile struct {
	Meta                 map[string]any         `json:"_meta"`
	Sensors              map[string]moduleEntry `json:"sensors"`
	CommunicationModules map[string]moduleEntry `json:"communication_modules"`
	Displays             map[string]moduleEntry `json:"displays"`
	Actuators            map[string]moduleEntry `json:"actuators"`
	VisionModules        map[string]moduleEntry `json:"vision_modules"`
	PowerModules         map[string]moduleEntry `json:"power_modules"`
}

type catalogModule struct {
	Name     string
	Category string
	Entry    moduleEntry
}

type platformAPIEntry struct {
	Label    string   `json:"label"`
	Aliases  []string `json:"aliases"`
	Language string   `json:"language"`
	Verified string   `json:"verified,omitempty"`
	Summary  string   `json:"summary"`
	Imports  []string `json:"imports"`
	Gotchas  []string `json:"gotchas"`
	Snippet  string   `json:"snippet"`
}

type platformAPIFile struct {
	Meta map[string]any              `json:"_meta"`
	APIs map[string]platformAPIEntry `json:"apis"`
}

// --- 加载(init 时解析 embed 的 JSON)---

var (
	catalogModules   []catalogModule
	catalogIndex     = map[string]int{} // 归一化模块名 -> catalogModules 下标
	platformAPIs     map[string]platformAPIEntry
	platformAPIAlias = map[string]string{} // 归一化别名 -> api key
	catalogLoadErr   error
)

// 常见口语名 -> 目录里的标准键(只为确证存在的模块写别名)
var moduleAlias = map[string]string{
	"servo":   "SG90",
	"舵机":      "SG90",
	"oled":    "OLED_SSD1306",
	"ssd1306": "OLED_SSD1306",
	"dht":     "DHT11",
	"温湿度":     "DHT11",
}

// 库名 -> Arduino 头文件(覆盖最常用的几个,够模型起步)
var libIncludes = map[string]string{
	"dht sensor library":      "<DHT.h>",
	"adafruit ssd1306":        "<Adafruit_SSD1306.h>",
	"adafruit gfx":            "<Adafruit_GFX.h>",
	"adafruit gfx library":    "<Adafruit_GFX.h>",
	"adafruit unified sensor": "<Adafruit_Sensor.h>",
	"esp32servo":              "<ESP32Servo.h>",
	"servo":                   "<Servo.h>",
	"wire":                    "<Wire.h>",
}

var i2cAddrRe = regexp.MustCompile(`0[xX][0-9A-Fa-f]{2}`)

func init() { catalogLoadErr = loadCatalog() }

func loadCatalog() error {
	var sc sensorCatalogFile
	if err := json.Unmarshal(sensorCatalogRaw, &sc); err != nil {
		return fmt.Errorf("parse sensor-catalog.json: %w", err)
	}
	addCat := func(m map[string]moduleEntry, category string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 稳定顺序,便于匹配/输出可复现
		for _, k := range keys {
			catalogIndex[normalizeModuleName(k)] = len(catalogModules)
			catalogModules = append(catalogModules, catalogModule{Name: k, Category: category, Entry: m[k]})
		}
	}
	addCat(sc.Sensors, "sensors")
	addCat(sc.CommunicationModules, "communication_modules")
	addCat(sc.Displays, "displays")
	addCat(sc.Actuators, "actuators")
	addCat(sc.VisionModules, "vision_modules")
	addCat(sc.PowerModules, "power_modules")

	var pa platformAPIFile
	if err := json.Unmarshal(platformAPIRaw, &pa); err != nil {
		return fmt.Errorf("parse platform-api.json: %w", err)
	}
	platformAPIs = pa.APIs
	for key, e := range pa.APIs {
		platformAPIAlias[normalizeModuleName(key)] = key
		for _, a := range e.Aliases {
			platformAPIAlias[normalizeModuleName(a)] = key
		}
	}
	return nil
}

// --- 匹配 ---

func normalizeModuleName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(" ", "", "_", "", "-", "", ".", "").Replace(s)
}

func findCatalogModule(query string) (catalogModule, bool) {
	nq := normalizeModuleName(query)
	if nq == "" {
		return catalogModule{}, false
	}
	if canon, ok := moduleAlias[nq]; ok {
		if i, ok := catalogIndex[normalizeModuleName(canon)]; ok {
			return catalogModules[i], true
		}
	}
	if i, ok := catalogIndex[nq]; ok {
		return catalogModules[i], true
	}
	// 归一化名互为子串(SSD1306 ⊂ OLED_SSD1306)
	for _, m := range catalogModules {
		nk := normalizeModuleName(m.Name)
		if strings.Contains(nk, nq) || strings.Contains(nq, nk) {
			return m, true
		}
	}
	// 退而求其次:匹配 function 字段(中文功能名)
	for _, m := range catalogModules {
		if m.Entry.Function != "" && strings.Contains(normalizeModuleName(m.Entry.Function), nq) {
			return m, true
		}
	}
	return catalogModule{}, false
}

// board 字符串 -> 目录里的板键(default_pins 的 key)
// catalogBoardKey 委托给共享注册表;它在子串兜底上比旧版多了 ESP32-C3/C6/H2/S2 → ""
// (不再注入经典 ESP32 的错引脚)。
func catalogBoardKey(board string) string {
	return boards.CatalogPinKey(board)
}

// 按平台选 arduino / platformio 库,再按板裁掉不适用的(ESP32 vs AVR 舵机库)
func filterLibraries(entry moduleEntry, platform, board string) []string {
	var libs []string
	if normalizeModuleName(platform) == "platformio" {
		libs = append(libs, entry.Libraries.PlatformIO...)
	} else {
		libs = append(libs, entry.Libraries.Arduino...)
	}
	nb := normalizeModuleName(board)
	isESP32 := strings.Contains(nb, "esp32")
	isAVR := strings.Contains(nb, "uno") || strings.Contains(nb, "nano") || strings.Contains(nb, "avr") || strings.Contains(nb, "mega")
	if !isESP32 && !isAVR {
		return libs
	}
	hasESP32Lib := false
	for _, l := range libs {
		if strings.Contains(strings.ToLower(l), "esp32") {
			hasESP32Lib = true
		}
	}
	out := make([]string, 0, len(libs))
	for _, l := range libs {
		low := strings.ToLower(l)
		if isAVR && strings.Contains(low, "esp32") {
			continue // AVR 上去掉 ESP32 专用库
		}
		if isESP32 && hasESP32Lib && normalizeLibName(l) == "servo" {
			continue // ESP32 上优先 ESP32Servo,去掉裸 Servo
		}
		out = append(out, l)
	}
	return out
}

func normalizeLibName(l string) string {
	l = strings.ToLower(l)
	if i := strings.Index(l, "@"); i >= 0 {
		l = l[:i] // 去掉 platformio 的 @版本
	}
	if i := strings.LastIndex(l, "/"); i >= 0 {
		l = l[i+1:] // 去掉 vendor/ 前缀
	}
	return strings.TrimSpace(l)
}

func includesForLibraries(libs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range libs {
		if h, ok := libIncludes[normalizeLibName(l)]; ok && !seen[h] {
			out = append(out, h)
			seen[h] = true
		}
	}
	return out
}

func extractI2CAddress(entry moduleEntry) string {
	if !strings.Contains(strings.ToUpper(entry.Protocol), "I2C") {
		return ""
	}
	return i2cAddrRe.FindString(entry.Notes)
}

func splitGotchas(notes string) []string {
	if strings.TrimSpace(notes) == "" {
		return nil
	}
	parts := strings.FieldsFunc(notes, func(r rune) bool { return r == ';' || r == '；' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- 解析单个模块 ---

type moduleSpecResult struct {
	Query       string         `json:"query"`
	Matched     bool           `json:"matched"`
	Kind        string         `json:"kind,omitempty"` // module | platform_api
	Name        string         `json:"name,omitempty"`
	Category    string         `json:"category,omitempty"`
	Function    string         `json:"function,omitempty"`
	Protocol    string         `json:"protocol,omitempty"`
	Libraries   []string       `json:"libraries,omitempty"`
	Includes    []string       `json:"includes,omitempty"`
	Pins        map[string]any `json:"pins,omitempty"`
	PinsBoard   string         `json:"pinsBoard,omitempty"`
	I2CAddress  string         `json:"i2cAddress,omitempty"`
	Voltage     string         `json:"voltage,omitempty"`
	Gotchas     []string       `json:"gotchas,omitempty"`
	Imports     []string       `json:"imports,omitempty"` // platform_api
	Snippet     string         `json:"snippet,omitempty"` // platform_api
	Suggestions []string       `json:"suggestions,omitempty"`
}

func resolveModule(query, board, platform string) moduleSpecResult {
	res := moduleSpecResult{Query: query}

	// 冷门平台 API(行空板 / MaixCAM 这类「板的软件写法」)
	if key, ok := platformAPIAlias[normalizeModuleName(query)]; ok {
		e := platformAPIs[key]
		res.Matched = true
		res.Kind = "platform_api"
		res.Name = key
		res.Function = e.Label
		res.Imports = e.Imports
		res.Gotchas = e.Gotchas
		res.Snippet = e.Snippet
		return res
	}

	// 元器件模块
	if m, ok := findCatalogModule(query); ok {
		res.Matched = true
		res.Kind = "module"
		res.Name = m.Name
		res.Category = m.Category
		res.Function = m.Entry.Function
		res.Protocol = m.Entry.Protocol
		res.Libraries = filterLibraries(m.Entry, platform, board)
		res.Includes = includesForLibraries(res.Libraries)
		res.Voltage = m.Entry.Voltage
		res.I2CAddress = extractI2CAddress(m.Entry)
		res.Gotchas = splitGotchas(m.Entry.Notes)
		if bk := catalogBoardKey(board); bk != "" {
			if pins, ok := m.Entry.DefaultPins[bk]; ok {
				res.Pins = pins
				res.PinsBoard = bk
			}
		}
		return res
	}

	res.Suggestions = []string{
		"未匹配到 \"" + query + "\";用标准名如 DHT11 / OLED_SSD1306 / SG90,冷门板用 MaixCAM / unihiker。无参数调用本工具可列出全部可用模块。",
	}
	return res
}

// --- 工具入口 ---

type moduleSpecReport struct {
	Board     string             `json:"board,omitempty"`
	Platform  string             `json:"platform,omitempty"`
	Modules   []moduleSpecResult `json:"modules,omitempty"`
	Available []string           `json:"available,omitempty"`
	NextStep  string             `json:"nextStep"`
}

func catalogNames() []string {
	out := make([]string, 0, len(catalogModules))
	for _, m := range catalogModules {
		out = append(out, m.Category+"/"+m.Name)
	}
	for key := range platformAPIs {
		out = append(out, "platform_api/"+key)
	}
	sort.Strings(out)
	return out
}

func runModuleSpec(args map[string]any) (string, error) {
	if catalogLoadErr != nil {
		return "", catalogLoadErr
	}
	names := strSliceArg(args, "modules")
	board := strArg(args, "board", "")
	platform := strArg(args, "platform", "")
	if len(names) == 0 {
		return prettyJSON(moduleSpecReport{
			Available: catalogNames(),
			NextStep:  "传 modules=[...](模块名/别名,如 DHT11、SSD1306、SG90、MaixCAM、unihiker)拿已校验的库/引脚/坑;可加 board(esp32dev/uno 等)按板过滤引脚与库。",
		}), nil
	}
	report := moduleSpecReport{Board: board, Platform: platform}
	for _, n := range names {
		report.Modules = append(report.Modules, resolveModule(n, board, platform))
	}
	report.NextStep = "按 libraries/includes 引库、按 pins 接线、逐条注意 gotchas;platform_api 的 imports/snippet 是该平台的正确写法范式,不要凭记忆改。"
	return prettyJSON(report), nil
}

// --- Markdown 渲染(供脚手架自动注入 docs/module_specs.md,2b 用)---

func moduleSpecMarkdown(board string, results []moduleSpecResult) string {
	var b strings.Builder
	b.WriteString("# 模块地面真值(已校验)\n\n")
	b.WriteString("> 本文件由 hardware_module_spec 自动生成。引库 / 接线 / 避坑以此为准,不要凭记忆改。\n")
	if board != "" {
		b.WriteString("> 目标板:" + board + "\n")
	}
	b.WriteString("\n")
	for _, r := range results {
		if !r.Matched {
			b.WriteString("## " + r.Query + "(未匹配,需人工确认)\n\n")
			continue
		}
		title := r.Name
		if r.Function != "" {
			title += " — " + r.Function
		}
		b.WriteString("## " + title + "\n\n")
		if r.Kind == "platform_api" {
			if len(r.Imports) > 0 {
				b.WriteString("- 导入:`" + strings.Join(r.Imports, "` / `") + "`\n")
			}
			for _, g := range r.Gotchas {
				b.WriteString("- ⚠ " + g + "\n")
			}
			if r.Snippet != "" {
				b.WriteString("\n```python\n" + r.Snippet + "\n```\n")
			}
			b.WriteString("\n")
			continue
		}
		if r.Protocol != "" {
			b.WriteString("- 协议:" + r.Protocol + "\n")
		}
		if len(r.Libraries) > 0 {
			b.WriteString("- 库:" + strings.Join(r.Libraries, ", ") + "\n")
		}
		if len(r.Includes) > 0 {
			b.WriteString("- 头文件:`" + strings.Join(r.Includes, "` `") + "`\n")
		}
		if r.I2CAddress != "" {
			b.WriteString("- I2C 地址:" + r.I2CAddress + "\n")
		}
		if r.Voltage != "" {
			b.WriteString("- 供电:" + r.Voltage + "\n")
		}
		if len(r.Pins) > 0 {
			b.WriteString("- 默认引脚(" + r.PinsBoard + "):" + formatPins(r.Pins) + "\n")
		}
		for _, g := range r.Gotchas {
			b.WriteString("- ⚠ " + g + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func formatPins(pins map[string]any) string {
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+formatPinValue(pins[k]))
	}
	return strings.Join(parts, ", ")
}

func formatPinValue(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.Itoa(int(t))
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// bundledHardwareCommandFiles 把 .reasonix/commands/hardware/*.md 作为脚手架输出的一部分。
// 这样新建的硬件项目目录里就直接能用 /hardware:init、/hardware:plan、/hardware:validate 等
// 斜杠命令 —— 不必再依赖学生把命令文件手动安装到 ~/.config/reasonix/commands/。
func bundledHardwareCommandFiles() map[string]string {
	out := map[string]string{}
	entries, err := bundledCommandsFS.ReadDir("data/commands")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := bundledCommandsFS.ReadFile("data/commands/" + e.Name())
		if err != nil {
			continue
		}
		out[".reasonix/commands/hardware/"+e.Name()] = string(b)
	}
	return out
}

// --- 编译错误蒸馏 ---
//
// 弱模型最容易卡死的地方是「读不懂级联报错」—— 编译失败常常有 50+ 行,
// 根因被淹在中间。distillBuildError 做两件事:
//   1) 抽出第一条真正的编译/链接错误(根因),丢掉脚手架噪音;
//   2) 对已知错误模式给出立刻可做的修法(library 替换/装库命令/引脚冲突)。
//
// 这是 grounding 体系的运行时镜像:不再"上下文给事实",而是"报错时给修法"。
// 关键样例:`Servo.h: No such file` + ESP32 项目 → 直接告诉模型改用 ESP32Servo,
// 而不是让它再去查文档或猜。

type errorPattern struct {
	keywords []string // 在错误输出里必须全部出现
	platform string   // 限定平台("" 表示通用);用 kind 头部匹配
	fix      string   // 具体可执行的修法
}

var compileErrorPatterns = []errorPattern{
	// P4-flash 实测踩到这个:ESP32 上 #include <Servo.h> 编译失败
	{
		keywords: []string{"Servo.h", "No such file"},
		platform: "arduino", // arduino-cli + ESP32 fqbn
		fix:      "ESP32 不能用 AVR 的 Servo 库。把 `#include <Servo.h>` 改成 `#include <ESP32Servo.h>`;若未装库,先调用 arduino_lib_install libs=[\"ESP32Servo\"]",
	},
	{
		keywords: []string{"ESP32Servo.h", "No such file"},
		fix:      "缺 ESP32Servo 库。调用 arduino_lib_install libs=[\"ESP32Servo\"](Arduino CLI),或在 platformio.ini 的 lib_deps 加 `madhephaestus/ESP32Servo@^1.1.2`。",
	},
	{
		keywords: []string{"Adafruit_SSD1306.h", "No such file"},
		fix:      "缺 SSD1306 库。调用 arduino_lib_install libs=[\"Adafruit SSD1306\", \"Adafruit GFX Library\", \"Adafruit Unified Sensor\"]",
	},
	{
		keywords: []string{"DHT.h", "No such file"},
		fix:      "缺 DHT 库。调用 arduino_lib_install libs=[\"DHT sensor library\", \"Adafruit Unified Sensor\"]",
	},
	{
		keywords: []string{"WiFi.h", "No such file"},
		platform: "arduino",
		fix:      "WiFi.h 通常只在 ESP32/ESP8266 上有。AVR(Uno/Nano)无板载 WiFi。请确认 fqbn 是 esp32 系或换用外接 WiFi 模块。",
	},
	{
		keywords: []string{"No such file or directory"},
		fix:      "缺库或缺头文件。看 fatal error 那行里的文件名:Arduino 项目先 `arduino-cli lib search <关键词>` 找准确库名,再调用 arduino_lib_install libs=[<库名>];PlatformIO 加进 lib_deps。",
	},
	{
		keywords: []string{"was not declared in this scope"},
		fix:      "标识符未声明:函数/变量未定义,或对应库的头文件没 #include。看错误行的标识符名,找它来自哪个库,补上 #include 或确认拼写。",
	},
	{
		keywords: []string{"undefined reference to"},
		fix:      "链接错误:声明找到了但实现没链上。常见原因:库虽 #include 了但没装(检查 lib install)、函数名拼错、PlatformIO lib_deps 漏配。",
	},
	{
		keywords: []string{"redefinition of"},
		fix:      "重复定义:同一个符号被定义多次。看错误行,常见原因是头文件没加 include guard、或 .ino 文件被多次拉入。",
	},
	{
		keywords: []string{"avrdude:", "not responding"},
		fix:      "AVR 烧录失败:开发板未响应。检查 USB 线、端口选择、Bootloader、按 RESET 重试;ESP32 项目可能用错了 fqbn(应是 esp32:esp32:* 不是 arduino:avr:*)。",
	},
	{
		keywords: []string{"Could not open port"},
		fix:      "串口被占用或不存在:关掉 IDE 串口监视器、运行 `hardware_detect` 确认端口;macOS 用 /dev/cu.*,Windows 用 COMx。",
	},
	{
		keywords: []string{"ModuleNotFoundError"},
		fix:      "Python 缺包。装包后重试:`pip install <包名>`;行空板上常用 `pip install pinpong unihiker`。",
	},
	{
		keywords: []string{"ImportError"},
		fix:      "Python 导入失败:模块名拼错、版本不匹配、或本环境根本没装。MaixCAM 用 `from maix import ...`,行空板 GUI 用 `from unihiker import GUI`、GPIO 用 `from pinpong.board import Board, Pin`。",
	},
}

// firstErrorLine 从级联输出里抽第一条「真错误」:优先 fatal error / error: 行。
func firstErrorLine(output string) string {
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	// 第一轮:找 fatal error(最根因)
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.Contains(t, "fatal error:") {
			return t
		}
	}
	// 第二轮:第一条 "error:" 或常见错误关键词
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		if strings.Contains(low, "error:") ||
			strings.Contains(t, "was not declared") ||
			strings.Contains(t, "undefined reference") ||
			strings.Contains(t, "ModuleNotFoundError") ||
			strings.Contains(t, "ImportError") ||
			strings.Contains(t, "Nothing to build") {
			return t
		}
	}
	return ""
}

func distillBuildError(kind, output string) (rootCause, fixHint string) {
	rootCause = firstErrorLine(output)
	for _, p := range compileErrorPatterns {
		if p.platform != "" && !strings.HasPrefix(kind, p.platform) {
			continue
		}
		matched := true
		for _, kw := range p.keywords {
			if !strings.Contains(output, kw) {
				matched = false
				break
			}
		}
		if matched {
			fixHint = p.fix
			return
		}
	}
	return
}

// modulesToConnections 把学生在 scaffold 时说的模块名解析为 manifest 连接。
// 关键价值:学生说"测温度",scaffold 不再写默认占位"板载 LED",
// 而是真把 DHT11 写进 hardware_manifest.json 的 connections —— 然后
// moduleSpecsForConnections 自动注入 DHT 的库/引脚/坑到 docs/module_specs.md。
// 一条参数,放大了整套 grounding 体系。
func modulesToConnections(moduleNames []string, board string) []hardwareConnection {
	if catalogLoadErr != nil {
		return nil
	}
	out := make([]hardwareConnection, 0, len(moduleNames))
	seen := map[string]bool{}
	bk := catalogBoardKey(board)
	for _, n := range moduleNames {
		key := normalizeModuleName(n)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		m, ok := findCatalogModule(n)
		if !ok {
			// 平台 API(unihiker / maixcam)虽不是元器件,仍可登记为 controller 注解
			continue
		}
		conn := hardwareConnection{
			Name:     m.Name,
			Role:     roleForCategory(m.Category),
			Protocol: m.Entry.Protocol,
			Voltage:  m.Entry.Voltage,
			Notes:    m.Entry.Notes,
		}
		if bk != "" {
			if pinMap, ok := m.Entry.DefaultPins[bk]; ok {
				conn.Pins = formatPinSlice(pinMap)
			}
		}
		out = append(out, conn)
	}
	return out
}

func roleForCategory(cat string) string {
	switch cat {
	case "sensors":
		return "sensor"
	case "displays":
		return "display"
	case "actuators":
		return "actuator"
	case "communication_modules":
		return "communication"
	case "vision_modules":
		return "vision"
	case "power_modules":
		return "power"
	}
	return "module"
}

func formatPinSlice(pins map[string]any) []string {
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+formatPinValue(pins[k]))
	}
	return out
}

// moduleSpecsForConnections 把 manifest 连接里能识别的模块解析成已校验规格,
// 渲染成 docs/module_specs.md 的内容。无任何模块命中时返回空串(不写噪音文件)。
// 这是「自动注入」的核心:让模型从项目上下文直接拿到事实,不必记忆、不必猜。
func moduleSpecsForConnections(conns []hardwareConnection, board string) string {
	if catalogLoadErr != nil || len(conns) == 0 {
		return ""
	}
	var results []moduleSpecResult
	seen := map[string]bool{}
	for _, c := range conns {
		key := normalizeModuleName(c.Name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if r := resolveModule(c.Name, board, ""); r.Matched {
			results = append(results, r) // 只写命中的,避免一堆「未匹配」噪音
		}
	}
	if len(results) == 0 {
		return ""
	}
	return moduleSpecMarkdown(board, results)
}
