package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func jsonPathFragment(path string) string {
	b, _ := json.Marshal(filepath.FromSlash(path))
	return strings.Trim(string(b), `"`)
}

func TestJSONRPCToolsListIncludesHardwareTools(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	err := handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`), w)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{"hardware_detect", "hardware_board_profile", "hardware_project_scaffold", "hardware_project_context", "hardware_project_validate", "hardware_project_audit", "hardware_project_repair", "hardware_repair_catalog", "hardware_evidence_record", "hardware_evidence_status", "hardware_device_verify_plan", "esp_idf_mcp_config", "ssh_deploy_run"} {
		if !strings.Contains(body, want) {
			t.Fatalf("tools/list missing %q:\n%s", want, body)
		}
	}
}

func TestBoardProfileReturnsESP32Constraints(t *testing.T) {
	out, err := runBoardProfile(map[string]any{"board": "esp32_arduino"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id": "esp32_arduino"`, `"logicVoltage": "3.3V"`, "GPIO16/RX2", "GPIO17/TX2", "启动绑带脚", "PlatformIO 根目录 .ino 不参与构建"} {
		if !strings.Contains(out, want) {
			t.Fatalf("board profile missing %q:\n%s", want, out)
		}
	}
}

// 数据驱动注册表(#1)回归:新加的板卡家族必须能被 findBoardProfile 命中并注入事实,
// 且工具链映射(FQBN/target)正确——否则编译第一步就因非法 FQBN 报错。
func TestNewBoardFamiliesResolveAndMapToolchains(t *testing.T) {
	cases := []struct {
		board, wantFQBN, wantInProfile string
	}{
		{"esp32s3", "esp32:esp32:esp32s3", "没有 input-only"},
		{"esp32c3", "esp32:esp32:esp32c3", "RISC-V"},
		{"esp8266", "esp8266:esp8266:nodemcuv2", "启动约束"},
		{"pico", "rp2040:rp2040:rpipico", "BOOTSEL"},
		{"stm32", "stm32", "PC13"},
		{"microbit", "microbit", "MakeCode"},
		{"mega", "arduino:avr:mega", "D20/D21"},
	}
	for _, c := range cases {
		if got := arduinoFQBN(c.board); got != c.wantFQBN {
			t.Errorf("arduinoFQBN(%q) = %q, want %q", c.board, got, c.wantFQBN)
		}
		if _, ok := findBoardProfile(c.board, ""); !ok {
			t.Errorf("findBoardProfile(%q) did not match a profile — new board not in registry", c.board)
		}
		out, err := runBoardProfile(map[string]any{"board": c.board})
		if err != nil {
			t.Fatalf("runBoardProfile(%q): %v", c.board, err)
		}
		if !strings.Contains(out, c.wantInProfile) {
			t.Errorf("board profile for %q missing %q", c.board, c.wantInProfile)
		}
	}
}

// 安全回归:显式指名的板卡在指定 platform 下匹配不到时,必须跨平台按名字查找,
// 绝不能回退成平台默认板——"platform=arduino board=esp32" 曾静默回退成 UNO,
// 返回 5V 逻辑电平,模型据此写接线方案会烧毁 3.3V 的 ESP32。
func TestFindBoardProfileNeverFallsBackToWrongVoltage(t *testing.T) {
	// esp32 挂在 platformio 分类下,但 Arduino 框架同样能编它:跨平台命中,电压必须是 3.3V。
	profile, ok := findBoardProfile("esp32", "arduino")
	if !ok {
		t.Fatal("findBoardProfile(esp32, arduino) should cross-platform match esp32_arduino")
	}
	if profile.ID != "esp32_arduino" {
		t.Fatalf("findBoardProfile(esp32, arduino) = %q, want esp32_arduino", profile.ID)
	}
	if profile.LogicVoltage != "3.3V" {
		t.Fatalf("esp32 profile logic voltage = %q, want 3.3V", profile.LogicVoltage)
	}

	// S3/C3 同理。
	for _, board := range []string{"esp32s3", "esp32c3"} {
		p, ok := findBoardProfile(board, "arduino")
		if !ok || p.LogicVoltage != "3.3V" {
			t.Errorf("findBoardProfile(%q, arduino) = (%q ok=%v), want 3.3V profile", board, p.LogicVoltage, ok)
		}
	}

	// 显式指名但注册表里查无此板:必须 ok=false,不能回退默认板。
	if p, ok := findBoardProfile("totally_unknown_board", "arduino"); ok {
		t.Fatalf("unknown board should not resolve, got %q", p.ID)
	}

	// 不指名板卡时,平台默认板回退仍然有效(合法用法)。
	p, ok := findBoardProfile("", "arduino")
	if !ok || p.Board != "uno" {
		t.Fatalf("findBoardProfile(\"\", arduino) = (%q ok=%v), want uno default", p.Board, ok)
	}
}

