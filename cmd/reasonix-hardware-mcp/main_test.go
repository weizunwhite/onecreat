package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
	for _, want := range []string{`"projectName": "legacy_snake"`, `"platform": "platformio"`, `"board": "esp32dev"`, `"status": "skipped"`, "docs/board_profile.md", "docs/failure_patterns.md", "hardware_manifest.json"} {
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
	for _, want := range []string{`"status": "stale"`, `"currentRecordCount": 0`, `"staleRecordCount": 5`, "project fingerprint changed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stale status missing %q:\n%s", want, out)
		}
	}
}

func TestESPIDFMCPConfigUsesWorkspaceEnvForEIM(t *testing.T) {
	fakeBin := t.TempDir()
	fakeEIM := filepath.Join(fakeBin, "eim")
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
		fakeEIM,
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

func TestDetectProjectTypesFindsArduinoSketchSubdir(t *testing.T) {
	dir := t.TempDir()
	sketchDir := filepath.Join(dir, "blink_demo")
	if err := os.MkdirAll(sketchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sketchDir, "blink_demo.ino"), []byte("void setup() {}\nvoid loop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	types := detectProjectTypes(dir)
	if !contains(types, "arduino") {
		t.Fatalf("detectProjectTypes() = %+v, want arduino", types)
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

func TestProjectValidateArduinoUsesNestedSketchDir(t *testing.T) {
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
	out, err := runProjectValidate(map[string]any{"project_dir": dir, "platform": "arduino", "fqbn": "arduino:avr:uno"})
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
	if err != nil {
		t.Fatal(err)
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
	for _, want := range []string{`"dryRun": true`, `"applied": false`, "src/main.cpp", ".onecreat-backup", "include/index_html.h"} {
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
	got := strings.Join(sshCommonArgs(0, ""), " ")
	for _, want := range []string{
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=accept-new",
		"-o ConnectTimeout=8",
		"-o ServerAliveInterval=5",
		"-o ServerAliveCountMax=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sshCommonArgs missing %q: %s", want, got)
		}
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
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(out, "$ arduino-cli monitor") {
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