func TestRepairCatalogListsAutoAndManualRules(t *testing.T) {
	out, err := runRepairCatalog(map[string]any{"platform": "platformio"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"platformio_root_ino_to_src_main", "hardware_project_repair", "missing_project_context", "wifi_ap_not_reachable", `"autoRepairCount": 2`} {
		if !strings.Contains(out, want) {
			t.Fatalf("repair catalog missing %q:\n%s", want, out)
		}
	}
}

// #3b 回归:arduino 平台的修复目录必须包含"core 未安装"规则,并指向 arduino_core_install
// 自动修复工具(这是全新环境首次编译最常见、过去无规则可循的墙)。
func TestRepairCatalogHasCoreNotInstalledRule(t *testing.T) {
	out, err := runRepairCatalog(map[string]any{"platform": "arduino"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"arduino_core_not_installed", "arduino_core_install", "core 未安装"} {
		if !strings.Contains(out, want) {
			t.Fatalf("arduino repair catalog missing %q:\n%s", want, out)
		}
	}
}

// 缺库自动修复回归:arduino 平台的修复目录必须包含"缺第三方库"规则并指向
// arduino_lib_install 自动修复工具(真实项目最高频编译墙,此前只有手动指引);
// 工具本身必须已注册且空参数被拒绝。
func TestRepairCatalogHasMissingLibraryRule(t *testing.T) {
	out, err := runRepairCatalog(map[string]any{"platform": "arduino"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"missing_arduino_library", "arduino_lib_install", "Adafruit Unified Sensor"} {
		if !strings.Contains(out, want) {
			t.Fatalf("arduino repair catalog missing %q:\n%s", want, out)
		}
	}

	registered := false
	for _, tl := range tools {
		if tl.name == "arduino_lib_install" {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("arduino_lib_install 工具未注册")
	}

	if _, err := runArduinoLibInstall(map[string]any{}); err == nil || !strings.Contains(err.Error(), "libs 必填") {
		t.Fatalf("空 libs 应报必填错误, got %v", err)
	}
}

// LED_PIN 回归:脚手架的板载 LED 必须按板卡取(ESP32=GPIO2,AVR=D13)。
// 曾硬编码 13,ESP32 第一课的"烧录成功"视觉反馈永远不亮。
func TestScaffoldLEDPinFollowsBoard(t *testing.T) {
	files := scaffoldArduino("blink_demo", "esp32")
	ino := files["blink_demo/blink_demo.ino"]
	if !strings.Contains(ino, "LED_PIN = 2;") {
		t.Errorf("arduino+esp32 脚手架 LED 应为 GPIO2:\n%s", ino)
	}
	files = scaffoldArduino("blink_demo", "nano")
	if !strings.Contains(files["blink_demo/blink_demo.ino"], "LED_PIN = 13;") {
		t.Error("nano 脚手架 LED 应保持 D13")
	}
	pio := scaffoldPlatformIO("blink_demo", "uno")
	if !strings.Contains(pio["include/pins.h"], "PIN_LED = 13;") {
		t.Errorf("platformio+uno 脚手架 LED 应为 D13:\n%s", pio["include/pins.h"])
	}
	pio = scaffoldPlatformIO("blink_demo", "esp32s3")
	if !strings.Contains(pio["include/pins.h"], "PIN_LED = 2;") {
		t.Error("platformio+esp32s3 脚手架 LED 应为 GPIO2(带 RGB 灯珠提示)")
	}
}

// CH340 驱动指引回归:Windows 零串口必须给出针对性驱动安装指引(而非一句"检查驱动");
// 检测到 CH340 串口但认不出板型时必须给 FQBN 指引(此前一条建议都不给)。
func TestRecommendationsForDriverAndUnknownBoard(t *testing.T) {
	if got := noSerialPortsAdvice("windows"); !strings.Contains(got, "CH341SER") || !strings.Contains(got, "设备管理器") {
		t.Errorf("Windows 零串口应给 CH340 驱动指引, got %q", got)
	}
	if got := noSerialPortsAdvice("darwin"); strings.Contains(got, "CH341SER") {
		t.Errorf("macOS 不应给 Windows 驱动指引, got %q", got)
	}

	rec := recommendations(detectReport{
		SerialPorts: []string{"/dev/cu.usbserial-110"},
		Boards:      []boardReport{{Port: "/dev/cu.usbserial-110", BoardName: "Unknown", Properties: "pid=0x7523, vid=0x1A86"}},
	})
	joined := strings.Join(rec, "\n")
	for _, want := range []string{"无法自动识别板型", "CH340", "esp32:esp32:esp32", "arduino:avr:uno"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Unknown 板建议缺 %q:\n%s", want, joined)
		}
	}
	if got := usbSerialChipHint("vid=0x303A"); !strings.Contains(got, "ESP32-S2/S3/C3") {
		t.Errorf("303a 应识别为乐鑫原生 USB, got %q", got)
	}
}

// #3a 回归:Windows COM 口解析能从 PowerShell GetPortNames / reg query 输出里抽出端口。
func TestParseWindowsCOMPorts(t *testing.T) {
	psOut := "$ powershell ...\nCOM3\r\nCOM10\r\nCOM3\r\n"
	got := parseWindowsCOMPorts(psOut)
	if len(got) != 2 || got[0] != "COM10" || got[1] != "COM3" {
		t.Fatalf("powershell parse = %v, want [COM10 COM3] (去重排序)", got)
	}
	regOut := "HKEY_LOCAL_MACHINE\\HARDWARE\\DEVICEMAP\\SERIALCOMM\n    \\Device\\Serial0    REG_SZ    COM4\n    \\Device\\USBSER000    REG_SZ    COM7\n"
	got = parseWindowsCOMPorts(regOut)
	if len(got) != 2 || got[0] != "COM4" || got[1] != "COM7" {
		t.Fatalf("reg parse = %v, want [COM4 COM7]", got)
	}
	if len(parseWindowsCOMPorts("no ports here\nCOMMENT something")) != 0 {
		t.Fatal("must not match COMMENT or non-COM tokens")
	}
}

func TestScaffoldPlatformIOMapsESP32S3Board(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "pio_demo",
		"platform":     "platformio",
		"board":        "esp32s3",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pio_demo", "platformio.ini"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"[env:esp32s3]", "platform = espressif32", "board = esp32-s3-devkitc-1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("platformio.ini missing %q:\n%s", want, body)
		}
	}
}

func TestScaffoldAllSupportedPlatformsCreateTeachingFiles(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	cases := []struct {
		platform string
		board    string
		wantFile string
		wantText string
	}{
		{"arduino", "uno", "arduino_demo/arduino_demo.ino", "Serial.begin"},
		{"platformio", "esp32dev", "src/main.cpp", "Serial.begin"},
		{"esp_idf", "esp32s3", "main/main.c", "ESP_LOGI"},
		{"micropython", "esp32", "src/main.py", "machine"},
		{"unihiker_python", "unihiker", "src/main.py", "read_sensor"},
		{"maixcam_python", "maixcam", "src/main.py", "camera.Camera"},
		{"raspberry_pi_python", "raspberry_pi", "src/main.py", "read_sensor"},
	}

	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			name := strings.ReplaceAll(tc.platform, "_python", "") + "_demo"
			_, err := runScaffold(map[string]any{
				"project_name": name,
				"platform":     tc.platform,
				"board":        tc.board,
			})
			if err != nil {
				t.Fatalf("runScaffold() error = %v", err)
			}
			readme, err := os.ReadFile(filepath.Join(dir, name, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"教学要求", "验证命令", "学生需要能解释每一行代码"} {
				if !strings.Contains(string(readme), want) {
					t.Fatalf("README missing %q:\n%s", want, readme)
				}
			}
			mainFile, err := os.ReadFile(filepath.Join(dir, name, filepath.FromSlash(tc.wantFile)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(mainFile), tc.wantText) {
				t.Fatalf("%s missing %q:\n%s", tc.wantFile, tc.wantText, mainFile)
			}

			manifestBytes, err := os.ReadFile(filepath.Join(dir, name, "hardware_manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				SchemaVersion      string   `json:"schemaVersion"`
				Platform           string   `json:"platform"`
				Board              string   `json:"board"`
				Entrypoints        []string `json:"entrypoints"`
				PreferredProtocols []string `json:"preferredProtocols"`
				Connections        []struct {
					Name     string   `json:"name"`
					Role     string   `json:"role"`
					Protocol string   `json:"protocol"`
					Pins     []string `json:"pins"`
					Voltage  string   `json:"voltage"`
				} `json:"connections"`
				MCPTools     []string `json:"mcpTools"`
				Verification struct {
					LocalCommand       string   `json:"localCommand"`
					MinimumLocalGate   string   `json:"minimumLocalGate"`
					RealHardwareChecks []string `json:"realHardwareChecks"`
				} `json:"verification"`
			}
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatalf("hardware_manifest.json is not valid JSON: %v\n%s", err, manifestBytes)
			}
			if manifest.SchemaVersion != "reasonix-hardware-project/v1" || manifest.Platform != tc.platform || manifest.Board != tc.board {
				t.Fatalf("unexpected manifest metadata: %+v", manifest)
			}
			if !contains(manifest.Entrypoints, tc.wantFile) || !contains(manifest.PreferredProtocols, "UART") {
				t.Fatalf("manifest missing entrypoint/protocol: %+v", manifest)
			}
			if len(manifest.Connections) == 0 || manifest.Connections[0].Protocol == "" || manifest.Connections[0].Voltage == "" {
				t.Fatalf("manifest missing structured connections: %+v", manifest.Connections)
			}
			if !contains(manifest.MCPTools, "hardware_detect") || !contains(manifest.MCPTools, "hardware_project_audit") || !contains(manifest.MCPTools, "hardware_project_validate") || !contains(manifest.MCPTools, "hardware_evidence_record") || !contains(manifest.MCPTools, "hardware_evidence_status") || !contains(manifest.MCPTools, "hardware_device_verify_plan") {
				t.Fatalf("manifest missing MCP tools: %+v", manifest)
			}
			if manifest.Verification.LocalCommand == "" || manifest.Verification.MinimumLocalGate != "compile_or_syntax" || len(manifest.Verification.RealHardwareChecks) == 0 {
				t.Fatalf("manifest missing verification metadata: %+v", manifest.Verification)
			}

			docChecks := map[string][]string{
				"docs/board_profile.md":       {"板卡 Profile", "风险引脚", "验证流程"},
				"docs/failure_patterns.md":    {"硬件失败模式", "自动修复", "需要证据"},
				"docs/wiring.md":              {"UART", "I2C", "hardware_manifest.json", "Manifest 连接清单"},
				"docs/verification.md":        {"hardware_detect", "hardware_project_validate", "hardware_evidence_record", "失败判断"},
				"tests/hardware_checklist.md": {"真实硬件验证", "学生答辩检查", "hardware_project_validate", "hardware_evidence_record"},
			}
			for rel, needles := range docChecks {
				body, err := os.ReadFile(filepath.Join(dir, name, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatalf("missing %s: %v", rel, err)
				}
				for _, want := range needles {
					if !strings.Contains(string(body), want) {
						t.Fatalf("%s missing %q:\n%s", rel, want, body)
					}
			}
		})
	}
}

func TestProjectContextAddsMissingFilesWithoutOverwritingExistingReadme(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "legacy_snake")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("# Customer README\nkeep this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "platformio.ini"), []byte("[env:esp32dev]\nplatform = espressif32\nboard = esp32dev\nframework = arduino\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runProjectContext(map[string]any{
		"project_dir":             projectDir,
		"platform":                "platformio",
		"board":                   "esp32dev",
		"allow_outside_workspace": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"projectName": "legacy_snake"`, `"platform": "platformio"`, `"board": "esp32dev"`, `"status": "skipped"`, jsonPathFragment("docs/board_profile.md"), jsonPathFragment("docs/failure_patterns.md"), "hardware_manifest.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context output missing %q:\n%s", want, out)
		}
	}
	readme, err := os.ReadFile(filepath.Join(projectDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "Customer README") || strings.Contains(string(readme), "教学要求") {
		t.Fatalf("existing README should not be overwritten:\n%s", readme)
	}
	for _, rel := range []string{"hardware_manifest.json", "docs/wiring.md", "docs/verification.md", "docs/board_profile.md", "docs/failure_patterns.md", "tests/hardware_checklist.md"} {
		if !exists(filepath.Join(projectDir, filepath.FromSlash(rel))) {
			t.Fatalf("context did not create %s", rel)
		}
	}
}

func TestProjectAuditPassesGeneratedScaffold(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "audit_demo",
		"platform":     "platformio",
		"board":        "esp32dev",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	out, err := runProjectAudit(map[string]any{"project_dir": filepath.Join(dir, "audit_demo")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`0 failed, 0 warnings`, `"expectedPlatform": "platformio"`, "hardware_manifest.json", "tests/hardware_checklist.md", `"kind": "connection"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectAuditWarnsAboutFiveVoltLogicOnESP32(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "voltage_demo",
		"platform":     "platformio",
		"board":        "esp32dev",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	manifestPath := filepath.Join(dir, "voltage_demo", "hardware_manifest.json")
	var manifest map[string]any
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["connections"] = []map[string]any{{
		"name":     "5V ultrasonic echo",
		"role":     "sensor",
		"protocol": "GPIO",
		"pins":     []string{"GPIO4"},
		"voltage":  "5V",
	}}
	updated, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, append(updated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectAudit(map[string]any{"project_dir": filepath.Join(dir, "voltage_demo")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "connection"`, `"status": "warning"`, "level_shifter_or_3.3V_module"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectAuditWarnsAboutDuplicateGPIOPins(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "pin_conflict_demo",
		"platform":     "platformio",
		"board":        "esp32dev",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	manifestPath := filepath.Join(dir, "pin_conflict_demo", "hardware_manifest.json")
	var manifest map[string]any
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["connections"] = []map[string]any{
		{
			"name":     "status_led",
			"role":     "actuator",
			"protocol": "GPIO",
			"pins":     []string{"GPIO2"},
			"voltage":  "3.3V",
		},
		{
			"name":     "buzzer",
			"role":     "actuator",
			"protocol": "GPIO",
			"pins":     []string{"GPIO2"},
			"voltage":  "3.3V",
		},
	}
	updated, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, append(updated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectAudit(map[string]any{"project_dir": filepath.Join(dir, "pin_conflict_demo")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "pin_conflict"`, `"status": "warning"`, "GPIO2:status_led,buzzer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectAuditReportsMissingHardwareContext(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectAudit(map[string]any{"project_dir": dir, "platform": "micropython"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"expectedPlatform": "micropython"`, `"status": "failed"`, "hardware_manifest.json", "docs/wiring.md", "tests/hardware_checklist.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
}

func TestDeviceVerifyPlanGeneratesPlatformIOCommandAndMCPSteps(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "pio_plan",
		"platform":     "platformio",
		"board":        "esp32dev",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	out, err := runDeviceVerifyPlan(map[string]any{
		"project_dir": filepath.Join(dir, "pio_plan"),
		"port":        "/dev/cu.fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	var plan deviceVerifyPlanReport
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not valid JSON: %v\n%s", err, out)
	}
	if plan.Platform != "platformio" || plan.Board != "esp32dev" {
		t.Fatalf("unexpected plan metadata: %+v", plan)
	}
	for _, want := range []string{"scripts/hardware-device-verify.sh", "--platform platformio", "--environment esp32dev", "--port /dev/cu.fake"} {
		if !strings.Contains(plan.RealDeviceCommand, want) {
			t.Fatalf("real command missing %q:\n%s", want, plan.RealDeviceCommand)
		}
	}
	if contains(plan.MissingInputs, "port") {
		t.Fatalf("port should not be missing when supplied: %+v", plan.MissingInputs)
	}
	if !planHasTool(plan.MCPSteps, "platformio_run") || !planHasTool(plan.MCPSteps, "hardware_evidence_status") {
		t.Fatalf("plan missing MCP steps: %+v", plan.MCPSteps)
	}
}

func TestDeviceVerifyPlanReportsMissingHostForMaixCAM(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "maix_plan",
		"platform":     "maixcam_python",
		"board":        "maixcam",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	out, err := runDeviceVerifyPlan(map[string]any{"project_dir": filepath.Join(dir, "maix_plan")})
	if err != nil {
		t.Fatal(err)
	}
	var plan deviceVerifyPlanReport
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not valid JSON: %v\n%s", err, out)
	}
	if !contains(plan.MissingInputs, "host") {
		t.Fatalf("host should be reported missing for placeholder MaixCAM target: %+v", plan.MissingInputs)
	}
	if !strings.Contains(plan.RealDeviceCommand, "--host maixcam-xxxx.local") {
		t.Fatalf("real command should include the scaffold default host for editing:\n%s", plan.RealDeviceCommand)
	}
	if !planHasTool(plan.MCPSteps, "ssh_deploy_run") {
		t.Fatalf("plan missing ssh_deploy_run step: %+v", plan.MCPSteps)
	}
}

func TestDeviceVerifyPlanRequiresExplicitDefaultSSHHost(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "unihiker_plan",
		"platform":     "unihiker_python",
		"board":        "unihiker",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	projectDir := filepath.Join(dir, "unihiker_plan")
	out, err := runDeviceVerifyPlan(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	var plan deviceVerifyPlanReport
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not valid JSON: %v\n%s", err, out)
	}
	if !contains(plan.MissingInputs, "host") {
		t.Fatalf("default Unihiker host should still require explicit confirmation: %+v", plan.MissingInputs)
	}
	if plan.ReadyForRealDevice {
		t.Fatalf("plan should not be ready with only the scaffold default host: %+v", plan)
	}
	if !strings.Contains(plan.RealDeviceCommand, "--host 10.1.2.3") {
		t.Fatalf("real command should still include editable default host:\n%s", plan.RealDeviceCommand)
	}

	out, err = runDeviceVerifyPlan(map[string]any{
		"project_dir": projectDir,
		"host":        "10.1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not valid JSON: %v\n%s", err, out)
	}
	if contains(plan.MissingInputs, "host") {
		t.Fatalf("explicitly supplied host should not be missing: %+v", plan.MissingInputs)
	}
	if plan.Resolved["hostSource"] != "argument" {
		t.Fatalf("hostSource should record explicit argument: %+v", plan.Resolved)
	}
}

func planHasTool(steps []deviceVerifyPlanStep, tool string) bool {
	for _, step := range steps {
		if step.Tool == tool {
			return true
		}
	}
	return false
}

// 知识库一致性自检:每个支持的平台都必须有一个在注册表里真实存在、平台匹配的
// defaultBoard;否则脚手架/board_profile 注入会静默落空,弱模型拿不到板卡事实。
func TestPlatformDefaultBoardsExist(t *testing.T) {
	platforms := []string{"arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}
	for _, p := range platforms {
		db := defaultBoard(p)
		if db == "" {
			t.Errorf("平台 %q 没有 defaultBoard", p)
			continue
		}
		if _, ok := findBoardProfile(db, p); !ok {
			t.Errorf("平台 %q 的 defaultBoard=%q 在注册表里找不到匹配 profile", p, db)
		}
	}
}

// arduino-cli monitor 读到 stdin EOF 会约 1s 自退,在 CH340 ESP32 上一个字节都采不到
// (2026-06 真机实测)。keepStdinOpen 给它一个永不 EOF 的 stdin,让它跑满采样窗口才被超时杀。
// 这里用 cat 复现:cat 读 stdin 到 EOF 才退 —— 默认(/dev/null)立即退,keepStdinOpen 撑到超时。
func TestKeepStdinOpenPreventsEOFExit(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("no cat on PATH")
	}
	// 默认 stdin=/dev/null:cat 立即读到 EOF 退出,远早于超时。
	start := time.Now()
	_, _ = runCommandText("cat", nil, "", 800*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("默认 stdin 应让 cat 立即 EOF 退出,实际耗时 %v(说明默认行为变了)", elapsed)
	}
	// keepStdinOpen=true:cat 阻塞读 stdin,一直撑到 800ms 超时——这正是 monitor 需要的。
	start = time.Now()
	_, err := runCommandTextOpts("cat", nil, "", 800*time.Millisecond, true)
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("keepStdinOpen=true 应撑到超时(~800ms),实际 %v(err=%v)——stdin 没撑住", elapsed, err)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("keepStdinOpen 跑满窗口应返回 timed out,得到 err=%v", err)
	}
}

func findDeployStep(steps []deviceVerifyPlanStep) *deviceVerifyPlanStep {
	for i := range steps {
		if steps[i].Tool == "ssh_deploy_run" {
			return &steps[i]
		}
	}
	return nil
}

// Python 子平台必须按 import 细分:行空板/MaixCAM/树莓派/真 MicroPython 库完全不同,
// 只看到 .py 就一律当 micropython 会让弱模型走错脚手架和部署。
func TestDetectPythonPlatformFromImports(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{"maixcam", "from maix import camera, display\n", "maixcam_python"},
		{"unihiker_pinpong", "from pinpong.board import Board, Pin\n", "unihiker_python"},
		{"unihiker_gui", "from unihiker import GUI\n", "unihiker_python"},
		{"raspberry_gpiozero", "from gpiozero import LED, Button\n", "raspberry_pi_python"},
		{"raspberry_rpigpio", "import RPi.GPIO as GPIO\n", "raspberry_pi_python"},
		{"raspberry_picamera", "from picamera2 import Picamera2\n", "raspberry_pi_python"},
		{"micropython_machine", "from machine import Pin\n", "micropython"},
		{"ambiguous", "print('hello world')\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "main.py"), []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := detectPythonPlatform(dir); got != tc.want {
				t.Fatalf("detectPythonPlatform() = %q, want %q", got, tc.want)
			}
			if tc.want == "" {
				return
			}
			// 具体平台要排在 python_or_micropython 之前,并被 audit 选中。
			types := detectProjectTypes(dir)
			if !contains(types, tc.want) {
				t.Fatalf("detectProjectTypes() = %+v, want to contain %q", types, tc.want)
			}
			if got := auditExpectedPlatform("auto", "", types); got != tc.want {
				t.Fatalf("auditExpectedPlatform(auto) = %q, want %q (detected=%+v)", got, tc.want, types)
			}
		})
	}
}

// 三种 SSH 部署平台不能再共用一条命令:树莓派要 /home/pi 路径,
// MaixCAM 要连模型/素材一起传,行空板保持 root+src。
func TestRealDevicePlanStepsDifferentiatePythonDeploy(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "proj_demo")

	rpi := findDeployStep(realDevicePlanSteps("raspberry_pi_python", projectDir, "raspberry_pi", map[string]string{"host": "pi.local", "user": "pi"}, 115200, 8))
	if rpi == nil {
		t.Fatal("raspberry_pi_python missing ssh_deploy_run step")
	}
	if got := rpi.Arguments["remote_path"].(string); !strings.HasPrefix(got, "/home/pi/") {
		t.Errorf("RPi(pi 用户) remote_path 应在 /home/pi 下,got %q", got)
	}
	if got := rpi.Arguments["remote_path"].(string); strings.HasPrefix(got, "/root/") {
		t.Errorf("RPi 不应部署到 pi 用户写不进的 /root,got %q", got)
	}

	maix := findDeployStep(realDevicePlanSteps("maixcam_python", projectDir, "maixcam", map[string]string{"host": "m.local", "user": "root"}, 115200, 8))
	if maix == nil {
		t.Fatal("maixcam_python missing ssh_deploy_run step")
	}
	if got := maix.Arguments["local_path"].(string); got != projectDir {
		t.Errorf("MaixCAM 应部署整个项目目录(含 models/assets),got local_path=%q want %q", got, projectDir)
	}
	if got := maix.Arguments["command"].(string); !strings.Contains(got, "/src/main.py") {
		t.Errorf("MaixCAM 部署整树后命令应指向 src/main.py,got %q", got)
	}

	uni := findDeployStep(realDevicePlanSteps("unihiker_python", projectDir, "unihiker", map[string]string{"host": "10.1.2.3", "user": "root"}, 115200, 8))
	if uni == nil {
		t.Fatal("unihiker_python missing ssh_deploy_run step")
	}
	if got := uni.Arguments["local_path"].(string); got != filepath.Join(projectDir, "src") {
		t.Errorf("Unihiker 应只传 src,got local_path=%q", got)
	}
	if got := uni.Arguments["remote_path"].(string); !strings.HasPrefix(got, "/root/") {
		t.Errorf("Unihiker(root 用户) remote_path 应在 /root 下,got %q", got)
	}
}

func TestToolchainAvailableMatchesDetailedToolchainNames(t *testing.T) {
	toolchains := []toolchainReport{
		{Name: "ESP-IDF idf.py", Available: true},
		{Name: "PlatformIO", Available: true},
	}
	if !toolchainAvailable(toolchains, "ESP-IDF") {
		t.Fatal("ESP-IDF should match ESP-IDF idf.py")
	}
	if !toolchainAvailable(toolchains, "platformio") {
		t.Fatal("toolchain matching should be case-insensitive")
	}
	if toolchainAvailable([]toolchainReport{{Name: "ESP-IDF idf.py", Available: false}}, "ESP-IDF") {
		t.Fatal("unavailable toolchain should not match")
	}
}

func TestEvidenceRecordAppendsJSONLAndChecklist(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "evidence_demo",
		"platform":     "arduino",
		"board":        "uno",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	projectDir := "evidence_demo"
	out, err := runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "compile",
		"status":      "passed",
		"summary":     "Arduino compile passed with 0 errors.",
		"command":     "arduino-cli compile -b arduino:avr:uno evidence_demo",
		"output":      "Sketch uses 928 bytes (2%) of program storage space.",
		"port":        "/dev/cu.usbserial-0001",
	})
	if err != nil {
		t.Fatalf("runEvidenceRecord() error = %v\n%s", err, out)
	}
	for _, want := range []string{"hardware_evidence.jsonl", "hardware_checklist.md", "Arduino compile passed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("evidence output missing %q:\n%s", want, out)
		}
	}
	jsonl, err := os.ReadFile(filepath.Join(dir, projectDir, "tests", "hardware_evidence.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record evidenceRecord
	if err := json.Unmarshal(bytes.TrimSpace(jsonl), &record); err != nil {
		t.Fatalf("hardware_evidence.jsonl is not valid JSONL: %v\n%s", err, jsonl)
	}
	if record.Stage != "compile" || record.Status != "passed" || record.Platform != "arduino" || record.Board != "uno" {
		t.Fatalf("unexpected evidence record: %+v", record)
	}
	if record.ProjectFingerprint == "" || record.FingerprintFileCount == 0 {
		t.Fatalf("evidence record should include current project fingerprint: %+v", record)
	}
	checklist, err := os.ReadFile(filepath.Join(dir, projectDir, "tests", "hardware_checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 验证证据记录", "compile - passed", "/dev/cu.usbserial-0001", "Sketch uses 928 bytes"} {
		if !strings.Contains(string(checklist), want) {
			t.Fatalf("checklist missing %q:\n%s", want, checklist)
		}
	}
}

func TestEvidenceStatusDistinguishesLocalAndHardwareVerification(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	// #2-3:清空本进程的真实执行日志,避免跨测试污染;本测试自己模拟真实执行。
	hostExecMu.Lock()
	hostExecs = nil
	hostExecMu.Unlock()

	_, err = runScaffold(map[string]any{
		"project_name": "status_demo",
		"platform":     "arduino",
		"board":        "uno",
	})
	if err != nil {
		t.Fatalf("runScaffold() error = %v", err)
	}
	projectDir := "status_demo"
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "compile",
		"status":      "passed",
		"summary":     "compile passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "hardware_pending"`, `"device_upload"`, `"runtime_log"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("hardware pending status missing %q:\n%s", want, out)
		}
	}

	// 失败的烧录执行不能支撑 passed 的 upload 证据——曾有通路:烧录失败后模型记一条
	// status=passed 就被判为有真实支撑。
	recordHostExecution("A fatal error occurred: Failed to connect to ESP32\n", false, "upload")
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "upload",
		"status":      "passed",
		"summary":     "claimed upload passed but the only real upload failed",
		"command":     "arduino-cli upload -p /dev/cu.fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"status": "hardware_pending"`) {
		t.Fatalf("passed upload evidence backed only by a FAILED real upload must not verify:\n%s", out)
	}

	// #2-3:upload 证据要求"真发生过一次成功的烧录执行"——模拟 arduino_upload 真跑成功。
	recordHostExecution("Writing at 0x00010000... (100 %)\nHash of data verified.\n", true, "upload")
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "upload",
		"status":      "passed",
		"summary":     "upload passed",
		"command":     "arduino-cli upload -p /dev/cu.fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "monitor",
		"status":      "passed",
		"summary":     "monitor passed but no serial output was pasted",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "hardware_pending"`, `"runtime_log"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("empty monitor output should not verify runtime log, missing %q:\n%s", want, out)
		}
	}
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "monitor",
		"status":      "passed",
		"summary":     "monitor only captured a command header",
		"output":      "$ arduino-cli monitor -p /dev/cu.fake\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "hardware_pending"`, `"runtime_log"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("command-header-only monitor output should not verify runtime log, missing %q:\n%s", want, out)
		}
	}
	// #2-3:伪造的串口输出(没有任何真实 monitor 执行支撑)即便有内容,也不能验证 runtime_log。
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "monitor",
		"status":      "passed",
		"summary":     "fabricated serial output not backed by any real run",
		"output":      "totally\nmade\nup\nserial\noutput\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "hardware_pending"`, `"runtime_log"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("fabricated serial output must not verify runtime log, missing %q:\n%s", want, out)
		}
	}

	// 真实 monitor 执行 + 如实粘贴其输出 → runtime_log 验证通过 → hardware_verified。
	recordHostExecution("ready\nled:on\nled:off\n", true, "monitor", "serial")
	_, err = runEvidenceRecord(map[string]any{
		"project_dir": projectDir,
		"stage":       "monitor",
		"status":      "passed",
		"summary":     "monitor produced serial output",
		"output":      "ready\nled:on\nled:off\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "hardware_verified"`, `"missingGroups": []`, "real-hardware stages are recorded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hardware verified status missing %q:\n%s", want, out)
		}
	}

	sketch := filepath.Join(dir, projectDir, projectDir+".ino")
	if err := os.WriteFile(sketch, []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runEvidenceStatus(map[string]any{"project_dir": projectDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "stale"`, `"currentRecordCount": 0`, `"staleRecordCount": 7`, "project fingerprint changed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stale status missing %q:\n%s", want, out)
		}
	}
}

func TestESPIDFMCPConfigUsesWorkspaceEnvForEIM(t *testing.T) {
	fakeBin := t.TempDir()
	fakeName := "eim"
	if runtime.GOOS == "windows" {
		fakeName = "eim.exe"
	}
	fakeEIM := filepath.Join(fakeBin, fakeName)
	if err := os.WriteFile(fakeEIM, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	t.Setenv("REASONIX_EIM", fakeEIM)
	t.Setenv("REASONIX_DISABLE_LOCAL_ESP_IDF", "1")

	out, err := runESPIDFMCPConfig(map[string]any{
		"project_dir": "/tmp/idf-demo",
		"use_eim":     true,
	})
	if err != nil {
		t.Fatalf("runESPIDFMCPConfig() error = %v", err)
	}
	for _, want := range []string{
		jsonPathFragment(fakeEIM),
		`IDF_MCP_WORKSPACE_FOLDER`,
		`idf.py mcp-server`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config output missing %q:\n%s", want, out)
		}
	}
}

func TestHardwareDetectUsesEmptyArraysInsteadOfNull(t *testing.T) {
	dir := t.TempDir()
	out, err := runHardwareDetect(map[string]any{"project_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"recommendations": null`) || strings.Contains(out, `"serialPorts": null`) {
		t.Fatalf("hardware_detect should not return null arrays:\n%s", out)
	}
}

func TestDetectProjectTypesRequiresArduinoSketchRoot(t *testing.T) {
	dir := t.TempDir()
	sketchDir := filepath.Join(dir, "blink_demo")
	if err := os.MkdirAll(sketchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sketchDir, "blink_demo.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if types := detectProjectTypes(dir); contains(types, "arduino") {
		t.Fatalf("detectProjectTypes(parent) = %+v, should not treat nested sketch as the active Arduino project", types)
	}
	if types := detectProjectTypes(sketchDir); !contains(types, "arduino") {
		t.Fatalf("detectProjectTypes(sketch) = %+v, want arduino", types)
	}
	candidates := detectHardwareProjectCandidates(dir)
	if len(candidates) != 1 || candidates[0].Dir != sketchDir || candidates[0].Kind != "arduino" {
		t.Fatalf("detectHardwareProjectCandidates() = %+v, want nested Arduino sketch candidate", candidates)
	}
}

func TestValidateArduinoProjectRejectsParentDirectory(t *testing.T) {
	dir := t.TempDir()
	sketchDir := filepath.Join(dir, "blink_demo")
	if err := os.MkdirAll(sketchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sketchDir, "blink_demo.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results := validateArduinoProject(dir, nil, time.Second)
	if len(results) != 1 {
		t.Fatalf("validateArduinoProject() returned %d results, want 1", len(results))
	}
	if results[0].Kind != "arduino_project_boundary" || results[0].Status != "failed" {
		t.Fatalf("validateArduinoProject() = %+v, want boundary failure", results[0])
	}
	if !strings.Contains(results[0].NextStep, "blink_demo") {
		t.Fatalf("NextStep should mention candidate sketch dir, got %q", results[0].NextStep)
	}
}

func TestDetectProjectTypesRequiresPythonEntrypoint(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "tools", "scripts")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "helper.py"), []byte("print('not a hardware entrypoint')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if types := detectProjectTypes(dir); contains(types, "python_or_micropython") {
		t.Fatalf("detectProjectTypes() = %+v, should not treat arbitrary nested Python as a hardware project", types)
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.py"), []byte("print('hardware entrypoint')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if types := detectProjectTypes(dir); !contains(types, "python_or_micropython") {
		t.Fatalf("detectProjectTypes() = %+v, want python_or_micropython", types)
	}
}

func TestIsLikelyHardwareSerialPortFiltersSystemPorts(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/dev/cu.Bluetooth-Incoming-Port", false},
		{"/dev/cu.debug-console", false},
		{"/dev/cu.usbserial-0001", true},
		{"/dev/cu.SLAB_USBtoUART", true},
		{"/dev/ttyACM0", true},
	}
	for _, tc := range cases {
		if got := isLikelyHardwareSerialPort(tc.path); got != tc.want {
			t.Fatalf("isLikelyHardwareSerialPort(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestParseArduinoBoardListFiltersSystemPortsAndMapsFQBN(t *testing.T) {
	body := []byte(`{
  "detected_ports": [
    {
      "port": {"address": "/dev/cu.Bluetooth-Incoming-Port", "protocol": "serial", "properties": {}}
    },
    {
      "port": {"address": "/dev/cu.usbserial-0001", "protocol": "serial", "properties": {"pid": "EA60", "vid": "10C4"}},
      "matching_boards": [
        {"name": "ESP32 Dev Module", "fqbn": "esp32:esp32:esp32"}
      ]
    }
  ]
}`)
	boards := parseArduinoBoardList(body)
	if len(boards) != 1 {
		t.Fatalf("boards = %+v, want 1 hardware board", boards)
	}
	got := boards[0]
	if got.Port != "/dev/cu.usbserial-0001" || got.BoardName != "ESP32 Dev Module" || got.FQBN != "esp32:esp32:esp32" || got.Core != "esp32:esp32" {
		t.Fatalf("unexpected board mapping: %+v", got)
	}
	if !strings.Contains(got.Properties, "pid=EA60") || !strings.Contains(got.Properties, "vid=10C4") {
		t.Fatalf("properties not compacted: %+v", got)
	}
}

func TestParsePlatformIODeviceListFiltersSystemPorts(t *testing.T) {
	body := []byte(`[
  {"port": "/dev/cu.debug-console", "description": "n/a", "hwid": "n/a"},
  {"port": "/dev/cu.usbmodem1101", "description": "ESP32-S3 USB", "hwid": "USB VID:PID=303A:1001"}
]`)
	devices := parsePlatformIODeviceList(body)
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want 1 hardware device", devices)
	}
	got := devices[0]
	if got.Port != "/dev/cu.usbmodem1101" || got.Description != "ESP32-S3 USB" || got.HWID != "USB VID:PID=303A:1001" {
		t.Fatalf("unexpected device mapping: %+v", got)
	}
}

func TestProjectValidateArduinoRequiresSketchRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommand(t, fakeBin, "arduino-cli")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	sketchDir := filepath.Join(dir, "validate_demo")
	if err := os.MkdirAll(sketchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sketchDir, "validate_demo.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parentOut, err := runProjectValidate(map[string]any{"project_dir": dir, "platform": "arduino", "fqbn": "arduino:avr:uno"})
	if err == nil {
		t.Fatalf("parent directory validation should fail, got:\n%s", parentOut)
	}
	for _, want := range []string{`"kind": "arduino_project_boundary"`, `"status": "failed"`, "validate_demo"} {
		if !strings.Contains(parentOut, want) {
			t.Fatalf("parent validation output missing %q:\n%s", want, parentOut)
		}
	}

	out, err := runProjectValidate(map[string]any{"project_dir": sketchDir, "platform": "arduino", "fqbn": "arduino:avr:uno"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "arduino"`, `"status": "passed"`, sketchDir, `arduino-cli compile -b arduino:avr:uno`} {
		if !strings.Contains(out, want) {
			t.Fatalf("validation output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectAuditFlagsPlatformIORootINOWithoutSrcEntrypoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "platformio.ini"), []byte("[env:esp32dev]\nplatform = espressif32\nboard = esp32dev\nframework = arduino\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "esp32_snake_web.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runProjectAudit(map[string]any{"project_dir": dir, "platform": "platformio"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "project_layout"`, `"status": "failed"`, "根目录 .ino", "src/main.cpp", "esp32_snake_web.ino"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectValidateStopsOnPlatformIORootINOLayout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "platformio.ini"), []byte("[env:esp32dev]\nplatform = espressif32\nboard = esp32dev\nframework = arduino\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "esp32_snake_web.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runProjectValidate(map[string]any{"project_dir": dir, "platform": "platformio"})
	// #2-2:验证失败时必须返回 error(让 agent 把收据记为 Success=false,失败的验证不能被
	// 当成成功签收);报告文本仍随 out 返回供模型看。
	if err == nil {
		t.Fatal("失败的 validate 应返回 error,否则编译失败会被吞成成功收据")
	}
	for _, want := range []string{`"kind": "platformio_layout"`, `"status": "failed"`, "Root .ino files found", "src/main.cpp", "重新运行 hardware_project_validate"} {
		if !strings.Contains(out, want) {
			t.Fatalf("validation output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectRepairDryRunPlatformIORootINO(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, "snake")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "platformio.ini"), []byte("[env:esp32dev]\nplatform = espressif32\nboard = esp32dev\nframework = arduino\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "snake.ino"), []byte("#include \"index_html.h\"\nvoid setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "index_html.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	out, err := runProjectRepair(map[string]any{"project_dir": projectDir, "repair": "platformio_root_ino_to_src_main", "dry_run": true, "allow_outside_workspace": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"dryRun": true`, `"applied": false`, jsonPathFragment("src/main.cpp"), ".onecreat-backup", jsonPathFragment("include/index_html.h")} {
		if !strings.Contains(out, want) {
			t.Fatalf("repair dry-run output missing %q:\n%s", want, out)
		}
	}
	if exists(filepath.Join(projectDir, "src", "main.cpp")) {
		t.Fatalf("dry-run should not create src/main.cpp")
	}
}

func TestProjectRepairAppliesPlatformIORootINO(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, "snake")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "platformio.ini"), []byte("[env:esp32dev]\nplatform = espressif32\nboard = esp32dev\nframework = arduino\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "snake.ino"), []byte("#include \"index_html.h\"\nvoid setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "index_html.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	out, err := runProjectRepair(map[string]any{"project_dir": projectDir, "repair": "platformio_root_ino_to_src_main", "allow_outside_workspace": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"applied": true`, "src/main.cpp", ".onecreat-backup"} {
		if !strings.Contains(out, want) {
			t.Fatalf("repair output missing %q:\n%s", want, out)
		}
	}
	mainCPP, err := os.ReadFile(filepath.Join(projectDir, "src", "main.cpp"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainCPP), "#include <Arduino.h>") || !strings.Contains(string(mainCPP), "void setup()") {
		t.Fatalf("unexpected src/main.cpp:\n%s", mainCPP)
	}
	if !exists(filepath.Join(projectDir, "include", "index_html.h")) {
		t.Fatalf("referenced root header should be copied to include/")
	}
	if exists(filepath.Join(projectDir, "snake.ino")) {
		t.Fatalf("root .ino should be moved out of the active PlatformIO entrypoint set")
	}
	audit, err := runProjectAudit(map[string]any{"project_dir": projectDir, "platform": "platformio"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audit, "PlatformIO 项目结构可编译") || strings.Contains(audit, "根目录 .ino 不会被当前 PlatformIO 构建当作入口") {
		t.Fatalf("project_layout should no longer fail after repair:\n%s", audit)
	}
}

func TestProjectValidatePythonSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommand(t, fakeBin, "python3")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectValidate(map[string]any{"project_dir": dir, "platform": "micropython"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "python_syntax"`, `"status": "passed"`, `python3 -m py_compile`} {
		if !strings.Contains(out, want) {
			t.Fatalf("validation output missing %q:\n%s", want, out)
		}
	}
	// py_compile 通过时必须诚实降级：明确指向真机验证，不能让学生把「语法过」当「已验证」。
	for _, want := range []string{"语法过 ≠ 能跑", "ssh_deploy_run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("py_compile 通过时缺少诚实降级提示 %q:\n%s", want, out)
		}
	}
}

func TestProjectValidateESPIDFSkipsWhenIDFMissing(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)
	t.Setenv("REASONIX_DISABLE_LOCAL_ESP_IDF", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"), []byte("project(demo)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainDir := filepath.Join(dir, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "CMakeLists.txt"), []byte("idf_component_register(SRCS \"main.c\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectValidate(map[string]any{"project_dir": dir, "platform": "esp_idf"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "esp_idf"`, `"status": "skipped"`, `未找到可用 ESP-IDF`} {
		if !strings.Contains(out, want) {
			t.Fatalf("validation output missing %q:\n%s", want, out)
		}
	}
}

func TestESPIDFLocalEnvFallbackRunsWithoutIDFOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fallback uses bash")
	}
	workspace := t.TempDir()
	idfPath, fakePython, pythonEnv := writeFakeLocalESPIDF(t, workspace)
	t.Setenv("PATH", "/bin:/usr/bin")
	t.Setenv("REASONIX_ESP_IDF_PATH", idfPath)
	t.Setenv("REASONIX_IDF_PYTHON", fakePython)
	t.Setenv("REASONIX_IDF_PYTHON_ENV_PATH", pythonEnv)
	t.Setenv("REASONIX_DYLD_LIBRARY_PATH", "/opt/fake-expat")

	projectDir := t.TempDir()
	out, err := runESPIDF(map[string]any{"project_dir": projectDir, "action": "build"})
	if err != nil {
		t.Fatalf("runESPIDF() error = %v\n%s", err, out)
	}
	for _, want := range []string{"$ bash -lc", "source", "export IDF_PYTHON_ENV_PATH=", "export DYLD_LIBRARY_PATH=", filepath.Join(idfPath, "tools", "idf.py"), "-C " + projectDir, "build"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestESPIDFMCPConfigUsesLocalWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fallback uses bash")
	}
	workspace := t.TempDir()
	idfPath, fakePython, pythonEnv := writeFakeLocalESPIDF(t, workspace)
	t.Setenv("PATH", "/bin:/usr/bin")
	t.Setenv("REASONIX_ESP_IDF_PATH", idfPath)
	t.Setenv("REASONIX_IDF_PYTHON", fakePython)
	t.Setenv("REASONIX_IDF_PYTHON_ENV_PATH", pythonEnv)

	out, err := runESPIDFMCPConfig(map[string]any{"project_dir": workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"localEspIdf": true`, `command = \"bash\"`, `mcp-server`, filepath.Join(idfPath, "tools", "idf.py")} {
		if !strings.Contains(out, want) {
			t.Fatalf("config output missing %q:\n%s", want, out)
		}
	}
}

func TestToolCommandsUseExpectedCLIForms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	for _, name := range []string{"arduino-cli", "pio", "idf.py", "mpremote", "scp", "ssh"} {
		writeFakeCommand(t, fakeBin, name)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	projectDir := t.TempDir()
	script := filepath.Join(projectDir, "main.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		run  func() (string, error)
		want []string
	}{
		{
			name: "arduino compile",
			run: func() (string, error) {
				return runArduinoCompile(map[string]any{"sketch_dir": projectDir, "fqbn": "arduino:avr:uno", "port": "/dev/cu.fake"})
			},
			want: []string{"$ arduino-cli compile -b arduino:avr:uno -p /dev/cu.fake", projectDir},
		},
		{
			name: "platformio upload monitor",
			run: func() (string, error) {
				return runPlatformIO(map[string]any{
					"project_dir":  projectDir,
					"environment":  "esp32dev",
					"targets":      []any{"upload", "monitor"},
					"upload_port":  "/dev/cu.fake",
					"monitor_port": "/dev/cu.fake",
				})
			},
			want: []string{"$ pio run -d", "-e esp32dev", "-t upload", "-t monitor", "--upload-port /dev/cu.fake", "--monitor-port /dev/cu.fake"},
		},
		{
			name: "esp-idf flash monitor",
			run: func() (string, error) {
				return runESPIDF(map[string]any{"project_dir": projectDir, "action": "flash_monitor", "port": "/dev/cu.fake", "baud": 115200})
			},
			want: []string{"$ idf.py -C", "-p /dev/cu.fake", "-b 115200", "flash monitor"},
		},
		{
			name: "mpremote run",
			run: func() (string, error) {
				return runMPRemote(map[string]any{"script": script, "device": "auto"})
			},
			want: []string{"$ mpremote connect auto run", script},
		},
		{
			name: "ssh deploy",
			run: func() (string, error) {
				return runSSHDeploy(map[string]any{
					"host":            "10.1.2.3",
					"user":            "root",
					"ssh_port":        2222,
					"identity_file":   "/tmp/reasonix-test-key",
					"connect_timeout": 3,
					"local_path":      script,
					"remote_path":     "/root/demo/main.py",
					"command":         "python3 /root/demo/main.py",
				})
			},
			want: []string{
				"$ scp",
				"-o BatchMode=yes",
				"-o StrictHostKeyChecking=accept-new",
				"-o ConnectTimeout=3",
				"-i /tmp/reasonix-test-key",
				"-P 2222",
				"root@10.1.2.3:/root/demo/main.py",
				"--- remote command ---",
				"$ ssh",
				"-p 2222",
				"root@10.1.2.3",
				"python3 /root/demo/main.py",
			},
		},
	}

	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			out, err := check.run()
			if err != nil {
				t.Fatalf("command failed: %v\n%s", err, out)
			}
			for _, want := range check.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestSSHCommonArgsDefaultsAreNonInteractive(t *testing.T) {
	got := strings.Join(sshCommonArgs(0, "", true), " ")
	for _, want := range []string{
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=accept-new",
		"-o UserKnownHostsFile=", // 不写 ~/.ssh(沙箱会拒),固定走临时目录
		"-o ConnectTimeout=8",
		"-o ServerAliveInterval=5",
		"-o ServerAliveCountMax=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sshCommonArgs missing %q: %s", want, got)
		}
	}
	// 密码认证模式(batch=false)绝不能带 BatchMode,否则 sshpass 也救不了
	pw := strings.Join(sshCommonArgs(0, "", false), " ")
	if strings.Contains(pw, "BatchMode") {
		t.Fatalf("password mode must not set BatchMode: %s", pw)
	}
}

func TestCallToolErrorIncludesCommandOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "arduino-cli", "#!/bin/sh\nprintf 'upload failed: missing port\\n' >&2\nexit 7\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	params, err := json.Marshal(map[string]any{
		"name": "arduino_upload",
		"arguments": map[string]any{
			"sketch_dir": dir,
			"fqbn":       "arduino:avr:uno",
			"port":       "/dev/cu.fake",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := callTool(params)
	if rpcErr != nil {
		t.Fatalf("callTool RPC error = %+v", rpcErr)
	}
	body := toolResultText(t, result)
	for _, want := range []string{"$ arduino-cli upload", "upload failed: missing port", "error: arduino-cli failed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("error result missing %q:\n%s", want, body)
		}
	}
}

func TestArduinoMonitorTimeoutWithoutOutputIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "arduino-cli", "#!/bin/sh\nsleep 5\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runArduinoMonitor(map[string]any{"port": "/dev/cu.fake", "seconds": 1})
	if err == nil {
		t.Fatalf("runArduinoMonitor should fail when it times out without serial output:\n%s", out)
	}
	// 跑满采样窗口仍无串口输出 -> 返回明确的“没采到输出”硬件侧排查指引(而不是干瘪的
	// timeout);命令头要带上,方便排查。stdin 撑住后 monitor 不再秒退,这是超时分支。
	if !strings.Contains(err.Error(), "串口没采到输出") || !strings.Contains(out, "$ arduino-cli monitor") {
		t.Fatalf("unexpected monitor timeout result: err=%v out=%s", err, out)
	}
}

func TestArduinoMonitorNoOutputIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "arduino-cli", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runArduinoMonitor(map[string]any{"port": "/dev/cu.fake", "seconds": 1})
	if err == nil {
		t.Fatalf("runArduinoMonitor should fail when monitor exits without serial output:\n%s", out)
	}
	// 没采到输出的指引必须:点明串口无输出、明确劝阻 bash 瞎试(screen 等)、
	// 且仍保留原始命令行供排查。
	msg := err.Error()
	if !strings.Contains(msg, "串口") || !strings.Contains(msg, "screen") || !strings.Contains(msg, "不要") {
		t.Fatalf("no-output guidance should warn against bash retries: %v", msg)
	}
	if !strings.Contains(out, "$ arduino-cli monitor") {
		t.Fatalf("original monitor command line should be preserved: %s", out)
	}
}

func TestPlatformIOMonitorNoOutputIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "pio", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runPlatformIO(map[string]any{
		"project_dir": t.TempDir(),
		"targets":     []any{"monitor"},
	})
	if err == nil {
		t.Fatalf("runPlatformIO monitor should fail when it exits without runtime output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no runtime output") || !strings.Contains(out, "$ pio run") {
		t.Fatalf("unexpected platformio monitor result: err=%v out=%s", err, out)
	}
}

func TestESPIDFMonitorNoOutputIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "idf.py", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REASONIX_DISABLE_LOCAL_ESP_IDF", "1")

	out, err := runESPIDF(map[string]any{
		"project_dir": t.TempDir(),
		"action":      "monitor",
		"port":        "/dev/cu.fake",
	})
	if err == nil {
		t.Fatalf("runESPIDF monitor should fail when it exits without runtime output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no runtime output") || !strings.Contains(out, "$ idf.py") {
		t.Fatalf("unexpected ESP-IDF monitor result: err=%v out=%s", err, out)
	}
}

func TestMPRemoteNoOutputIsErrorByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "mpremote", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	projectDir := t.TempDir()
	script := filepath.Join(projectDir, "main.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runMPRemote(map[string]any{"script": script, "device": "auto"})
	if err == nil {
		t.Fatalf("runMPRemote should fail when it exits without runtime output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no runtime output") || !strings.Contains(out, "$ mpremote connect auto run") {
		t.Fatalf("unexpected mpremote no-output result: err=%v out=%s", err, out)
	}

	out, err = runMPRemote(map[string]any{"script": script, "device": "auto", "require_output": false})
	if err != nil {
		t.Fatalf("require_output=false should allow output-free mpremote runs: %v\n%s", err, out)
	}
}

func TestSSHRemoteNoOutputIsErrorByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake commands are only used on Unix test hosts")
	}
	fakeBin := t.TempDir()
	writeFakeCommandBody(t, fakeBin, "scp", "#!/bin/sh\nexit 0\n")
	writeFakeCommandBody(t, fakeBin, "ssh", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	projectDir := t.TempDir()
	script := filepath.Join(projectDir, "main.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{
		"host":        "10.1.2.3",
		"user":        "root",
		"local_path":  script,
		"remote_path": "/root/demo/main.py",
		"command":     "python3 /root/demo/main.py",
	}
	out, err := runSSHDeploy(args)
	if err == nil {
		t.Fatalf("runSSHDeploy should fail when remote command exits without runtime output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no runtime output") || !strings.Contains(out, "--- remote command ---") || !strings.Contains(out, "$ ssh") {
		t.Fatalf("unexpected ssh no-output result: err=%v out=%s", err, out)
	}

	args["require_output"] = false
	out, err = runSSHDeploy(args)
	if err != nil {
		t.Fatalf("require_output=false should allow output-free SSH commands: %v\n%s", err, out)
	}
}

func writeFakeCommand(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf 'fake command: %s\\n' \"$0\"\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFakeCommandBody(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func toolResultText(t *testing.T, result any) string {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.IsError {
		t.Fatalf("result should be marked as error: %s", body)
	}
	if len(parsed.Content) == 0 {
		t.Fatalf("result content is empty: %s", body)
	}
	return parsed.Content[0].Text
}

func writeFakeLocalESPIDF(t *testing.T, workspace string) (idfPath, fakePython, pythonEnv string) {
	t.Helper()
	idfPath = filepath.Join(workspace, "tools", "esp", "v6.0", "esp-idf")
	toolsDir := filepath.Join(idfPath, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exportBody := "export IDF_PATH=" + shellQuote(idfPath) + "\n"
	if err := os.WriteFile(filepath.Join(idfPath, "export.sh"), []byte(exportBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "idf.py"), []byte("# fake idf.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakePython = filepath.Join(workspace, "fake-python")
	pythonBody := "#!/bin/sh\nprintf 'fake local idf python: %s\\n' \"$*\"\n"
	if err := os.WriteFile(fakePython, []byte(pythonBody), 0o755); err != nil {
		t.Fatal(err)
	}
	pythonEnv = filepath.Join(workspace, "venv")
	if err := os.MkdirAll(filepath.Join(pythonEnv, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return idfPath, fakePython, pythonEnv
}

func TestScaffoldRefusesOutsideWorkspaceByDefault(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	_, err = runScaffold(map[string]any{
		"project_name": "outside_demo",
		"platform":     "arduino",
		"project_dir":  filepath.Join(outside, "outside_demo"),
	})
	if err == nil {
		t.Fatal("runScaffold() should refuse writes outside workspace by default")
	}
}
