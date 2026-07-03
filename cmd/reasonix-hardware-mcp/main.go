// Command reasonix-hardware-mcp exposes a first-pass hardware programming MCP
// server for Reasonix. It wraps common embedded toolchains with explicit,
// structured tools so the agent does not have to invent shell commands for
// board detection, project scaffolding, builds, flashing, and short log capture.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/hardware/boards"
)

var version = "dev"

const (
	protocolVersion    = "2024-11-05"
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	maxCommandOutput   = 48 * 1024
	defaultTimeout     = 90 * time.Second
	defaultLogSeconds  = 8
)

func main() {
	log.SetPrefix("reasonix-hardware-mcp: ")
	log.SetFlags(0)
	ensureManagedToolsOnPath() // 让本应用一键安装到用户目录的 arduino-cli 等工具能被找到
	if err := serve(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// --- JSON-RPC framing ---

type request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serve(in *os.File, out *os.File) error {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	defer w.Flush()

	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if rerr := handleLine(bytes.TrimSpace(line), w); rerr != nil {
				return rerr
			}
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			return nil
		}
	}
}

func handleLine(line []byte, w *bufio.Writer) error {
	if len(line) == 0 {
		return nil
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("skipping unparseable line: %v", err)
		return nil
	}
	if req.ID == nil {
		return nil
	}

	resp := response{JSONRPC: "2.0", ID: *req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"prompts":   map[string]any{},
				"resources": map[string]any{},
			},
			"serverInfo": map[string]any{"name": "reasonix-hardware-mcp", "version": version},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		resp.Result, resp.Error = callTool(req.Params)
	case "prompts/list":
		resp.Result = map[string]any{"prompts": promptList()}
	case "prompts/get":
		resp.Result, resp.Error = getPrompt(req.Params)
	case "resources/list":
		resp.Result = map[string]any{"resources": resourceList()}
	case "resources/read":
		resp.Result, resp.Error = readResource(req.Params)
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}

	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// --- tools ---

type toolDef struct {
	name        string
	description string
	schema      map[string]any
	readOnly    bool
	run         func(args map[string]any) (string, error)
}

var tools = []toolDef{
	{
		name:        "hardware_detect",
		description: "Detect local hardware programming toolchains, serial ports, and likely project type. Use before compiling, flashing, or choosing a scaffold.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"project_dir": map[string]any{"type": "string", "description": "Project directory to inspect. Defaults to current workspace."},
		}, nil),
		run: runHardwareDetect,
	},
	{
		name:        "hardware_board_profile",
		description: "Return structured board knowledge for AI hardware programming: safe pins, risky pins, default frameworks, toolchains, upload flow, and teaching notes.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"board":    map[string]any{"type": "string", "description": "Board name or alias, e.g. esp32dev, esp32_arduino, uno, nano, unihiker, maixcam, raspberry_pi."},
			"platform": map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform hint."},
			"all":      map[string]any{"type": "boolean", "description": "Return every built-in board profile summary. Defaults to false."},
		}, nil),
		run: runBoardProfile,
	},
	{
		name:        "hardware_module_spec",
		description: "Return verified ground-truth facts for hardware modules/components (library names, includes, default pins per board, I2C address, voltage, gotchas) and niche platform APIs (MaixCAM K230 MaixPy, Unihiker pinpong). Use this BEFORE writing module driver code so the model does not guess wrong libraries, pins, or APIs.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"modules":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Module names or aliases, e.g. [\"DHT11\", \"SSD1306\", \"SG90\", \"MaixCAM\", \"unihiker\"]. Omit to list every available module."},
			"board":    map[string]any{"type": "string", "description": "Target board, e.g. esp32dev, esp32s3, uno, nano. Filters default pins and resolves board-specific libraries (e.g. ESP32Servo vs Servo)."},
			"platform": map[string]any{"type": "string", "description": "Optional platform hint (arduino/platformio) to select the matching library names."},
		}, nil),
		run: runModuleSpec,
	},
	{
		name:        "hardware_project_scaffold",
		description: "Create a teaching-friendly hardware project scaffold for Arduino, PlatformIO, ESP-IDF, MicroPython, Unihiker, MaixCAM, or Raspberry Pi Python.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_name": map[string]any{"type": "string", "description": "English directory/project name, e.g. smart_trash_bin."},
			"project_dir":  map[string]any{"type": "string", "description": "Output directory. Defaults to ./<project_name>."},
			"platform": map[string]any{
				"type": "string",
				"enum": []string{"arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"},
			},
			"board":                   map[string]any{"type": "string", "description": "Board name/id, e.g. uno, esp32dev, esp32s3, esp32c3, unihiker, maixcam."},
			"modules":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Hardware modules the student wants in this project (e.g. [\"DHT11\", \"SSD1306\", \"SG90\"]). They become real connections in hardware_manifest.json and trigger docs/module_specs.md to embed verified libraries, pins, and gotchas. Highly recommended — replaces the default placeholder \"on-board LED\" connections."},
			"overwrite":               map[string]any{"type": "boolean", "description": "Overwrite existing files created by the scaffold."},
			"allow_outside_workspace": map[string]any{"type": "boolean", "description": "Allow writing outside the plugin working directory. Defaults to false."},
		}, []string{"project_name", "platform"}),
		run: runScaffold,
	},
	{
		name:        "hardware_project_context",
		description: "Create or refresh the standard AI-readable hardware project context files for an existing project: manifest, wiring guide, verification guide, board profile, failure patterns, and evidence checklist.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":             map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"project_name":            map[string]any{"type": "string", "description": "Project name used in generated docs. Defaults to manifest or directory name."},
			"platform":                map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Project platform. Defaults to manifest or auto-detect."},
			"board":                   map[string]any{"type": "string", "description": "Board name/id. Defaults to manifest or platform default."},
			"dry_run":                 map[string]any{"type": "boolean", "description": "Preview files without writing. Defaults to false."},
			"overwrite":               map[string]any{"type": "boolean", "description": "Overwrite existing context files. Defaults to false."},
			"allow_outside_workspace": map[string]any{"type": "boolean", "description": "Allow writing outside the plugin working directory. Defaults to false."},
		}, nil),
		run: runProjectContext,
	},
	{
		name:        "hardware_project_validate",
		description: "Automatically validate a hardware project without flashing: Arduino compile, PlatformIO build, ESP-IDF build when idf.py is available, and Python/MicroPython syntax checks.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":     map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"platform":        map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform override. Defaults to auto-detect."},
			"board":           map[string]any{"type": "string", "description": "Board name used for defaults, e.g. uno, nano, esp32dev, esp32s3."},
			"fqbn":            map[string]any{"type": "string", "description": "Arduino FQBN override, e.g. arduino:avr:uno."},
			"environment":     map[string]any{"type": "string", "description": "PlatformIO environment override, e.g. esp32dev."},
			"target":          map[string]any{"type": "string", "description": "ESP-IDF target for set-target before build, e.g. esp32s3."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Per-command timeout. Defaults to 90."},
		}, nil),
		run: runProjectValidate,
	},
	{
		name:        "hardware_project_audit",
		description: "Audit a hardware project for complete AI-readable project context: manifest, wiring guide, verification guide, hardware checklist, entrypoints, and platform consistency.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"project_dir": map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"platform":    map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional expected platform. Defaults to manifest or auto-detect."},
		}, nil),
		run: runProjectAudit,
	},
	{
		name:        "hardware_project_repair",
		description: "Apply narrow, reversible repairs for known hardware project layout issues. Currently supports migrating a root Arduino .ino into a PlatformIO src/main.cpp entrypoint with backups.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":             map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"repair":                  map[string]any{"type": "string", "enum": []string{"platformio_root_ino_to_src_main"}, "description": "Repair to apply."},
			"source_ino":              map[string]any{"type": "string", "description": "Optional root .ino file to migrate when several exist."},
			"dry_run":                 map[string]any{"type": "boolean", "description": "Preview actions without writing files. Defaults to false."},
			"overwrite":               map[string]any{"type": "boolean", "description": "Overwrite src/main.cpp if it already exists. Defaults to false."},
			"allow_outside_workspace": map[string]any{"type": "boolean", "description": "Allow writing outside the plugin working directory. Defaults to false."},
		}, []string{"repair"}),
		run: runProjectRepair,
	},
	{
		name:        "hardware_repair_catalog",
		description: "List known hardware failure patterns, which ones can be auto-repaired, and the exact next-step tools to run. Use after audit/validate failures before editing code.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"platform": map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform filter."},
		}, nil),
		run: runRepairCatalog,
	},
	{
		name:        "hardware_evidence_record",
		description: "Record hardware verification evidence into tests/hardware_evidence.jsonl and tests/hardware_checklist.md after compile, upload, flash, monitor, mpremote, or SSH deployment. 真机执行类阶段(upload/flash/monitor/serial/mpremote/ssh/deploy)的证据会与本会话里真实跑过的对应 MCP 工具核对:`output` 必须如实粘贴该工具的真实输出,凭空编造的串口/运行日志对不上真实执行,不会被算作 hardware_verified。先用 arduino_monitor_sample / ssh_deploy_run / mpremote_run 等真跑一次,再把它的输出原样填进 output。",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":             map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"platform":                map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform. Defaults to manifest or auto-detect."},
			"board":                   map[string]any{"type": "string", "description": "Optional board name/id."},
			"stage":                   map[string]any{"type": "string", "enum": []string{"audit", "compile", "syntax", "upload", "flash", "monitor", "serial", "mpremote", "ssh", "deploy", "manual"}, "description": "Verification stage being recorded."},
			"status":                  map[string]any{"type": "string", "enum": []string{"passed", "failed", "skipped", "info"}, "description": "Evidence status."},
			"summary":                 map[string]any{"type": "string", "description": "Short human-readable result summary."},
			"command":                 map[string]any{"type": "string", "description": "Command or MCP tool call that produced the evidence."},
			"output":                  map[string]any{"type": "string", "description": "Relevant command output or serial log excerpt."},
			"port":                    map[string]any{"type": "string", "description": "Serial port, SSH host, or device identifier."},
			"artifact_path":           map[string]any{"type": "string", "description": "Optional screenshot/log/artifact path."},
			"allow_outside_workspace": map[string]any{"type": "boolean", "description": "Allow writing outside the plugin working directory. Defaults to false."},
		}, []string{"stage", "status", "summary"}),
		run: runEvidenceRecord,
	},
	{
		name:        "hardware_evidence_status",
		description: "Summarize recorded hardware verification evidence and report whether local validation, real-device upload/deploy, and runtime log checks are complete for the platform.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"project_dir": map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"platform":    map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform. Defaults to manifest or auto-detect."},
		}, nil),
		run: runEvidenceStatus,
	},
	{
		name:        "hardware_device_verify_plan",
		description: "Generate an exact real-device verification plan for a hardware project, including the lab runner command, missing inputs, and MCP tool sequence. Does not flash or modify files.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"project_dir":     map[string]any{"type": "string", "description": "Hardware project directory. Defaults to current workspace."},
			"platform":        map[string]any{"type": "string", "enum": []string{"auto", "arduino", "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"}, "description": "Optional platform override. Defaults to manifest or auto-detect."},
			"board":           map[string]any{"type": "string", "description": "Optional board override."},
			"fqbn":            map[string]any{"type": "string", "description": "Arduino FQBN override."},
			"environment":     map[string]any{"type": "string", "description": "PlatformIO environment override."},
			"target":          map[string]any{"type": "string", "description": "ESP-IDF target override."},
			"port":            map[string]any{"type": "string", "description": "Serial/upload/monitor port override."},
			"device":          map[string]any{"type": "string", "description": "MicroPython mpremote device override."},
			"host":            map[string]any{"type": "string", "description": "SSH host override for Unihiker, MaixCAM, or Raspberry Pi."},
			"user":            map[string]any{"type": "string", "description": "SSH user override."},
			"baud":            map[string]any{"type": "number", "description": "Serial baud rate. Defaults to 115200."},
			"monitor_seconds": map[string]any{"type": "number", "description": "Runtime log capture seconds. Defaults to 8."},
		}, nil),
		run: runDeviceVerifyPlan,
	},
	{
		name:        "arduino_compile",
		description: "Compile an Arduino sketch with arduino-cli. This only builds; use arduino_upload after a successful compile.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"sketch_dir":      map[string]any{"type": "string", "description": "Directory containing the .ino sketch."},
			"fqbn":            map[string]any{"type": "string", "description": "Fully qualified board name, e.g. arduino:avr:uno."},
			"port":            map[string]any{"type": "string", "description": "Optional serial/upload port."},
			"export_binaries": map[string]any{"type": "boolean"},
			"verbose":         map[string]any{"type": "boolean"},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 90."},
		}, []string{"sketch_dir", "fqbn"}),
		run: runArduinoCompile,
	},
	{
		name:        "arduino_core_install",
		description: "Install an arduino-cli board core (runs core update-index + core install). 用于修复『arduino-cli 已装但开发板 core 未安装』的首次编译失败。core 取值：arduino:avr(UNO/Nano/Mega)、esp32:esp32(ESP32 全系)、esp8266:esp8266、rp2040:rp2040。ESP32/ESP8266/RP2040 的第三方 board manager URL 已内置，会联网下载（可能几分钟）。",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"core":            map[string]any{"type": "string", "description": "Core id, e.g. arduino:avr / esp32:esp32 / esp8266:esp8266 / rp2040:rp2040."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 600 (core 下载较慢)."},
		}, []string{"core"}),
		run: runArduinoCoreInstall,
	},
	{
		name:        "hardware_install_toolchain",
		description: "一键安装核心硬件工具链(Phase 1)。若 arduino-cli 未安装，自动从官方下载单文件二进制到用户目录（免管理员、免 Python、Windows/macOS 通用），再安装板卡 core（默认 arduino:avr + esp32:esp32，覆盖 Arduino 全系与 ESP32 全系）。装完即可编译/烧录。给学生/老师打包后点一下就能从零备齐环境。可用 cores 参数自定义要装的 core；ESP-IDF / PlatformIO / USB 驱动不在本工具范围（见各自手动指引）。",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"cores":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要安装的 arduino-cli core 列表，默认 [arduino:avr, esp32:esp32]。"},
			"timeout_seconds": map[string]any{"type": "number", "description": "每步超时秒数，默认 600（首次下载 core 较慢）。"},
		}, nil),
		run: runInstallToolchain,
	},
	{
		name:        "hardware_install_arduino_cli",
		description: "只安装 arduino-cli 本体（不含 core）。GUI 分步进度用：先装 arduino-cli，再逐个装 core，中间能刷新进度。返回单步状态 JSON。",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"timeout_seconds": map[string]any{"type": "number", "description": "超时秒数，默认 600。"},
		}, nil),
		run: runInstallArduinoCLI,
	},
	{
		name:        "hardware_install_core",
		description: "安装单个 arduino-cli 板卡 core（已装则秒跳过）。GUI 分步进度用。core 取值：arduino:avr / esp32:esp32 / esp8266:esp8266 / rp2040:rp2040。返回单步状态 JSON。",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"core":            map[string]any{"type": "string", "description": "Core id，如 esp32:esp32。"},
			"timeout_seconds": map[string]any{"type": "number", "description": "超时秒数，默认 900。"},
		}, []string{"core"}),
		run: runInstallCore,
	},
	{
		name:        "arduino_upload",
		description: "Upload a previously compiled or buildable Arduino sketch with arduino-cli. arduino-cli upload does not compile by itself.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"sketch_dir":      map[string]any{"type": "string", "description": "Directory containing the .ino sketch."},
			"fqbn":            map[string]any{"type": "string", "description": "Fully qualified board name, e.g. arduino:avr:uno."},
			"port":            map[string]any{"type": "string", "description": "Serial/upload port, e.g. /dev/cu.usbserial-xxxx."},
			"verify":          map[string]any{"type": "boolean"},
			"build_path":      map[string]any{"type": "string", "description": "Optional directory containing compiled binaries."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 90."},
		}, []string{"sketch_dir", "fqbn", "port"}),
		run: runArduinoUpload,
	},
	{
		name:        "arduino_ota_upload",
		description: "Upload a compiled Arduino sketch over WiFi (OTA) via arduino-cli network upload (espota). The board must already run an ArduinoOTA-capable sketch and be reachable at `address`. No USB needed.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"sketch_dir":      map[string]any{"type": "string", "description": "Directory containing the .ino sketch."},
			"fqbn":            map[string]any{"type": "string", "description": "Fully qualified board name, e.g. esp32:esp32:esp32."},
			"address":         map[string]any{"type": "string", "description": "Board network address: IP (192.168.1.50) or mDNS host (esp32-onecreat.local)."},
			"password":        map[string]any{"type": "string", "description": "OTA password from the board's ArduinoOTA.setPassword(). Optional."},
			"build_path":      map[string]any{"type": "string", "description": "Optional directory with compiled binaries (skip recompile)."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 180."},
		}, []string{"sketch_dir", "fqbn", "address"}),
		run: runArduinoOTAUpload,
	},
	{
		name:        "firmware_publish",
		description: "Publish firmware to a remote HTTP server for OTA cloud-pull: scp the .bin to <ssh_host>:<remote_dir>/<project>/firmware.bin and write version.txt. Boards running the cloud-pull agent fetch it automatically. Provide bin_path, or sketch_dir+fqbn to compile first.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_name":    map[string]any{"type": "string", "description": "Project folder on the server, e.g. line_follower (avoid spaces)."},
			"version":         map[string]any{"type": "string", "description": "New version written to version.txt, e.g. 1.0.2."},
			"ssh_host":        map[string]any{"type": "string", "description": "SSH host/alias for the firmware server, e.g. nas."},
			"remote_dir":      map[string]any{"type": "string", "description": "Firmware root on the server, e.g. /share/Public/onecreat-firmware."},
			"base_url":        map[string]any{"type": "string", "description": "Public base URL for reporting, e.g. http://192.168.6.131:9000."},
			"bin_path":        map[string]any{"type": "string", "description": "Prebuilt .bin to publish. If empty, compile sketch_dir+fqbn."},
			"sketch_dir":      map[string]any{"type": "string", "description": "Sketch to compile when bin_path is empty."},
			"fqbn":            map[string]any{"type": "string", "description": "FQBN to compile with when bin_path is empty."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Overall timeout. Defaults to 300."},
		}, []string{"project_name", "version", "ssh_host", "remote_dir"}),
		run: runFirmwarePublish,
	},
	{
		name:        "arduino_monitor_sample",
		description: "Capture a short Arduino serial monitor sample via arduino-cli monitor for debugging. The command is stopped automatically after seconds.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"port":    map[string]any{"type": "string", "description": "Serial port."},
			"fqbn":    map[string]any{"type": "string", "description": "Optional FQBN."},
			"baud":    map[string]any{"type": "number", "description": "Baud rate. Defaults to 115200."},
			"seconds": map[string]any{"type": "number", "description": "Capture duration. Defaults to 8."},
		}, []string{"port"}),
		run: runArduinoMonitor,
	},
	{
		name:        "platformio_run",
		description: "Run PlatformIO build/upload/monitor targets using pio run. Use targets ['upload','monitor'] for upload plus serial monitor.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":     map[string]any{"type": "string", "description": "Directory containing platformio.ini."},
			"environment":     map[string]any{"type": "string", "description": "Optional PlatformIO environment, e.g. esp32dev."},
			"targets":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional targets, e.g. ['upload'], ['upload','monitor']."},
			"upload_port":     map[string]any{"type": "string"},
			"monitor_port":    map[string]any{"type": "string"},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 90, or 12 when monitor target is included."},
		}, []string{"project_dir"}),
		run: runPlatformIO,
	},
	{
		name:        "esp_idf_run",
		description: "Run common ESP-IDF idf.py actions: set_target, build, flash, monitor, flash_monitor, clean, or fullclean. Prefer the official ESP-IDF MCP server when available; this is the direct fallback.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"project_dir":     map[string]any{"type": "string", "description": "ESP-IDF project directory."},
			"action":          map[string]any{"type": "string", "enum": []string{"set_target", "build", "flash", "monitor", "flash_monitor", "clean", "fullclean"}},
			"target":          map[string]any{"type": "string", "description": "Chip target for set_target, e.g. esp32, esp32s3, esp32c3."},
			"port":            map[string]any{"type": "string", "description": "Serial port for flash/monitor."},
			"baud":            map[string]any{"type": "number", "description": "Monitor baud rate. Defaults to 115200."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 90, or 12 for monitor."},
		}, []string{"project_dir", "action"}),
		run: runESPIDF,
	},
	{
		name:        "esp_idf_mcp_config",
		description: "Generate Reasonix/Claude-compatible config snippets for Espressif's official ESP-IDF Tools MCP server and report local availability.",
		readOnly:    true,
		schema: objectSchema(map[string]any{
			"project_dir": map[string]any{"type": "string", "description": "ESP-IDF project directory. Defaults to current workspace."},
			"server_name": map[string]any{"type": "string", "description": "MCP server name. Defaults to esp-idf-tools."},
			"use_eim":     map[string]any{"type": "boolean", "description": "Use EIM to activate ESP-IDF environment. Recommended when available."},
		}, nil),
		run: runESPIDFMCPConfig,
	},
	{
		name:        "mpremote_run",
		description: "Run a MicroPython script on a connected board with mpremote. Useful for ESP32 MicroPython and rapid classroom demos.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"script":          map[string]any{"type": "string", "description": "Local .py file to run from RAM."},
			"device":          map[string]any{"type": "string", "description": "Optional mpremote device, e.g. auto, /dev/ttyUSB0, id:<serial>."},
			"no_follow":       map[string]any{"type": "boolean", "description": "Return immediately and leave script running."},
			"require_output":  map[string]any{"type": "boolean", "description": "Require runtime output beyond the command header. Defaults to true unless no_follow is true."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 30."},
		}, []string{"script"}),
		run: runMPRemote,
	},
	{
		name:        "ssh_deploy_run",
		description: "Copy files to a Linux-based board over scp, then optionally run a command over ssh. Use for Unihiker, MaixCAM, and Raspberry Pi workflows.",
		readOnly:    false,
		schema: objectSchema(map[string]any{
			"host":            map[string]any{"type": "string", "description": "Device host/IP, e.g. 10.1.2.3 or maixcam-xxxx.local."},
			"user":            map[string]any{"type": "string", "description": "SSH user. Defaults to root."},
			"ssh_port":        map[string]any{"type": "number", "description": "SSH port. Defaults to 22."},
			"identity_file":   map[string]any{"type": "string", "description": "Optional SSH private key path."},
			"password":        map[string]any{"type": "string", "description": "SSH password for password-auth devices (runs via sshpass; Unihiker default is root/dfrobot). Omit when key auth is configured."},
			"local_path":      map[string]any{"type": "string", "description": "Local file or directory to copy."},
			"remote_path":     map[string]any{"type": "string", "description": "Remote destination path."},
			"recursive":       map[string]any{"type": "boolean", "description": "Use scp -r for directories."},
			"command":         map[string]any{"type": "string", "description": "Optional remote command to run after copy."},
			"require_output":  map[string]any{"type": "boolean", "description": "Require remote command output beyond command headers. Defaults to true when command is provided."},
			"connect_timeout": map[string]any{"type": "number", "description": "SSH connect timeout in seconds. Defaults to 8."},
			"timeout_seconds": map[string]any{"type": "number", "description": "Command timeout. Defaults to 90."},
		}, []string{"host", "local_path", "remote_path"}),
		run: runSSHDeploy,
	},
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
			"annotations": map[string]any{
				"readOnlyHint": t.readOnly,
				"title":        t.name,
			},
		})
	}
	return out
}

func callTool(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	for _, t := range tools {
		if t.name != p.Name {
			continue
		}
		text, err := t.run(p.Arguments)
		// 登记真实工具执行的输出到执行日志(无论成功/失败,真实输出都可作为后续证据的锚点),
		// 供 hardware_evidence_record 核实模型填的 output 是否对应真实执行(#2-3)。
		recordToolExecution(p.Name, p.Arguments, text)
		if err != nil {
			return textResult(errorTextWithOutput(text, err), true), nil
		}
		return textResult(text, false), nil
	}
	return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
}

func errorTextWithOutput(output string, err error) string {
	if strings.TrimSpace(output) == "" {
		return err.Error()
	}
	return strings.TrimRight(output, "\n") + "\n\nerror: " + err.Error()
}

func textResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// --- tool handlers ---

type detectReport struct {
	Workspace         string                   `json:"workspace"`
	ProjectDir        string                   `json:"projectDir"`
	ProjectTypes      []string                 `json:"projectTypes"`
	CandidateProjects []projectCandidateReport `json:"candidateProjects"`
	SerialPorts       []string                 `json:"serialPorts"`
	Boards            []boardReport            `json:"boards"`
	Devices           []deviceReport           `json:"devices"`
	Toolchains        []toolchainReport        `json:"toolchains"`
	NetworkBoards     []networkBoardReport     `json:"networkBoards"`
	Recommendations   []string                 `json:"recommendations"`
	ESPIDFOfficial    map[string]string        `json:"espIdfOfficialMcp"`
}

type projectCandidateReport struct {
	Dir   string `json:"dir"`
	Kind  string `json:"kind"`
	Entry string `json:"entry,omitempty"`
}

type boardReport struct {
	Port       string `json:"port"`
	Protocol   string `json:"protocol,omitempty"`
	BoardName  string `json:"boardName,omitempty"`
	FQBN       string `json:"fqbn,omitempty"`
	Core       string `json:"core,omitempty"`
	Properties string `json:"properties,omitempty"`
}

type deviceReport struct {
	Port        string `json:"port"`
	Description string `json:"description,omitempty"`
	HWID        string `json:"hwid,omitempty"`
}

type toolchainReport struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type boardProfileReport struct {
	Board       string                `json:"board,omitempty"`
	Platform    string                `json:"platform,omitempty"`
	Profile     *boardProfile         `json:"profile,omitempty"`
	Profiles    []boardProfileSummary `json:"profiles,omitempty"`
	NextStep    string                `json:"nextStep"`
	Suggestions []string              `json:"suggestions,omitempty"`
}

type boardProfileSummary struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Platform         string   `json:"platform"`
	Board            string   `json:"board"`
	DefaultFramework string   `json:"defaultFramework"`
	Aliases          []string `json:"aliases,omitempty"`
}

// boardProfile / boardPinRule 现在是共享数据驱动注册表 internal/hardware/boards 的
// 类型别名:板卡事实统一长在 boards.json,桌面端与 MCP 走同一份,扩板=改 JSON。
type boardProfile = boards.Board
type boardPinRule = boards.PinRule

func runHardwareDetect(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	projectDir := strArg(args, "project_dir", cwd)
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}

	report := detectReport{
		Workspace:         cwd,
		ProjectDir:        absProject,
		ProjectTypes:      detectProjectTypes(absProject),
		CandidateProjects: detectHardwareProjectCandidates(absProject),
		SerialPorts:       listSerialPorts(),
		Boards:            listArduinoBoards(),
		Devices:           listPlatformIODevices(),
		Toolchains: []toolchainReport{
			detectToolchain("arduino-cli", []string{"arduino-cli", "version"}, "Install Arduino CLI from https://arduino.github.io/arduino-cli/"),
			detectToolchain("PlatformIO", []string{"pio", "--version"}, "Install with `python3 -m pip install platformio` or use PlatformIO IDE."),
			detectESPIDFToolchain(),
			detectEIMToolchain(),
			detectToolchain("mpremote", []string{"mpremote", "--version"}, "Install with `python3 -m pip install --user mpremote` or `pipx install mpremote`."),
			detectToolchain("ssh", []string{"ssh", "-V"}, "Install OpenSSH client."),
			detectToolchain("scp", []string{"scp"}, "Install OpenSSH client."),
		},
	}
	report.NetworkBoards = detectNetworkBoards()
	report.ESPIDFOfficial = espIDFMCPHint(absProject, "esp-idf-tools", hasEIM() && !resolveLocalESPIDF().Available)
	report.Recommendations = recommendations(report)
	return prettyJSON(report), nil
}

func runBoardProfile(args map[string]any) (string, error) {
	platform := strArg(args, "platform", "auto")
	if boolArg(args, "all", false) {
		report := boardProfileReport{
			Platform: platform,
			Profiles: boardProfileSummaries(filterBoardProfiles(platform)),
			NextStep: "选择一个 profile 后，先按 recommendedProtocols 和 riskyPins 输出接线/引脚说明，再进入代码阶段。",
		}
		return prettyJSON(report), nil
	}
	board := strArg(args, "board", "")
	if board == "" && platform != "" && platform != "auto" {
		board = defaultBoard(platform)
	}
	profile, ok := findBoardProfile(board, platform)
	if !ok {
		report := boardProfileReport{
			Board:    board,
			Platform: platform,
			Profiles: boardProfileSummaries(filterBoardProfiles(platform)),
			NextStep: "没有精确匹配的板卡 profile；选择最接近的 profile，或在硬件方案阶段要求用户确认板卡型号。",
			Suggestions: []string{
				"ESP32 Arduino/PlatformIO 可传 board=esp32dev 或 board=esp32_arduino。",
				"ESP32 ESP-IDF 可传 board=esp32_idf 或 platform=esp_idf。",
				"Arduino UNO/Nano 可传 board=uno 或 board=nano。",
			},
		}
		return prettyJSON(report), nil
	}
	report := boardProfileReport{
		Board:    profile.Board,
		Platform: profile.Platform,
		Profile:  &profile,
		NextStep: "先把 profile 中的默认引脚、风险引脚、电压和验证流程写入方案；用户确认后再生成代码，并运行 hardware_project_audit / hardware_project_validate。",
	}
	return prettyJSON(report), nil
}

func runScaffold(args map[string]any) (string, error) {
	name := cleanProjectName(strArg(args, "project_name", ""))
	if name == "" {
		return "", errors.New("project_name is required and must contain letters, digits, '_' or '-'")
	}
	platform := strArg(args, "platform", "")
	if platform == "" {
		return "", errors.New("platform is required")
	}
	board := strArg(args, "board", defaultBoard(platform))
	dir := strArg(args, "project_dir", name)
	overwrite := boolArg(args, "overwrite", false)
	allowOutside := boolArg(args, "allow_outside_workspace", false)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !allowOutside {
		if err := requireInsideWorkspace(absDir); err != nil {
			return "", err
		}
	}

	var files map[string]string
	switch platform {
	case "arduino":
		files = scaffoldArduino(name, board)
	case "platformio":
		files = scaffoldPlatformIO(name, board)
	case "esp_idf":
		files = scaffoldESPIDF(name, board)
	case "micropython":
		files = scaffoldMicroPython(name, board)
	case "unihiker_python":
		files = scaffoldLinuxPython(name, board, "Unihiker", "root@10.1.2.3")
	case "maixcam_python":
		files = scaffoldMaixCAM(name, board)
	case "raspberry_pi_python":
		files = scaffoldLinuxPython(name, board, "Raspberry Pi", "pi@raspberrypi.local")
	default:
		return "", fmt.Errorf("unsupported platform %q", platform)
	}
	for rel, body := range commonHardwareProjectFiles(name, platform, board, verificationCommand(platform, board, name)) {
		files[rel] = body
	}

	// 学生在 scaffold 时声明的模块清单(如 ["DHT11","SSD1306"])——
	// 解析成真连接,覆盖默认占位,触发已有的 module_specs.md 自动注入。
	moduleNames := strSliceArg(args, "modules")
	if conns := modulesToConnections(moduleNames, board); len(conns) > 0 {
		entrypoints := platformEntrypoints(platform, name)
		files["hardware_manifest.json"] = hardwareManifestWithEntrypointsAndConnections(name, platform, board, verificationCommand(platform, board, name), entrypoints, conns)
		if md := moduleSpecsForConnections(conns, board); md != "" {
			files["docs/module_specs.md"] = md
		}
	}

	// 把 /hardware:* 斜杠命令一并放进新项目,学生 cd 进去立刻能用。
	for rel, body := range bundledHardwareCommandFiles() {
		files[rel] = body
	}

	var written []string
	for rel, body := range files {
		target := filepath.Join(absDir, filepath.FromSlash(rel))
		if err := writeTextFile(target, body, overwrite); err != nil {
			return "", err
		}
		written = append(written, target)
	}
	sort.Strings(written)
	return "Created hardware scaffold:\n" + strings.Join(written, "\n"), nil
}

func runProjectContext(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !boolArg(args, "allow_outside_workspace", false) {
		if err := requireInsideWorkspace(abs); err != nil {
			return "", err
		}
	}

	manifest, _ := auditManifestFile(abs)
	projectName := cleanProjectName(strArg(args, "project_name", manifest.ProjectName))
	if projectName == "" {
		projectName = cleanProjectName(filepath.Base(abs))
	}
	if projectName == "" {
		projectName = "hardware_project"
	}
	detected := detectProjectTypes(abs)
	platform := auditExpectedPlatform(strArg(args, "platform", "auto"), manifest.Platform, detected)
	if platform == "" || platform == "unknown" {
		report := projectContextReport{
			ProjectDir:  abs,
			ProjectName: projectName,
			Platform:    platform,
			Board:       strArg(args, "board", manifest.Board),
			DryRun:      boolArg(args, "dry_run", false),
			Overwrite:   boolArg(args, "overwrite", false),
			Actions:     []contextAction{},
			NextStep:    "无法判断平台；请传 platform=arduino/platformio/esp_idf/micropython/unihiker_python/maixcam_python/raspberry_pi_python 后重试。",
		}
		return prettyJSON(report), errors.New("platform is required when project type cannot be detected")
	}
	board := strArg(args, "board", manifest.Board)
	if board == "" {
		board = defaultBoard(platform)
	}
	verify := verificationCommand(platform, board, projectName)
	files := standardHardwareContextFilesForProject(abs, projectName, platform, board, verify)
	// 已有项目:优先用 manifest 里真实声明的连接解析模块规格,覆盖默认值。
	if md := moduleSpecsForConnections(manifest.Connections, board); md != "" {
		files["docs/module_specs.md"] = md
	}
	dryRun := boolArg(args, "dry_run", false)
	overwrite := boolArg(args, "overwrite", false)
	report := projectContextReport{
		ProjectDir:  abs,
		ProjectName: projectName,
		Platform:    platform,
		Board:       board,
		DryRun:      dryRun,
		Overwrite:   overwrite,
		Actions:     []contextAction{},
		NextStep:    "运行 hardware_board_profile 确认板卡约束，再运行 hardware_project_audit 和 hardware_project_validate；完成本机验证后记录 evidence。",
	}

	keys := make([]string, 0, len(files))
	for rel := range files {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		target := filepath.Join(abs, filepath.FromSlash(rel))
		action := contextAction{Kind: "write", Path: target, Status: "planned", Message: "写入标准硬件项目上下文文件。"}
		if exists(target) && !overwrite {
			action.Status = "skipped"
			action.Message = "文件已存在；默认不覆盖客户已有内容。"
			report.Actions = append(report.Actions, action)
			continue
		}
		if dryRun {
			report.Actions = append(report.Actions, action)
			continue
		}
		if err := writeTextFile(target, files[rel], true); err != nil {
			action.Status = "failed"
			action.Message = err.Error()
			report.Actions = append(report.Actions, action)
			return prettyJSON(report), err
		}
		action.Status = "applied"
		report.Applied = true
		report.Actions = append(report.Actions, action)
	}
	return prettyJSON(report), nil
}

type validationReport struct {
	ProjectDir      string             `json:"projectDir"`
	ProjectTypes    []string           `json:"projectTypes"`
	Results         []validationResult `json:"results"`
	Summary         string             `json:"summary"`
	Recommendations []string           `json:"recommendations"`
}

type validationResult struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Status    string `json:"status"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	RootCause string `json:"rootCause,omitempty"` // 蒸馏出的根因错误,免去模型在级联报错里找
	FixHint   string `json:"fixHint,omitempty"`   // 已知错误模式对应的直接修法
	NextStep  string `json:"nextStep,omitempty"`
}

type auditReport struct {
	ProjectDir       string        `json:"projectDir"`
	ProjectTypes     []string      `json:"projectTypes"`
	ExpectedPlatform string        `json:"expectedPlatform"`
	Manifest         auditManifest `json:"manifest"`
	Results          []auditResult `json:"results"`
	Summary          string        `json:"summary"`
	Recommendations  []string      `json:"recommendations"`
}

type auditManifest struct {
	Present       bool     `json:"present"`
	SchemaVersion string   `json:"schemaVersion,omitempty"`
	ProjectName   string   `json:"projectName,omitempty"`
	Platform      string   `json:"platform,omitempty"`
	Board         string   `json:"board,omitempty"`
	Entrypoints   []string `json:"entrypoints,omitempty"`
}

type auditResult struct {
	Kind    string   `json:"kind"`
	Target  string   `json:"target"`
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Missing []string `json:"missing,omitempty"`
}

type repairReport struct {
	ProjectDir string         `json:"projectDir"`
	Repair     string         `json:"repair"`
	DryRun     bool           `json:"dryRun"`
	Applied    bool           `json:"applied"`
	Actions    []repairAction `json:"actions"`
	NextStep   string         `json:"nextStep,omitempty"`
}

type projectContextReport struct {
	ProjectDir  string          `json:"projectDir"`
	ProjectName string          `json:"projectName"`
	Platform    string          `json:"platform"`
	Board       string          `json:"board"`
	DryRun      bool            `json:"dryRun"`
	Overwrite   bool            `json:"overwrite"`
	Applied     bool            `json:"applied"`
	Actions     []contextAction `json:"actions"`
	NextStep    string          `json:"nextStep"`
}

type contextAction struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type repairCatalogReport struct {
	Platform        string       `json:"platform,omitempty"`
	Rules           []repairRule `json:"rules"`
	AutoRepairCount int          `json:"autoRepairCount"`
	NextStep        string       `json:"nextStep"`
}

type repairRule struct {
	Code             string   `json:"code"`
	Title            string   `json:"title"`
	Platforms        []string `json:"platforms"`
	DetectedBy       []string `json:"detectedBy"`
	AutoRepairTool   string   `json:"autoRepairTool,omitempty"`
	AutoRepair       string   `json:"autoRepair,omitempty"`
	ManualSteps      []string `json:"manualSteps"`
	EvidenceRequired []string `json:"evidenceRequired"`
}

type repairAction struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type hardwareManifestAuditData struct {
	SchemaVersion      string               `json:"schemaVersion"`
	ProjectName        string               `json:"projectName"`
	Platform           string               `json:"platform"`
	Board              string               `json:"board"`
	Entrypoints        []string             `json:"entrypoints"`
	PreferredProtocols []string             `json:"preferredProtocols"`
	Connections        []hardwareConnection `json:"connections"`
	MCPTools           []string             `json:"mcpTools"`
	Verification       struct {
		LocalCommand       string   `json:"localCommand"`
		MinimumLocalGate   string   `json:"minimumLocalGate"`
		RealHardwareChecks []string `json:"realHardwareChecks"`
	} `json:"verification"`
}

type hardwareConnection struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Protocol string   `json:"protocol"`
	Pins     []string `json:"pins,omitempty"`
	Voltage  string   `json:"voltage"`
	Notes    string   `json:"notes,omitempty"`
}

type evidenceRecord struct {
	TimestampUTC         string `json:"timestampUtc"`
	ProjectDir           string `json:"projectDir"`
	ProjectFingerprint   string `json:"projectFingerprint,omitempty"`
	FingerprintFileCount int    `json:"fingerprintFileCount,omitempty"`
	Platform             string `json:"platform"`
	Board                string `json:"board,omitempty"`
	Stage                string `json:"stage"`
	Status               string `json:"status"`
	Summary              string `json:"summary"`
	Command              string `json:"command,omitempty"`
	Port                 string `json:"port,omitempty"`
	ArtifactPath         string `json:"artifactPath,omitempty"`
	OutputExcerpt        string `json:"outputExcerpt,omitempty"`
	HostObserved         bool   `json:"hostObserved,omitempty"` // 该证据的 output 对得上 MCP 真实跑过的工具执行(#2-3)
}

type evidenceStatusReport struct {
	ProjectDir              string                  `json:"projectDir"`
	Platform                string                  `json:"platform"`
	Board                   string                  `json:"board,omitempty"`
	EvidenceFile            string                  `json:"evidenceFile"`
	CurrentFingerprint      string                  `json:"currentFingerprint,omitempty"`
	CurrentFingerprintFiles int                     `json:"currentFingerprintFiles,omitempty"`
	RecordCount             int                     `json:"recordCount"`
	CurrentRecordCount      int                     `json:"currentRecordCount"`
	StaleRecordCount        int                     `json:"staleRecordCount"`
	Status                  string                  `json:"status"`
	Summary                 string                  `json:"summary"`
	Stages                  []evidenceStageSummary  `json:"stages"`
	RequiredGroups          []evidenceRequiredGroup `json:"requiredGroups"`
	MissingGroups           []string                `json:"missingGroups"`
	Recommendations         []string                `json:"recommendations"`
}

type evidenceStageSummary struct {
	Stage         string `json:"stage"`
	Passed        int    `json:"passed"`
	Failed        int    `json:"failed"`
	Skipped       int    `json:"skipped"`
	Info          int    `json:"info"`
	LatestStatus  string `json:"latestStatus,omitempty"`
	LatestSummary string `json:"latestSummary,omitempty"`
	LatestAt      string `json:"latestAt,omitempty"`
}

type evidenceRequiredGroup struct {
	Name        string   `json:"name"`
	Stages      []string `json:"stages"`
	RequiredFor string   `json:"requiredFor"`
	Passed      bool     `json:"passed"`
}

type deviceVerifyPlanReport struct {
	ProjectDir          string                 `json:"projectDir"`
	Platform            string                 `json:"platform"`
	Board               string                 `json:"board,omitempty"`
	ProjectTypes        []string               `json:"projectTypes"`
	ManifestPresent     bool                   `json:"manifestPresent"`
	LocalOnlyCommand    string                 `json:"localOnlyCommand"`
	RealDeviceCommand   string                 `json:"realDeviceCommand"`
	ReadyForRealDevice  bool                   `json:"readyForRealDevice"`
	MissingInputs       []string               `json:"missingInputs"`
	DetectedSerialPorts []string               `json:"detectedSerialPorts"`
	DetectedBoards      []boardReport          `json:"detectedBoards"`
	DetectedDevices     []deviceReport         `json:"detectedDevices"`
	Toolchains          []toolchainReport      `json:"toolchains"`
	Resolved            map[string]string      `json:"resolved"`
	MCPSteps            []deviceVerifyPlanStep `json:"mcpSteps"`
	Recommendations     []string               `json:"recommendations"`
}

type deviceVerifyPlanStep struct {
	Order     int            `json:"order"`
	Stage     string         `json:"stage"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Notes     string         `json:"notes,omitempty"`
}

func runProjectValidate(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	timeout := timeoutArg(args, "timeout_seconds", defaultTimeout)
	platform := strArg(args, "platform", "auto")
	if platform == "" {
		platform = "auto"
	}
	projectTypes := detectProjectTypes(abs)
	validationTypes := projectTypesForValidation(platform, projectTypes)
	report := validationReport{
		ProjectDir:      abs,
		ProjectTypes:    projectTypes,
		Results:         []validationResult{},
		Recommendations: []string{},
	}
	for _, typ := range validationTypes {
		switch typ {
		case "arduino":
			report.Results = append(report.Results, validateArduinoProject(abs, args, timeout)...)
		case "platformio":
			report.Results = append(report.Results, validatePlatformIOProject(abs, args, timeout))
		case "esp_idf":
			report.Results = append(report.Results, validateESPIDFProject(abs, args, timeout)...)
		case "python_or_micropython", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python":
			report.Results = append(report.Results, validatePythonProject(abs, timeout))
		case "unknown":
			report.Results = append(report.Results, validationResult{
				Kind:     "unknown",
				Target:   abs,
				Status:   "skipped",
				NextStep: "未识别项目类型；请确认目录中有 platformio.ini、ESP-IDF CMakeLists.txt、Arduino .ino 或 Python 源文件。",
			})
		}
	}
	report.Results = dedupeValidationResults(report.Results)
	report.Summary = validationSummary(report.Results)
	report.Recommendations = validationRecommendations(report.Results)
	out := prettyJSON(report)
	// 有任何一项 failed(编译/构建失败)时返回 error,让 MCP 标 isError、agent 把收据记为
	// Success=false——否则编译失败被吞成成功收据,弱模型用一次 validate 就把没编译过的项目
	// 当成「已验证」签收(#2-2)。报告文本仍随 error 一并回给模型,它能看到失败详情。
	if failed := countFailedValidations(report.Results); failed > 0 {
		return out, fmt.Errorf("hardware_project_validate: %d 项验证失败(见上方报告);未通过编译/构建不能算验证通过", failed)
	}
	return out, nil
}

// countFailedValidations 统计 status=="failed" 的验证项(skipped/warning 不算失败)。
func countFailedValidations(results []validationResult) int {
	n := 0
	for _, r := range results {
		if r.Status == "failed" {
			n++
		}
	}
	return n
}

func runProjectAudit(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	detected := detectProjectTypes(abs)
	report := auditReport{
		ProjectDir:      abs,
		ProjectTypes:    detected,
		Results:         []auditResult{},
		Recommendations: []string{},
	}

	manifest, manifestResult := auditManifestFile(abs)
	report.Manifest = auditManifest{
		Present:       manifestResult.Status == "passed" || manifestResult.Status == "warning",
		SchemaVersion: manifest.SchemaVersion,
		ProjectName:   manifest.ProjectName,
		Platform:      manifest.Platform,
		Board:         manifest.Board,
		Entrypoints:   manifest.Entrypoints,
	}
	report.Results = append(report.Results, manifestResult)

	expected := auditExpectedPlatform(strArg(args, "platform", "auto"), manifest.Platform, detected)
	report.ExpectedPlatform = expected
	report.Results = append(report.Results, auditProjectType(abs, expected, detected))
	report.Results = append(report.Results, auditProjectLayout(abs, expected)...)

	projectName := manifest.ProjectName
	if projectName == "" {
		projectName = filepath.Base(abs)
	}
	board := manifest.Board
	if board == "" && expected != "" && expected != "unknown" {
		board = defaultBoard(expected)
	}

	report.Results = append(report.Results, auditRequiredHardwareFiles(abs)...)
	report.Results = append(report.Results, auditConnections(abs, expected, manifest.Connections)...)
	report.Results = append(report.Results, auditEntrypoints(abs, expected, projectName, manifest.Entrypoints)...)
	if expected != "" && expected != "unknown" && board != "" {
		report.Results = append(report.Results, auditVerificationCommand(abs, expected, board, projectName, manifest.Verification.LocalCommand))
	}

	report.Summary = auditSummary(report.Results)
	report.Recommendations = auditRecommendations(report.Results)
	return prettyJSON(report), nil
}

func runProjectRepair(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !boolArg(args, "allow_outside_workspace", false) {
		if err := requireInsideWorkspace(abs); err != nil {
			return "", err
		}
	}
	repair := strArg(args, "repair", "")
	if repair == "" {
		return "", errors.New("repair is required")
	}
	switch repair {
	case "platformio_root_ino_to_src_main":
		return repairPlatformIORootINO(abs, args)
	default:
		return "", errors.New("unsupported repair: " + repair)
	}
}

func repairPlatformIORootINO(projectDir string, args map[string]any) (string, error) {
	dryRun := boolArg(args, "dry_run", false)
	overwrite := boolArg(args, "overwrite", false)
	report := repairReport{
		ProjectDir: projectDir,
		Repair:     "platformio_root_ino_to_src_main",
		DryRun:     dryRun,
		Actions:    []repairAction{},
		NextStep:   "运行 hardware_project_audit，再运行 hardware_project_validate；编译通过后再 upload 和 monitor，并用 hardware_evidence_record 保存证据。",
	}
	if !exists(filepath.Join(projectDir, "platformio.ini")) {
		return prettyJSON(report), errors.New("platformio.ini not found; this repair only applies to PlatformIO projects")
	}
	source, err := chooseRootINO(projectDir, strArg(args, "source_ino", ""))
	if err != nil {
		return prettyJSON(report), err
	}
	srcMain := filepath.Join(projectDir, "src", "main.cpp")
	if exists(srcMain) && !overwrite {
		report.Actions = append(report.Actions, repairAction{Kind: "write", Path: srcMain, Status: "blocked", Message: "src/main.cpp 已存在；如确认覆盖，传 overwrite=true。"})
		return prettyJSON(report), errors.New("src/main.cpp already exists")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return prettyJSON(report), err
	}
	body := convertINOToPlatformIOCPP(string(data))
	includes := quotedHeaderIncludes(string(data))
	headerCopies := platformIOHeaderCopyActions(projectDir, includes)

	report.Actions = append(report.Actions, repairAction{Kind: "mkdir", Path: filepath.Join(projectDir, "src"), Status: "planned", Message: "确保 PlatformIO src/ 目录存在。"})
	report.Actions = append(report.Actions, repairAction{Kind: "write", Path: srcMain, Status: "planned", Message: "写入 PlatformIO 主入口 src/main.cpp。"})
	for _, action := range headerCopies {
		report.Actions = append(report.Actions, action)
	}
	backupPath := filepath.Join(projectDir, ".onecreat-backup", time.Now().UTC().Format("20060102-150405")+"-"+filepath.Base(source))
	report.Actions = append(report.Actions, repairAction{Kind: "move", Path: backupPath, Status: "planned", Message: "将根目录 .ino 移到备份目录，避免模型继续修改死入口文件。"})

	if dryRun {
		return prettyJSON(report), nil
	}
	if err := os.MkdirAll(filepath.Dir(srcMain), 0o755); err != nil {
		return prettyJSON(report), err
	}
	if err := os.WriteFile(srcMain, []byte(body), 0o644); err != nil {
		return prettyJSON(report), err
	}
	for _, include := range includes {
		rootHeader := filepath.Join(projectDir, include)
		includeHeader := filepath.Join(projectDir, "include", include)
		if !exists(rootHeader) || exists(includeHeader) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(includeHeader), 0o755); err != nil {
			return prettyJSON(report), err
		}
		headerData, err := os.ReadFile(rootHeader)
		if err != nil {
			return prettyJSON(report), err
		}
		if err := os.WriteFile(includeHeader, headerData, 0o644); err != nil {
			return prettyJSON(report), err
		}
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		return prettyJSON(report), err
	}
	if err := os.Rename(source, backupPath); err != nil {
		return prettyJSON(report), err
	}
	for i := range report.Actions {
		if report.Actions[i].Status == "planned" {
			report.Actions[i].Status = "applied"
		}
	}
	report.Applied = true
	return prettyJSON(report), nil
}

func runRepairCatalog(args map[string]any) (string, error) {
	platform := strArg(args, "platform", "auto")
	rules := repairRulesForPlatform(platform)
	autoCount := 0
	for _, rule := range rules {
		if rule.AutoRepairTool != "" {
			autoCount++
		}
	}
	report := repairCatalogReport{
		Platform:        platform,
		Rules:           rules,
		AutoRepairCount: autoCount,
		NextStep:        "把 audit/validate 的 kind、output 和 error 对照 catalog；有 autoRepairTool 时先用工具做最小修复，再重新 audit/validate；没有自动修复时按 manualSteps 处理。",
	}
	return prettyJSON(report), nil
}

func runEvidenceRecord(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !boolArg(args, "allow_outside_workspace", false) {
		if err := requireInsideWorkspace(abs); err != nil {
			return "", err
		}
	}

	stage := strArg(args, "stage", "")
	status := strArg(args, "status", "")
	summary := strArg(args, "summary", "")
	if err := requireAllowed("stage", stage, []string{"audit", "compile", "syntax", "upload", "flash", "monitor", "serial", "mpremote", "ssh", "deploy", "manual"}); err != nil {
		return "", err
	}
	if err := requireAllowed("status", status, []string{"passed", "failed", "skipped", "info"}); err != nil {
		return "", err
	}
	if summary == "" {
		return "", errors.New("summary is required")
	}

	manifest, _ := auditManifestFile(abs)
	platform := strArg(args, "platform", "")
	if platform == "" || platform == "auto" {
		platform = manifest.Platform
	}
	if platform == "" {
		platform = auditExpectedPlatform("auto", "", detectProjectTypes(abs))
	}
	board := strArg(args, "board", "")
	if board == "" {
		board = manifest.Board
	}
	fingerprint, fingerprintFiles, err := projectFingerprint(abs)
	if err != nil {
		return "", err
	}

	record := evidenceRecord{
		TimestampUTC:         time.Now().UTC().Format(time.RFC3339),
		ProjectDir:           abs,
		ProjectFingerprint:   fingerprint,
		FingerprintFileCount: fingerprintFiles,
		Platform:             platform,
		Board:                board,
		Stage:                stage,
		Status:               status,
		Summary:              summary,
		Command:              strArg(args, "command", ""),
		Port:                 strArg(args, "port", ""),
		ArtifactPath:         strArg(args, "artifact_path", ""),
		OutputExcerpt:        truncateText(strArg(args, "output", ""), 4000),
	}
	// 真机执行类阶段:用本进程的真实执行日志核实证据是否有真实执行支撑(#2-3)。
	if hardwareExecutionStage(stage) {
		record.HostObserved = computeHostObserved(stage, strArg(args, "output", ""))
	}

	testsDir := filepath.Join(abs, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		return "", err
	}
	evidencePath := filepath.Join(testsDir, "hardware_evidence.jsonl")
	line, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if err := appendTextFile(evidencePath, string(line)+"\n"); err != nil {
		return "", err
	}

	checklistPath := filepath.Join(testsDir, "hardware_checklist.md")
	if !exists(checklistPath) {
		verify := ""
		if platform != "" && board != "" {
			verify = verificationCommand(platform, board, filepath.Base(abs))
		}
		if err := writeTextFile(checklistPath, hardwareChecklist(filepath.Base(abs), platform, board, verify), false); err != nil {
			return "", err
		}
	}
	if err := appendEvidenceToChecklist(checklistPath, record); err != nil {
		return "", err
	}

	return prettyJSON(map[string]any{
		"record": record,
		"written": []string{
			evidencePath,
			checklistPath,
		},
		"nextStep": "继续执行下一阶段验证；真实硬件验证完成后，把 upload/flash/monitor/ssh 的输出都追加到 hardware_evidence.jsonl。",
	}), nil
}

func runEvidenceStatus(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	manifest, _ := auditManifestFile(abs)
	platform := strArg(args, "platform", "")
	if platform == "" || platform == "auto" {
		platform = manifest.Platform
	}
	if platform == "" {
		platform = auditExpectedPlatform("auto", "", detectProjectTypes(abs))
	}
	board := manifest.Board
	fingerprint, fingerprintFiles, err := projectFingerprint(abs)
	if err != nil {
		return "", err
	}
	evidencePath := filepath.Join(abs, "tests", "hardware_evidence.jsonl")
	records, invalid, err := readEvidenceRecords(evidencePath)
	if err != nil {
		report := evidenceStatusReport{
			ProjectDir:              abs,
			Platform:                platform,
			Board:                   board,
			EvidenceFile:            evidencePath,
			CurrentFingerprint:      fingerprint,
			CurrentFingerprintFiles: fingerprintFiles,
			Status:                  "no_evidence",
			Summary:                 "0 evidence records; local and real-hardware verification are not proven.",
			Recommendations:         []string{"先运行 hardware_project_audit 和 hardware_project_validate，再调用 hardware_evidence_record 记录本机验证结果。"},
		}
		return prettyJSON(report), nil
	}
	currentRecords, staleRecords := partitionEvidenceByFingerprint(records, fingerprint)
	stageSummaries := summarizeEvidenceStages(currentRecords)
	required := evidenceRequiredGroups(platform)
	missing := []string{}
	for i := range required {
		required[i].Passed = evidenceGroupPassed(currentRecords, required[i].Stages)
		if !required[i].Passed {
			missing = append(missing, required[i].Name)
		}
	}
	status := evidenceOverallStatus(currentRecords, required)
	if len(records) > 0 && len(currentRecords) == 0 {
		status = "stale"
	}
	recs := evidenceStatusRecommendations(status, platform, missing, invalid, len(staleRecords))
	report := evidenceStatusReport{
		ProjectDir:              abs,
		Platform:                platform,
		Board:                   board,
		EvidenceFile:            evidencePath,
		CurrentFingerprint:      fingerprint,
		CurrentFingerprintFiles: fingerprintFiles,
		RecordCount:             len(records),
		CurrentRecordCount:      len(currentRecords),
		StaleRecordCount:        len(staleRecords),
		Status:                  status,
		Summary:                 evidenceStatusSummary(status, len(records), missing),
		Stages:                  stageSummaries,
		RequiredGroups:          required,
		MissingGroups:           missing,
		Recommendations:         recs,
	}
	return prettyJSON(report), nil
}

func runDeviceVerifyPlan(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	detected := detectProjectTypes(abs)
	manifest, manifestResult := auditManifestFile(abs)
	platform := auditExpectedPlatform(strArg(args, "platform", "auto"), manifest.Platform, detected)
	board := strArg(args, "board", manifest.Board)
	if board == "" && platform != "" && platform != "unknown" {
		board = defaultBoard(platform)
	}

	serialPorts := listSerialPorts()
	boards := listArduinoBoards()
	devices := listPlatformIODevices()
	toolchains := []toolchainReport{
		detectToolchain("arduino-cli", []string{"arduino-cli", "version"}, "Install Arduino CLI from https://arduino.github.io/arduino-cli/"),
		detectToolchain("PlatformIO", []string{"pio", "--version"}, "Install with `python3 -m pip install platformio` or use PlatformIO IDE."),
		detectESPIDFToolchain(),
		detectToolchain("mpremote", []string{"mpremote", "--version"}, "Install with `python3 -m pip install --user mpremote` or `pipx install mpremote`."),
		detectToolchain("ssh", []string{"ssh", "-V"}, "Install OpenSSH client."),
		detectToolchain("scp", []string{"scp"}, "Install OpenSSH client."),
	}

	resolved := map[string]string{}
	missing := []string{}
	recs := []string{}

	if manifestResult.Status == "failed" {
		recs = append(recs, "先补齐 hardware_manifest.json，或用 hardware_project_scaffold 生成标准项目上下文。")
	}
	if platform == "" || platform == "unknown" {
		missing = append(missing, "platform")
		recs = append(recs, "指定 platform，或补齐能被检测的项目入口文件。")
	}
	if board == "" || board == "unknown" {
		missing = append(missing, "board")
	}

	baud := intArg(args, "baud", 115200)
	monitorSeconds := intArg(args, "monitor_seconds", 8)
	resolved["baud"] = strconv.Itoa(baud)
	resolved["monitorSeconds"] = strconv.Itoa(monitorSeconds)

	localParts := []string{"scripts/hardware-device-verify.sh", "--platform", placeholderIfEmpty(platform, "PLATFORM"), "--project-dir", abs}
	realParts := append([]string{}, localParts...)
	if board != "" && board != "unknown" {
		localParts = append(localParts, "--board", board)
		realParts = append(realParts, "--board", board)
		resolved["board"] = board
	}
	localParts = append(localParts, "--local-only")

	switch platform {
	case "arduino":
		fqbn := strArg(args, "fqbn", firstDetectedFQBN(boards))
		if fqbn == "" {
			fqbn = arduinoFQBN(board)
		}
		port := strArg(args, "port", chooseSerialPort(boards, devices, serialPorts))
		realParts = append(realParts, "--fqbn", fqbn, "--port", placeholderIfEmpty(port, "SERIAL_PORT"), "--baud", strconv.Itoa(baud), "--monitor-seconds", strconv.Itoa(monitorSeconds))
		resolved["fqbn"] = fqbn
		if port != "" {
			resolved["port"] = port
		} else {
			missing = append(missing, "port")
		}
		missing = appendMissingToolchain(missing, toolchains, "arduino-cli")
	case "platformio":
		env := strArg(args, "environment", "")
		if env == "" {
			env = inferPlatformIOEnv(abs)
		}
		if env == "" {
			env = platformIOEnv(board)
		}
		port := strArg(args, "port", chooseSerialPort(boards, devices, serialPorts))
		realParts = append(realParts, "--environment", env, "--port", placeholderIfEmpty(port, "SERIAL_PORT"), "--baud", strconv.Itoa(baud), "--monitor-seconds", strconv.Itoa(monitorSeconds))
		resolved["environment"] = env
		if port != "" {
			resolved["port"] = port
		} else {
			missing = append(missing, "port")
		}
		missing = appendMissingToolchain(missing, toolchains, "PlatformIO")
	case "esp_idf":
		target := strArg(args, "target", espIDFTarget(board))
		port := strArg(args, "port", chooseSerialPort(boards, devices, serialPorts))
		realParts = append(realParts, "--target", target, "--port", placeholderIfEmpty(port, "SERIAL_PORT"), "--baud", strconv.Itoa(baud), "--monitor-seconds", strconv.Itoa(monitorSeconds))
		resolved["target"] = target
		if port != "" {
			resolved["port"] = port
		} else {
			missing = append(missing, "port")
		}
		missing = appendMissingToolchain(missing, toolchains, "ESP-IDF")
	case "micropython":
		device := strArg(args, "device", "")
		if device == "" {
			device = strArg(args, "port", "")
		}
		if device == "" && len(serialPorts) > 0 {
			device = serialPorts[0]
		}
		if device == "" {
			device = "auto"
			missing = append(missing, "device")
			recs = append(recs, "MicroPython 未检测到明确串口；计划使用 mpremote auto。若多块板同时连接，请传入 device。")
		}
		realParts = append(realParts, "--device", device)
		resolved["device"] = device
		missing = appendMissingToolchain(missing, toolchains, "mpremote")
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		user, host := sshTargetDefaults(platform, manifest.Verification.LocalCommand)
		hostSource := "manifest_or_default"
		if argUser, ok := stringArg(args, "user"); ok {
			user = argUser
		}
		if argHost, ok := stringArg(args, "host"); ok {
			host = argHost
			hostSource = "argument"
		}
		if sshHostNeedsConfirmation(platform, host, hostSource) {
			missing = append(missing, "host")
		}
		if user == "" {
			user = "root"
		}
		realParts = append(realParts, "--host", placeholderIfEmpty(host, "DEVICE_HOST"), "--user", user)
		resolved["host"] = host
		resolved["hostSource"] = hostSource
		resolved["user"] = user
		missing = appendMissingToolchain(missing, toolchains, "ssh")
		missing = appendMissingToolchain(missing, toolchains, "scp")
	default:
		recs = append(recs, "当前平台无法生成完整实机命令；请先确认 platform。")
	}

	missing = uniqueStrings(missing)
	if len(missing) == 0 {
		recs = append(recs, "实机验证参数已齐；连接开发板后运行 realDeviceCommand，并把输出作为 evidence。")
	} else {
		recs = append(recs, "补齐 missingInputs 后再执行 realDeviceCommand；localOnlyCommand 可先验证编译/语法和证据链。")
	}

	report := deviceVerifyPlanReport{
		ProjectDir:          abs,
		Platform:            platform,
		Board:               board,
		ProjectTypes:        detected,
		ManifestPresent:     manifestResult.Status != "failed",
		LocalOnlyCommand:    shellJoin(localParts),
		RealDeviceCommand:   shellJoin(realParts),
		ReadyForRealDevice:  len(missing) == 0,
		MissingInputs:       missing,
		DetectedSerialPorts: serialPorts,
		DetectedBoards:      boards,
		DetectedDevices:     devices,
		Toolchains:          toolchains,
		Resolved:            resolved,
		MCPSteps:            deviceVerifyMCPSteps(platform, abs, board, resolved, baud, monitorSeconds),
		Recommendations:     recs,
	}
	return prettyJSON(report), nil
}

func placeholderIfEmpty(value, placeholder string) string {
	if strings.TrimSpace(value) == "" {
		return placeholder
	}
	return value
}

func firstDetectedFQBN(boards []boardReport) string {
	for _, board := range boards {
		if strings.TrimSpace(board.FQBN) != "" {
			return board.FQBN
		}
	}
	return ""
}

func chooseSerialPort(boards []boardReport, devices []deviceReport, serialPorts []string) string {
	for _, board := range boards {
		if strings.TrimSpace(board.Port) != "" {
			return board.Port
		}
	}
	for _, device := range devices {
		if strings.TrimSpace(device.Port) != "" {
			return device.Port
		}
	}
	if len(serialPorts) > 0 {
		return serialPorts[0]
	}
	return ""
}

func appendMissingToolchain(missing []string, toolchains []toolchainReport, name string) []string {
	if toolchainAvailable(toolchains, name) {
		return missing
	}
	return append(missing, "toolchain:"+name)
}

func toolchainAvailable(toolchains []toolchainReport, name string) bool {
	want := strings.ToLower(name)
	for _, tool := range toolchains {
		got := strings.ToLower(tool.Name)
		if tool.Available && (got == want || strings.Contains(got, want)) {
			return true
		}
	}
	return false
}

func inferPlatformIOEnv(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "platformio.ini"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[env:") && strings.HasSuffix(line, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "[env:"), "]")
		}
	}
	return ""
}

func sshTargetDefaults(platform, command string) (user, host string) {
	if u, h := parseSSHUserHost(command); h != "" {
		return u, h
	}
	return defaultSSHTarget(platform)
}

func defaultSSHTarget(platform string) (user, host string) {
	switch platform {
	case "unihiker_python":
		return "root", "10.1.2.3"
	case "maixcam_python":
		return "root", "maixcam-xxxx.local"
	case "raspberry_pi_python":
		return "pi", "raspberrypi.local"
	default:
		return "root", ""
	}
}

func sshHostNeedsConfirmation(platform, host, source string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.Contains(host, "xxxx") || strings.Contains(host, "<") || strings.Contains(host, "DEVICE_HOST") {
		return true
	}
	if source == "argument" {
		return false
	}
	_, defaultHost := defaultSSHTarget(platform)
	return strings.EqualFold(host, defaultHost)
}

func parseSSHUserHost(command string) (user, host string) {
	fields := strings.Fields(command)
	for _, field := range fields {
		if !strings.Contains(field, "@") || !strings.Contains(field, ":") {
			continue
		}
		before, _, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		u, h, ok := strings.Cut(before, "@")
		if !ok || strings.TrimSpace(h) == "" {
			continue
		}
		return strings.TrimSpace(u), strings.TrimSpace(h)
	}
	return "", ""
}

func deviceVerifyMCPSteps(platform, projectDir, board string, resolved map[string]string, baud, monitorSeconds int) []deviceVerifyPlanStep {
	steps := []deviceVerifyPlanStep{
		{Order: 1, Stage: "detect", Tool: "hardware_detect", Arguments: map[string]any{"project_dir": projectDir}, Notes: "确认工具链、串口和当前项目类型。"},
		{Order: 2, Stage: "audit", Tool: "hardware_project_audit", Arguments: map[string]any{"project_dir": projectDir, "platform": platform}, Notes: "先确认 manifest、接线说明、验证说明和检查清单完整。"},
		{Order: 3, Stage: "local", Tool: "hardware_project_validate", Arguments: localValidatePlanArgs(platform, projectDir, board, resolved), Notes: "只做本机编译或语法检查，不烧录。"},
		{Order: 4, Stage: "evidence", Tool: "hardware_evidence_record", Arguments: map[string]any{"project_dir": projectDir, "platform": platform, "board": board, "stage": localEvidenceStage(platform), "status": "passed", "summary": "local validation passed"}, Notes: "把本机验证输出写入 tests/hardware_evidence.jsonl。"},
	}
	nextOrder := 5
	for _, step := range realDevicePlanSteps(platform, projectDir, board, resolved, baud, monitorSeconds) {
		step.Order = nextOrder
		steps = append(steps, step)
		nextOrder++
	}
	steps = append(steps,
		deviceVerifyPlanStep{Order: nextOrder, Stage: "evidence", Tool: "hardware_evidence_record", Arguments: map[string]any{"project_dir": projectDir, "platform": platform, "board": board, "stage": "manual", "status": "passed", "summary": "real-device stage passed"}, Notes: "实际执行后把 stage 改成 upload、flash、monitor、mpremote 或 deploy，并粘贴真实输出。"},
		deviceVerifyPlanStep{Order: nextOrder + 1, Stage: "status", Tool: "hardware_evidence_status", Arguments: map[string]any{"project_dir": projectDir, "platform": platform}, Notes: "只有返回 hardware_verified 才算实机验证闭环。"},
	)
	return steps
}

func localValidatePlanArgs(platform, projectDir, board string, resolved map[string]string) map[string]any {
	args := map[string]any{"project_dir": projectDir, "platform": platform, "board": board}
	if v := resolved["fqbn"]; v != "" {
		args["fqbn"] = v
	}
	if v := resolved["environment"]; v != "" {
		args["environment"] = v
	}
	if v := resolved["target"]; v != "" {
		args["target"] = v
	}
	return args
}

func localEvidenceStage(platform string) string {
	switch platform {
	case "arduino", "platformio", "esp_idf":
		return "compile"
	default:
		return "syntax"
	}
}

func realDevicePlanSteps(platform, projectDir, board string, resolved map[string]string, baud, monitorSeconds int) []deviceVerifyPlanStep {
	switch platform {
	case "arduino":
		return []deviceVerifyPlanStep{
			{Stage: "upload", Tool: "arduino_upload", Arguments: map[string]any{"sketch_dir": arduinoSketchDirForPlan(projectDir), "fqbn": resolved["fqbn"], "port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT")}, Notes: "上传前确认端口没有被串口监视器占用。"},
			{Stage: "monitor", Tool: "arduino_monitor_sample", Arguments: map[string]any{"fqbn": resolved["fqbn"], "port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT"), "baud": baud, "seconds": monitorSeconds}, Notes: "至少采集到一段稳定 Serial 输出。"},
		}
	case "platformio":
		return []deviceVerifyPlanStep{
			{Stage: "upload", Tool: "platformio_run", Arguments: map[string]any{"project_dir": projectDir, "environment": resolved["environment"], "targets": []string{"upload"}, "upload_port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT")}},
			{Stage: "monitor", Tool: "platformio_run", Arguments: map[string]any{"project_dir": projectDir, "environment": resolved["environment"], "targets": []string{"monitor"}, "monitor_port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT"), "timeout_seconds": monitorSeconds + 10}},
		}
	case "esp_idf":
		return []deviceVerifyPlanStep{
			{Stage: "flash", Tool: "esp_idf_run", Arguments: map[string]any{"project_dir": projectDir, "action": "flash", "target": resolved["target"], "port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT"), "baud": baud}},
			{Stage: "monitor", Tool: "esp_idf_run", Arguments: map[string]any{"project_dir": projectDir, "action": "monitor", "port": placeholderIfEmpty(resolved["port"], "SERIAL_PORT"), "baud": baud, "timeout_seconds": monitorSeconds + 10}},
		}
	case "micropython":
		return []deviceVerifyPlanStep{
			{Stage: "mpremote", Tool: "mpremote_run", Arguments: map[string]any{"script": pythonScriptForPlan(projectDir), "device": resolved["device"]}, Notes: "如果 device=auto 选错板，请改成具体串口。"},
		}
	case "unihiker_python":
		// 行空板:GPIO 用 pinpong、UI 用 unihiker;默认 root 登录,只需部署 src。
		host := placeholderIfEmpty(resolved["host"], "DEVICE_HOST")
		user := deployUser(platform, resolved["user"])
		remoteDir := remoteDeployDir(user, projectDir)
		return []deviceVerifyPlanStep{
			{Stage: "deploy", Tool: "ssh_deploy_run", Arguments: map[string]any{"host": host, "user": user, "local_path": filepath.Join(projectDir, "src"), "remote_path": remoteDir, "recursive": true, "command": "python3 " + remoteDir + "/main.py"}, Notes: "行空板多为密码登录(默认 root/dfrobot),传 password 参数;GPIO 用 pinpong、屏幕 UI 用 unihiker,部署前确认设备已装这两个库。"},
		}
	case "maixcam_python":
		// MaixCAM(K230):视觉项目的模型(models/*.mud/.onnx)和素材(assets/)必须一起上板,
		// 只传 src 会让推理时找不到模型文件 —— 所以部署整个项目目录,命令指向 src/main.py。
		host := placeholderIfEmpty(resolved["host"], "DEVICE_HOST")
		user := deployUser(platform, resolved["user"])
		remoteDir := remoteDeployDir(user, projectDir)
		return []deviceVerifyPlanStep{
			{Stage: "deploy", Tool: "ssh_deploy_run", Arguments: map[string]any{"host": host, "user": user, "local_path": projectDir, "remote_path": remoteDir, "recursive": true, "command": "python3 " + remoteDir + "/src/main.py"}, Notes: "MaixCAM(K230)要连 models/(.mud/.onnx)和 assets/ 一起部署,只传 src 上板会缺模型;默认 root 登录,代码用 MaixPy v4 的 maix.* API(不是 K210 老 API)。"},
		}
	case "raspberry_pi_python":
		// 树莓派默认 pi 用户,家目录 /home/pi(写不进 /root);部署 src 即可。
		host := placeholderIfEmpty(resolved["host"], "DEVICE_HOST")
		user := deployUser(platform, resolved["user"])
		remoteDir := remoteDeployDir(user, projectDir)
		return []deviceVerifyPlanStep{
			{Stage: "deploy", Tool: "ssh_deploy_run", Arguments: map[string]any{"host": host, "user": user, "local_path": filepath.Join(projectDir, "src"), "remote_path": remoteDir, "recursive": true, "command": "python3 " + remoteDir + "/main.py"}, Notes: "树莓派默认 pi 用户(家目录 /home/pi),不是 root;先确认设备已装依赖(gpiozero / opencv-python / picamera2);CSI 排线摄像头用 picamera2,USB 摄像头才用 cv2.VideoCapture(0)。"},
		}
	default:
		return nil
	}
}

// deployUser 选定 SSH 部署用户:优先用已解析的用户,空则回退到该平台默认(树莓派=pi、其余=root)。
func deployUser(platform, resolvedUser string) string {
	if u := strings.TrimSpace(resolvedUser); u != "" {
		return u
	}
	if u, _ := defaultSSHTarget(platform); u != "" {
		return u
	}
	return "root"
}

// remoteDeployDir 按登录用户推断远端部署目录:root 用 /root,其它用户(如树莓派的 pi)用 /home/<user>。
// 避免把项目部署到 pi 用户写不进的 /root 导致权限失败。
func remoteDeployDir(user, projectDir string) string {
	base := "/root/reasonix"
	if u := strings.TrimSpace(user); u != "" && u != "root" {
		base = "/home/" + u + "/reasonix"
	}
	return base + "/" + filepath.Base(projectDir)
}

func arduinoSketchDirForPlan(projectDir string) string {
	matches, _ := filepath.Glob(filepath.Join(projectDir, "*", "*.ino"))
	if len(matches) > 0 {
		return filepath.Dir(matches[0])
	}
	matches, _ = filepath.Glob(filepath.Join(projectDir, "*.ino"))
	if len(matches) > 0 {
		return filepath.Dir(matches[0])
	}
	return projectDir
}

func pythonScriptForPlan(projectDir string) string {
	preferred := filepath.Join(projectDir, "src", "main.py")
	if exists(preferred) {
		return preferred
	}
	return filepath.Join(projectDir, "main.py")
}

func readEvidenceRecords(path string) ([]evidenceRecord, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	records := []evidenceRecord{}
	invalid := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record evidenceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			invalid++
			continue
		}
		records = append(records, record)
	}
	return records, invalid, nil
}

func projectFingerprint(projectDir string) (string, int, error) {
	files := []string{}
	err := filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != projectDir && d.IsDir() && shouldSkipProjectDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !fingerprintEligibleFile(rel) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, path := range files {
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return "", 0, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", 0, err
		}
		h.Write([]byte(filepath.ToSlash(rel)))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), len(files), nil
}

func fingerprintEligibleFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch rel {
	case "tests/hardware_evidence.jsonl", "tests/hardware_checklist.md", ".DS_Store":
		return false
	}
	base := filepath.Base(rel)
	switch base {
	case "CMakeLists.txt", "Makefile", "platformio.ini", "sdkconfig", "sdkconfig.defaults", "requirements.txt", "hardware_manifest.json", "README.md":
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".ino", ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".py", ".cmake", ".md", ".json", ".ini", ".toml", ".yaml", ".yml", ".txt":
		return true
	default:
		return false
	}
}

func partitionEvidenceByFingerprint(records []evidenceRecord, fingerprint string) ([]evidenceRecord, []evidenceRecord) {
	current := []evidenceRecord{}
	stale := []evidenceRecord{}
	for _, record := range records {
		if record.ProjectFingerprint == fingerprint {
			current = append(current, record)
			continue
		}
		stale = append(stale, record)
	}
	return current, stale
}

func summarizeEvidenceStages(records []evidenceRecord) []evidenceStageSummary {
	byStage := map[string]*evidenceStageSummary{}
	for _, record := range records {
		stage := record.Stage
		if stage == "" {
			stage = "unknown"
		}
		item := byStage[stage]
		if item == nil {
			item = &evidenceStageSummary{Stage: stage}
			byStage[stage] = item
		}
		switch record.Status {
		case "passed":
			item.Passed++
		case "failed":
			item.Failed++
		case "skipped":
			item.Skipped++
		case "info":
			item.Info++
		}
		if record.TimestampUTC >= item.LatestAt {
			item.LatestAt = record.TimestampUTC
			item.LatestStatus = record.Status
			item.LatestSummary = record.Summary
		}
	}
	keys := make([]string, 0, len(byStage))
	for key := range byStage {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]evidenceStageSummary, 0, len(keys))
	for _, key := range keys {
		out = append(out, *byStage[key])
	}
	return out
}

func evidenceRequiredGroups(platform string) []evidenceRequiredGroup {
	switch platform {
	case "arduino":
		return []evidenceRequiredGroup{
			{Name: "local_compile", Stages: []string{"compile"}, RequiredFor: "local"},
			{Name: "device_upload", Stages: []string{"upload"}, RequiredFor: "hardware"},
			{Name: "runtime_log", Stages: []string{"monitor", "serial"}, RequiredFor: "hardware"},
		}
	case "platformio":
		return []evidenceRequiredGroup{
			{Name: "local_compile", Stages: []string{"compile"}, RequiredFor: "local"},
			{Name: "device_upload", Stages: []string{"upload"}, RequiredFor: "hardware"},
			{Name: "runtime_log", Stages: []string{"monitor", "serial"}, RequiredFor: "hardware"},
		}
	case "esp_idf":
		return []evidenceRequiredGroup{
			{Name: "local_build", Stages: []string{"compile"}, RequiredFor: "local"},
			{Name: "device_flash", Stages: []string{"flash"}, RequiredFor: "hardware"},
			{Name: "runtime_log", Stages: []string{"monitor", "serial"}, RequiredFor: "hardware"},
		}
	case "micropython":
		return []evidenceRequiredGroup{
			{Name: "local_syntax", Stages: []string{"syntax"}, RequiredFor: "local"},
			{Name: "device_run", Stages: []string{"mpremote"}, RequiredFor: "hardware"},
		}
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return []evidenceRequiredGroup{
			{Name: "local_syntax", Stages: []string{"syntax"}, RequiredFor: "local"},
			{Name: "device_deploy", Stages: []string{"ssh", "deploy"}, RequiredFor: "hardware"},
		}
	default:
		return []evidenceRequiredGroup{
			{Name: "local_validation", Stages: []string{"compile", "syntax"}, RequiredFor: "local"},
			{Name: "runtime_evidence", Stages: []string{"upload", "flash", "monitor", "serial", "mpremote", "ssh", "deploy", "manual"}, RequiredFor: "hardware"},
		}
	}
}

func evidenceGroupPassed(records []evidenceRecord, stages []string) bool {
	for _, record := range records {
		if record.Status == "passed" && contains(stages, record.Stage) && evidenceRecordStrongEnough(record) {
			return true
		}
	}
	return false
}

// --- 真实执行日志(#2-3:把"真机证据"绑定到 MCP 真实跑过的工具,防伪造)---
//
// MCP 是常驻进程:真正执行外部命令的动作工具(arduino_compile/upload/monitor、
// platformio_run、esp_idf_run、mpremote_run、ssh_deploy_run、hardware_project_validate)
// 把它们的【真实输出】登记到这里。hardware_evidence_record 记录"真机"阶段证据时,会拿
// 模型填的 output 与这份日志比对——只有与某次覆盖该阶段的真实执行共享一行可辨识文本,
// 才算 hostObserved。模型凭空捏造的串口/SSH 输出对不上任何真实执行,不再能推到
// hardware_verified。这是"灵活性归模型,保证归系统"的技术兑现。
type hostExecution struct {
	stages map[string]bool
	output string
}

var (
	hostExecMu sync.Mutex
	hostExecs  []hostExecution
)

// recordHostExecution 登记一次真实工具执行的输出与它覆盖的证据阶段(保留最近 60 次)。
func recordHostExecution(output string, stages ...string) {
	out := strings.TrimSpace(output)
	if out == "" || len(stages) == 0 {
		return
	}
	sm := make(map[string]bool, len(stages))
	for _, s := range stages {
		sm[s] = true
	}
	hostExecMu.Lock()
	hostExecs = append(hostExecs, hostExecution{stages: sm, output: out})
	if len(hostExecs) > 60 {
		hostExecs = hostExecs[len(hostExecs)-60:]
	}
	hostExecMu.Unlock()
}

// recordToolExecution 在 dispatch 调用某动作工具后,按工具(+ action 参数)映射出它覆盖
// 的证据阶段,把真实输出登记进执行日志。统一一处挂钩,不必改每个动作函数。
func recordToolExecution(name string, args map[string]any, output string) {
	var stages []string
	switch name {
	case "arduino_compile":
		stages = []string{"compile"}
	case "arduino_upload", "arduino_ota_upload":
		stages = []string{"upload"}
	case "arduino_monitor_sample":
		stages = []string{"monitor", "serial"}
	case "platformio_run":
		stages = []string{"compile", "upload", "flash", "monitor", "serial"}
	case "esp_idf_run":
		stages = []string{"compile", "flash", "upload", "monitor", "serial"}
	case "mpremote_run":
		stages = []string{"mpremote", "monitor", "serial"}
	case "ssh_deploy_run":
		stages = []string{"ssh", "deploy", "monitor", "serial"}
	case "hardware_project_validate":
		stages = []string{"compile", "syntax"}
	default:
		return
	}
	recordHostExecution(output, stages...)
}

// distinctiveLines 取 output 里足够独特(>=4 个非空白字符)的行,用于跨执行内容比对。
// 阈值取 4:真实串口行常很短(如 "led:on"、"ready"、"temp=25"),太高会漏掉真证据;
// 太低(如 1-2 字符的 "ok"/"on")又易误配。比对要求整行逐字出现在某次真实执行输出里。
func distinctiveLines(output string) []string {
	var out []string
	for _, ln := range strings.Split(output, "\n") {
		t := strings.TrimSpace(ln)
		if len([]rune(t)) >= 4 {
			out = append(out, t)
		}
	}
	return out
}

// hostObservedFor 判断模型记录的 output 是否对应某次覆盖该 stage 的真实执行:二者需共享
// 至少一行可辨识文本。模型若如实粘贴真实工具输出即命中;凭空捏造则对不上。
func hostObservedFor(stage, output string) bool {
	lines := distinctiveLines(output)
	if len(lines) == 0 {
		return false
	}
	hostExecMu.Lock()
	defer hostExecMu.Unlock()
	for _, e := range hostExecs {
		if !e.stages[stage] {
			continue
		}
		for _, ln := range lines {
			if strings.Contains(e.output, ln) {
				return true
			}
		}
	}
	return false
}

// hostExecutionHappened 判断本进程是否真的跑过一次覆盖该 stage 的动作工具(不看输出内容,
// 只看"真发生过")。用于 upload/flash 这类"重点是真烧录过、而非输出逐行可比"的阶段。
func hostExecutionHappened(stage string) bool {
	hostExecMu.Lock()
	defer hostExecMu.Unlock()
	for _, e := range hostExecs {
		if e.stages[stage] {
			return true
		}
	}
	return false
}

// hardwareExecutionStage 标记"必须由真实设备执行支撑"的证据阶段(伪造重灾区)。
func hardwareExecutionStage(stage string) bool {
	switch stage {
	case "upload", "flash", "monitor", "serial", "mpremote", "ssh", "deploy":
		return true
	default:
		return false
	}
}

// computeHostObserved 按阶段决定证据是否"有真实执行支撑":运行/串口类要求输出对得上真实
// 执行(防伪造日志);烧录类只要求真跑过一次烧录(输出常无可比内容)。
func computeHostObserved(stage, output string) bool {
	switch stage {
	case "upload", "flash":
		return hostExecutionHappened(stage)
	case "monitor", "serial", "mpremote", "ssh", "deploy":
		return hostObservedFor(stage, output)
	default:
		return false
	}
}

func evidenceRecordStrongEnough(record evidenceRecord) bool {
	// 真机执行类阶段(烧录/串口/SSH 运行等)除了要有输出体,还必须 hostObserved——即记录的
	// 输出对得上 MCP 真实跑过的对应工具。否则模型可以凭空填一段串口输出就推到 hardware_verified(#2-3)。
	if hardwareExecutionStage(record.Stage) && !record.HostObserved {
		return false
	}
	switch record.Stage {
	case "monitor", "serial", "mpremote", "ssh", "deploy":
		return commandOutputHasBody(record.OutputExcerpt)
	case "upload", "flash":
		return strings.TrimSpace(record.Command) != "" || strings.TrimSpace(record.OutputExcerpt) != "" || strings.TrimSpace(record.ArtifactPath) != ""
	case "manual":
		return strings.TrimSpace(record.Command) != "" && commandOutputHasBody(record.OutputExcerpt)
	default:
		return true
	}
}

func evidenceOverallStatus(records []evidenceRecord, required []evidenceRequiredGroup) string {
	if len(records) == 0 {
		return "no_evidence"
	}
	for _, record := range records {
		if record.Status == "failed" {
			return "failed"
		}
	}
	localPassed := true
	hardwarePassed := true
	hardwareRequired := false
	for _, group := range required {
		if group.RequiredFor == "hardware" {
			hardwareRequired = true
			if !group.Passed {
				hardwarePassed = false
			}
			continue
		}
		if !group.Passed {
			localPassed = false
		}
	}
	if localPassed && (!hardwareRequired || hardwarePassed) {
		return "hardware_verified"
	}
	if localPassed {
		return "hardware_pending"
	}
	return "local_pending"
}

func evidenceStatusSummary(status string, count int, missing []string) string {
	switch status {
	case "hardware_verified":
		return fmt.Sprintf("%d evidence records; local validation and real-hardware stages are recorded.", count)
	case "hardware_pending":
		return fmt.Sprintf("%d evidence records; local validation is recorded, real-hardware evidence is missing: %s.", count, strings.Join(missing, ", "))
	case "local_pending":
		return fmt.Sprintf("%d evidence records; local validation evidence is incomplete: %s.", count, strings.Join(missing, ", "))
	case "failed":
		return fmt.Sprintf("%d evidence records; at least one verification stage failed.", count)
	case "stale":
		return fmt.Sprintf("%d evidence records; all records belong to an older project fingerprint, so current code is not verified.", count)
	default:
		return fmt.Sprintf("%d evidence records; verification status is not proven.", count)
	}
}

func evidenceStatusRecommendations(status, platform string, missing []string, invalid, stale int) []string {
	recs := []string{}
	if invalid > 0 {
		recs = append(recs, fmt.Sprintf("hardware_evidence.jsonl contains %d invalid record(s); fix or remove malformed lines.", invalid))
	}
	if stale > 0 {
		recs = append(recs, fmt.Sprintf("%d evidence record(s) are stale because the project fingerprint changed; rerun local and real-device verification after the latest code/config edits.", stale))
	}
	switch status {
	case "no_evidence":
		recs = append(recs, "运行 hardware_project_validate 后调用 hardware_evidence_record 记录本机验证证据。")
	case "local_pending":
		recs = append(recs, "先补齐本机编译或语法检查证据，再连接真实硬件。")
	case "hardware_pending":
		recs = append(recs, "本机验证已有证据；继续连接真实硬件，完成上传/烧录/部署和运行日志采集。")
	case "failed":
		recs = append(recs, "先修复 failed 证据对应的问题，再重新验证并追加新的 evidence。")
	case "hardware_verified":
		recs = append(recs, "真实硬件验证证据已齐；如代码或接线变化，重新运行验证并追加 evidence。")
	case "stale":
		recs = append(recs, "当前代码或项目配置已变化；先重新运行 hardware_project_validate 并记录本机 evidence，再连接真实硬件重新记录上传/烧录/运行日志。")
	}
	if len(missing) > 0 {
		recs = append(recs, "缺失阶段组："+strings.Join(missing, ", "))
	}
	if platform == "esp_idf" && contains(missing, "device_flash") {
		recs = append(recs, "ESP-IDF 项目连接 ESP32 后运行 esp_idf_run action=flash_monitor，并记录 flash 与 monitor evidence。")
	}
	return uniqueStrings(recs)
}

func requireAllowed(name, value string, allowed []string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("unsupported %s %q; expected one of %s", name, value, strings.Join(allowed, ", "))
}

func appendTextFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body)
	return err
}

func appendEvidenceToChecklist(path string, record evidenceRecord) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := strings.TrimRight(string(data), "\n")
	var b strings.Builder
	b.WriteString(body)
	if !strings.Contains(body, "## 验证证据记录") {
		b.WriteString("\n\n## 验证证据记录\n\n")
		b.WriteString("使用 `hardware_evidence_record` 追加 compile、upload、monitor、ssh 等证据；结构化日志保存在 `tests/hardware_evidence.jsonl`。\n")
	}
	b.WriteString("\n\n")
	b.WriteString("### ")
	b.WriteString(record.TimestampUTC)
	b.WriteString(" - ")
	b.WriteString(record.Stage)
	b.WriteString(" - ")
	b.WriteString(record.Status)
	b.WriteString("\n\n")
	b.WriteString("- 平台：")
	b.WriteString(emptyAsUnknown(record.Platform))
	b.WriteString("\n")
	if record.Board != "" {
		b.WriteString("- 开发板：")
		b.WriteString(record.Board)
		b.WriteString("\n")
	}
	if record.Port != "" {
		b.WriteString("- 端口/设备：")
		b.WriteString(record.Port)
		b.WriteString("\n")
	}
	if record.Command != "" {
		b.WriteString("- 命令：")
		b.WriteString(record.Command)
		b.WriteString("\n")
	}
	if record.ArtifactPath != "" {
		b.WriteString("- 证据文件：")
		b.WriteString(record.ArtifactPath)
		b.WriteString("\n")
	}
	b.WriteString("- 结果：")
	b.WriteString(record.Summary)
	b.WriteString("\n")
	if record.OutputExcerpt != "" {
		b.WriteString("\n~~~text\n")
		b.WriteString(strings.ReplaceAll(record.OutputExcerpt, "~~~", "~~~ "))
		b.WriteString("\n~~~\n")
	}
	b.WriteString("\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func emptyAsUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func truncateText(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if max <= 0 || len(runes) <= max {
		return value
	}
	if max <= 20 {
		return string(runes[:max])
	}
	return string(runes[:max-20]) + "\n... output truncated ..."
}

func auditManifestFile(projectDir string) (hardwareManifestAuditData, auditResult) {
	path := filepath.Join(projectDir, "hardware_manifest.json")
	result := auditResult{Kind: "manifest", Target: path, Status: "passed", Message: "hardware_manifest.json is complete enough for AI hardware workflows."}
	var manifest hardwareManifestAuditData
	data, err := os.ReadFile(path)
	if err != nil {
		result.Status = "failed"
		result.Message = "缺少 hardware_manifest.json，AI 无法稳定判断平台、入口文件和验证链路。"
		result.Missing = []string{"hardware_manifest.json"}
		return manifest, result
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		result.Status = "failed"
		result.Message = "hardware_manifest.json 不是合法 JSON。"
		result.Missing = []string{"valid_json"}
		return manifest, result
	}
	missing := []string{}
	if manifest.SchemaVersion != "reasonix-hardware-project/v1" {
		missing = append(missing, "schemaVersion=reasonix-hardware-project/v1")
	}
	if strings.TrimSpace(manifest.Platform) == "" {
		missing = append(missing, "platform")
	}
	if strings.TrimSpace(manifest.Board) == "" {
		missing = append(missing, "board")
	}
	if len(manifest.Entrypoints) == 0 {
		missing = append(missing, "entrypoints")
	}
	if !contains(manifest.PreferredProtocols, "UART") {
		missing = append(missing, "preferredProtocols.UART")
	}
	if len(manifest.Connections) == 0 {
		missing = append(missing, "connections")
	}
	if !contains(manifest.MCPTools, "hardware_detect") {
		missing = append(missing, "mcpTools.hardware_detect")
	}
	if !contains(manifest.MCPTools, "hardware_project_audit") {
		missing = append(missing, "mcpTools.hardware_project_audit")
	}
	if !contains(manifest.MCPTools, "hardware_project_validate") {
		missing = append(missing, "mcpTools.hardware_project_validate")
	}
	if !contains(manifest.MCPTools, "hardware_evidence_record") {
		missing = append(missing, "mcpTools.hardware_evidence_record")
	}
	if !contains(manifest.MCPTools, "hardware_evidence_status") {
		missing = append(missing, "mcpTools.hardware_evidence_status")
	}
	if !contains(manifest.MCPTools, "hardware_device_verify_plan") {
		missing = append(missing, "mcpTools.hardware_device_verify_plan")
	}
	if manifest.Verification.LocalCommand == "" {
		missing = append(missing, "verification.localCommand")
	}
	if manifest.Verification.MinimumLocalGate != "compile_or_syntax" {
		missing = append(missing, "verification.minimumLocalGate=compile_or_syntax")
	}
	if len(manifest.Verification.RealHardwareChecks) == 0 {
		missing = append(missing, "verification.realHardwareChecks")
	}
	if len(missing) > 0 {
		result.Status = "failed"
		result.Message = "hardware_manifest.json 缺少硬件项目必需字段。"
		result.Missing = missing
	}
	return manifest, result
}

func auditExpectedPlatform(override, manifestPlatform string, detected []string) string {
	if override != "" && override != "auto" {
		return override
	}
	if manifestPlatform != "" {
		return manifestPlatform
	}
	for _, typ := range detected {
		if typ != "unknown" && typ != "python_or_micropython" {
			return typ
		}
	}
	if contains(detected, "python_or_micropython") {
		return "micropython"
	}
	return "unknown"
}

func auditProjectType(projectDir, expected string, detected []string) auditResult {
	result := auditResult{Kind: "project_type", Target: projectDir, Status: "passed", Message: "项目类型和 manifest/参数一致。"}
	if expected == "" || expected == "unknown" {
		result.Status = "failed"
		result.Message = "未识别硬件项目类型。"
		result.Missing = []string{"platformio.ini / CMakeLists.txt / .ino / src/main.py"}
		return result
	}
	if !platformMatchesDetected(expected, detected) {
		result.Status = "warning"
		result.Message = "manifest 或参数指定的平台与自动检测结果不完全一致，请确认项目类型。"
		result.Missing = []string{"detected:" + strings.Join(detected, ",")}
	}
	return result
}

func auditProjectLayout(projectDir, platform string) []auditResult {
	switch platform {
	case "platformio":
		return []auditResult{auditPlatformIOLayout(projectDir)}
	default:
		return nil
	}
}

func auditPlatformIOLayout(projectDir string) auditResult {
	target := filepath.Join(projectDir, "platformio.ini")
	result := auditResult{
		Kind:    "project_layout",
		Target:  target,
		Status:  "passed",
		Message: "PlatformIO 项目结构可编译：已找到 src/ 下的入口源码。",
	}
	if !exists(target) {
		result.Status = "failed"
		result.Message = "缺少 platformio.ini，不能按 PlatformIO 项目验证。"
		result.Missing = []string{"platformio.ini"}
		return result
	}
	srcFiles := findPlatformIOSourceFiles(projectDir)
	rootSketches := findRootINOSketches(projectDir)
	if len(srcFiles) == 0 {
		result.Status = "failed"
		result.Message = "检测到 platformio.ini，但 src/ 下没有可编译入口；根目录 .ino 不会被当前 PlatformIO 构建当作入口。"
		result.Missing = []string{"src/main.cpp 或 src/*.cpp"}
		for _, sketch := range rootSketches {
			result.Missing = append(result.Missing, "root .ino ignored by PlatformIO: "+filepath.Base(sketch))
		}
		return result
	}
	if len(rootSketches) > 0 {
		result.Status = "warning"
		result.Message = "PlatformIO 项目已找到 src/ 入口，但根目录仍有 .ino；模型可能改错文件，建议只保留一个入口。"
		for _, sketch := range rootSketches {
			result.Missing = append(result.Missing, filepath.Base(sketch))
		}
	}
	return result
}

func platformMatchesDetected(platform string, detected []string) bool {
	if contains(detected, platform) {
		return true
	}
	switch platform {
	case "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return contains(detected, "python_or_micropython")
	default:
		return false
	}
}

func auditRequiredHardwareFiles(projectDir string) []auditResult {
	checks := map[string][]string{
		"README.md":                   {"教学要求", "验证命令", "hardware_manifest.json"},
		"docs/board_profile.md":       {"板卡 Profile", "风险引脚", "验证流程"},
		"docs/failure_patterns.md":    {"硬件失败模式", "自动修复", "需要证据"},
		"docs/wiring.md":              {"UART", "I2C", "hardware_manifest.json", "Manifest 连接清单"},
		"docs/verification.md":        {"hardware_detect", "hardware_project_validate", "hardware_evidence_record", "失败判断"},
		"tests/hardware_checklist.md": {"真实硬件验证", "学生答辩检查", "hardware_project_validate", "hardware_evidence_record"},
	}
	results := make([]auditResult, 0, len(checks))
	keys := make([]string, 0, len(checks))
	for rel := range checks {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		results = append(results, auditTextFile(projectDir, rel, checks[rel]))
	}
	return results
}

func auditConnections(projectDir, platform string, connections []hardwareConnection) []auditResult {
	target := filepath.Join(projectDir, "hardware_manifest.json")
	if len(connections) == 0 {
		return []auditResult{{
			Kind:    "connections",
			Target:  target,
			Status:  "failed",
			Message: "hardware_manifest.json 缺少 connections，AI 无法审计引脚、协议和电压风险。",
			Missing: []string{"connections"},
		}}
	}

	results := []auditResult{}
	pinUsers := map[string][]string{}
	pinProtocols := map[string]map[string]bool{}
	for i, conn := range connections {
		result := auditResult{
			Kind:    "connection",
			Target:  fmt.Sprintf("%s:connections[%d]", target, i),
			Status:  "passed",
			Message: conn.Name + " connection metadata is complete.",
		}
		if strings.TrimSpace(conn.Name) == "" {
			result.Missing = append(result.Missing, "name")
		}
		if strings.TrimSpace(conn.Role) == "" {
			result.Missing = append(result.Missing, "role")
		}
		protocol := normalizeProtocol(conn.Protocol)
		if protocol == "" {
			result.Missing = append(result.Missing, "protocol")
		}
		if strings.TrimSpace(conn.Voltage) == "" {
			result.Missing = append(result.Missing, "voltage")
		}
		if protocolRequiresPins(protocol) && len(conn.Pins) == 0 {
			result.Missing = append(result.Missing, "pins")
		}
		if len(result.Missing) > 0 {
			result.Status = "failed"
			result.Message = "connection 缺少必要字段，无法用于真实接线审计。"
		} else if has3V3Logic(platform) && isFiveVoltLogic(conn) {
			result.Status = "warning"
			result.Message = "3.3V 平台存在 5V 逻辑连接风险；请确认使用 3.3V 模块或电平转换。"
			result.Missing = []string{"level_shifter_or_3.3V_module"}
		}
		results = append(results, result)

		for _, pin := range conn.Pins {
			norm := normalizePin(pin)
			if norm == "" || shareablePin(norm) {
				continue
			}
			pinUsers[norm] = append(pinUsers[norm], conn.Name)
			if pinProtocols[norm] == nil {
				pinProtocols[norm] = map[string]bool{}
			}
			pinProtocols[norm][protocol] = true
		}
	}
	for pin, users := range pinUsers {
		uniqueUsers := uniqueStrings(users)
		if len(uniqueUsers) < 2 || duplicatePinAllowed(pinProtocols[pin]) {
			continue
		}
		results = append(results, auditResult{
			Kind:    "pin_conflict",
			Target:  target,
			Status:  "warning",
			Message: "同一个信号引脚被多个连接复用，真实接线前需要确认不是冲突。",
			Missing: []string{pin + ":" + strings.Join(uniqueUsers, ",")},
		})
	}
	return results
}

func normalizeProtocol(protocol string) string {
	return strings.ToUpper(strings.TrimSpace(protocol))
}

func protocolRequiresPins(protocol string) bool {
	switch protocol {
	case "GPIO", "PWM", "ADC", "I2C", "SPI", "UART":
		return true
	default:
		return false
	}
}

func has3V3Logic(platform string) bool {
	switch platform {
	case "platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return true
	default:
		return false
	}
}

func isFiveVoltLogic(conn hardwareConnection) bool {
	voltage := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(conn.Voltage), " ", ""))
	if voltage != "5V" && voltage != "5.0V" {
		return false
	}
	protocol := normalizeProtocol(conn.Protocol)
	switch protocol {
	case "USB", "USB_OR_WIFI", "USB_C_OR_GPIO_POWER":
		return false
	default:
		return true
	}
}

func normalizePin(pin string) string {
	pin = strings.ToUpper(strings.TrimSpace(pin))
	pin = strings.ReplaceAll(pin, " ", "")
	if strings.Contains(pin, "/") {
		pin = strings.TrimSpace(strings.Split(pin, "/")[0])
	}
	return pin
}

func shareablePin(pin string) bool {
	switch pin {
	case "", "USB", "USB-C", "WIFI", "GND", "3V3", "3.3V", "5V", "VCC", "VIN":
		return true
	default:
		return false
	}
}

func duplicatePinAllowed(protocols map[string]bool) bool {
	if len(protocols) == 0 {
		return true
	}
	return len(protocols) == 1 && protocols["I2C"]
}

func auditTextFile(projectDir, rel string, needles []string) auditResult {
	path := filepath.Join(projectDir, filepath.FromSlash(rel))
	result := auditResult{Kind: "file", Target: path, Status: "passed", Message: rel + " exists and contains expected hardware workflow text."}
	data, err := os.ReadFile(path)
	if err != nil {
		result.Status = "failed"
		result.Message = "缺少 " + rel + "。"
		result.Missing = []string{rel}
		return result
	}
	body := string(data)
	for _, needle := range needles {
		if !strings.Contains(body, needle) {
			result.Missing = append(result.Missing, needle)
		}
	}
	if len(result.Missing) > 0 {
		result.Status = "failed"
		result.Message = rel + " 内容不完整，缺少硬件工作流关键文本。"
	}
	return result
}

func auditEntrypoints(projectDir, platform, projectName string, manifestEntrypoints []string) []auditResult {
	entrypoints := manifestEntrypoints
	if len(entrypoints) == 0 && platform != "" && platform != "unknown" {
		entrypoints = platformEntrypoints(platform, projectName)
	}
	if len(entrypoints) == 0 {
		return []auditResult{{
			Kind:    "entrypoint",
			Target:  projectDir,
			Status:  "failed",
			Message: "没有可审计的入口文件。",
			Missing: []string{"entrypoints"},
		}}
	}
	results := make([]auditResult, 0, len(entrypoints))
	for _, rel := range entrypoints {
		path := filepath.Join(projectDir, filepath.FromSlash(rel))
		result := auditResult{Kind: "entrypoint", Target: path, Status: "passed", Message: rel + " exists."}
		if !exists(path) {
			result.Status = "failed"
			result.Message = "缺少入口文件 " + rel + "。"
			result.Missing = []string{rel}
		}
		results = append(results, result)
	}
	return results
}

func auditVerificationCommand(projectDir, platform, board, projectName, manifestCommand string) auditResult {
	want := verificationCommand(platform, board, projectName)
	result := auditResult{Kind: "verification_command", Target: filepath.Join(projectDir, "hardware_manifest.json"), Status: "passed", Message: "manifest verification command matches the platform defaults."}
	if manifestCommand == "" {
		result.Status = "failed"
		result.Message = "manifest 缺少 verification.localCommand。"
		result.Missing = []string{want}
		return result
	}
	if manifestCommand != want {
		result.Status = "warning"
		result.Message = "manifest verification command 与当前平台默认命令不同；如果是手动改过的项目，请确认是否符合真实板卡。"
		result.Missing = []string{want}
	}
	return result
}

func auditSummary(results []auditResult) string {
	passed, failed, warnings := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "passed":
			passed++
		case "failed":
			failed++
		case "warning":
			warnings++
		}
	}
	return fmt.Sprintf("%d passed, %d failed, %d warnings", passed, failed, warnings)
}

func auditRecommendations(results []auditResult) []string {
	recs := []string{}
	for _, r := range results {
		switch r.Kind {
		case "manifest":
			if r.Status == "failed" {
				recs = append(recs, "补齐 hardware_manifest.json；现有项目优先调用 hardware_project_context，空项目可用 hardware_project_scaffold 生成标准结构。")
			}
		case "file":
			if r.Status == "failed" {
				recs = append(recs, "调用 hardware_project_context 补齐 README、docs/wiring.md、docs/verification.md、docs/board_profile.md、docs/failure_patterns.md 和 tests/hardware_checklist.md，保证 AI 后续修改有接线、板卡和验证上下文。")
			}
		case "entrypoint":
			if r.Status == "failed" {
				recs = append(recs, "补齐平台入口文件，再运行 hardware_project_validate。")
			}
		case "project_type":
			if r.Status != "passed" {
				recs = append(recs, "确认 manifest.platform、项目文件和实际开发板平台一致。")
			}
		case "connections", "connection", "pin_conflict":
			if r.Status != "passed" {
				recs = append(recs, "更新 hardware_manifest.json 的 connections，并同步 docs/wiring.md；真实接线前确认引脚、电压和协议没有冲突。")
			}
		case "verification_command":
			if r.Status == "warning" {
				recs = append(recs, "确认 verification.localCommand 是否是为当前板卡定制；如果不是，改回平台默认验证命令。")
			}
		case "project_layout":
			if r.Status == "failed" {
				recs = append(recs, "修正 PlatformIO 项目结构：把根目录 .ino 迁移为 src/main.cpp（保留 setup/loop，补充 #include <Arduino.h>），或删除 platformio.ini 改用 Arduino CLI；修完后重新运行 hardware_project_validate。")
			} else if r.Status == "warning" {
				recs = append(recs, "清理重复入口：PlatformIO 项目只保留 src/ 下的主程序，避免模型继续修改根目录 .ino 死文件。")
			}
		}
	}
	return uniqueStrings(recs)
}

func projectTypesForValidation(platform string, detected []string) []string {
	if platform == "" || platform == "auto" {
		return detected
	}
	return []string{platform}
}

func validateArduinoProject(projectDir string, args map[string]any, timeout time.Duration) []validationResult {
	sketchDirs := findRootArduinoSketchDirs(projectDir)
	if len(sketchDirs) == 0 {
		nested := findArduinoSketchDirs(projectDir)
		if len(nested) > 0 {
			return []validationResult{{
				Kind:     "arduino_project_boundary",
				Target:   projectDir,
				Status:   "failed",
				NextStep: fmt.Sprintf("当前目录包含子 Arduino sketch,但它本身不是单一 sketch 根目录；请切换到具体项目目录后再编译，例如：%s。", formatCandidateDirs(projectDir, nested, 4)),
			}}
		}
		return []validationResult{{
			Kind:     "arduino",
			Target:   projectDir,
			Status:   "skipped",
			NextStep: "没有找到 .ino sketch；Arduino 项目通常需要 项目名/项目名.ino。",
		}}
	}
	// FQBN 以项目为准,优先级:显式 fqbn 覆盖 > 该 sketch 自己的 manifest.board > 调用方
	// 传入的 board(UI 选择)> uno 兜底。这样板型跟着项目 manifest 走(如 esp32s3),不再被
	// UI 下拉或默认 uno 带偏。逐 sketch 解析,兼容一个父目录下多个子项目各有 manifest。
	overrideFQBN := strArg(args, "fqbn", "")
	argBoard := strArg(args, "board", "")
	results := make([]validationResult, 0, len(sketchDirs))
	for _, sketch := range sketchDirs {
		fqbn := overrideFQBN
		if fqbn == "" {
			board := argBoard
			if m, _ := auditManifestFile(sketch); m.Board != "" {
				board = m.Board
			}
			if board == "" {
				board = "uno"
			}
			fqbn = arduinoFQBN(board)
		}
		cmdArgs := map[string]any{"sketch_dir": sketch, "fqbn": fqbn, "timeout_seconds": int(timeout / time.Second)}
		out, err := runArduinoCompile(cmdArgs)
		results = append(results, resultFromCommand("arduino", sketch, out, err, "先修复编译错误；编译通过后再连接开发板执行 arduino_upload 和 arduino_monitor_sample。"))
	}
	return results
}

func validatePlatformIOProject(projectDir string, args map[string]any, timeout time.Duration) validationResult {
	if result := validatePlatformIOLayout(projectDir); result != nil {
		return *result
	}
	cmdArgs := map[string]any{"project_dir": projectDir, "timeout_seconds": int(timeout / time.Second)}
	if env := strArg(args, "environment", ""); env != "" {
		cmdArgs["environment"] = env
	}
	out, err := runPlatformIO(cmdArgs)
	result := resultFromCommand("platformio", projectDir, out, err, "先修复 PlatformIO build 错误；编译通过后再用 targets=[upload, monitor] 做烧录和串口验证。")
	if err != nil && strings.Contains(out, "Nothing to build") {
		result.NextStep = "PlatformIO 没有编译到源码；确认主程序在 src/main.cpp 或 src/*.cpp 中，不要只把 .ino 放在项目根目录。"
	}
	return result
}

func validatePlatformIOLayout(projectDir string) *validationResult {
	if !exists(filepath.Join(projectDir, "platformio.ini")) {
		return &validationResult{
			Kind:     "platformio_layout",
			Target:   projectDir,
			Status:   "failed",
			NextStep: "缺少 platformio.ini；如果要用 PlatformIO，先创建 platformio.ini；如果是 Arduino IDE 项目，改用 platform=arduino 验证。",
		}
	}
	srcFiles := findPlatformIOSourceFiles(projectDir)
	if len(srcFiles) > 0 {
		return nil
	}
	rootSketches := findRootINOSketches(projectDir)
	next := "将主程序放到 src/main.cpp（保留 setup/loop，补充 #include <Arduino.h>），或删除 platformio.ini 改用 Arduino CLI；完成后重新运行 hardware_project_validate。"
	output := "PlatformIO layout check failed: no source file under src/."
	if len(rootSketches) > 0 {
		output += "\nRoot .ino files found but ignored by this PlatformIO project:\n- " + strings.Join(rootSketches, "\n- ")
	}
	return &validationResult{
		Kind:     "platformio_layout",
		Target:   projectDir,
		Status:   "failed",
		Output:   output,
		NextStep: next,
	}
}

func validateESPIDFProject(projectDir string, args map[string]any, timeout time.Duration) []validationResult {
	if !hasESPIDF() {
		return []validationResult{{
			Kind:     "esp_idf",
			Target:   projectDir,
			Status:   "skipped",
			NextStep: "本机未找到可用 ESP-IDF；请安装/激活 ESP-IDF，或设置 REASONIX_ESP_IDF_PATH 指向 esp-idf 目录。",
		}}
	}
	results := []validationResult{}
	if target := strArg(args, "target", ""); target != "" {
		out, err := runESPIDF(map[string]any{"project_dir": projectDir, "action": "set_target", "target": target, "timeout_seconds": int(timeout / time.Second)})
		results = append(results, resultFromCommand("esp_idf_set_target", projectDir, out, err, "先修复 set-target 错误，再执行 ESP-IDF build。"))
		if err != nil {
			return results
		}
	}
	out, err := runESPIDF(map[string]any{"project_dir": projectDir, "action": "build", "timeout_seconds": int(timeout / time.Second)})
	results = append(results, resultFromCommand("esp_idf", projectDir, out, err, "先修复 ESP-IDF build 错误；编译通过后再连接 ESP32 执行 flash_monitor。"))
	return results
}

func validatePythonProject(projectDir string, timeout time.Duration) validationResult {
	files := findPythonFiles(projectDir)
	if len(files) == 0 {
		return validationResult{
			Kind:     "python_syntax",
			Target:   projectDir,
			Status:   "skipped",
			NextStep: "没有找到 Python 源文件；请确认 src/main.py 是否存在。",
		}
	}
	python := pythonCommand()
	if python == "" {
		return validationResult{
			Kind:     "python_syntax",
			Target:   projectDir,
			Status:   "skipped",
			NextStep: "本机未找到 python3 或 python，无法做语法检查。",
		}
	}
	cmdArgs := append([]string{"-m", "py_compile"}, files...)
	out, err := runCommandText(python, cmdArgs, projectDir, timeout)
	res := resultFromCommand("python_syntax", projectDir, out, err, "先修复 Python 语法错误；语法通过后再按设备选择 mpremote 或 ssh_deploy_run。")
	if res.Status == "passed" {
		// py_compile 只查语法，不校验 import 是否存在、API 是否正确、真机能否运行。
		// 国产生态(maix/pinpong/unihiker)的库本机通常没装，幻觉出的方法语法合法照样过——
		// 绝不能让学生把「语法通过」误当「已验证」。把下一步明确指向真机验证。
		res.NextStep = "注意：这只是 Python 语法检查（py_compile），没有验证 import 是否存在、API 是否正确、真机能否运行。maix / pinpong / unihiker 等库本机通常未安装，语法过 ≠ 能跑。必须用 ssh_deploy_run（行空板/MaixCAM/树莓派）或 mpremote（MicroPython）在真机上跑通并采集输出，才算验证。"
	}
	return res
}

func resultFromCommand(kind, target, output string, err error, failedNextStep string) validationResult {
	result := validationResult{
		Kind:    kind,
		Target:  target,
		Status:  "passed",
		Command: firstCommandLine(output),
		Output:  output,
	}
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.NextStep = failedNextStep
		// 蒸馏:从级联报错抽根因 + 已知错误模式给直接修法,
		// 弱模型不必读整篇日志,看 rootCause + fixHint 就能下一步行动。
		result.RootCause, result.FixHint = distillBuildError(kind, output)
	}
	return result
}

func validationSummary(results []validationResult) string {
	passed, failed, skipped := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "passed":
			passed++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	return fmt.Sprintf("%d passed, %d failed, %d skipped", passed, failed, skipped)
}

func validationRecommendations(results []validationResult) []string {
	recs := []string{}
	for _, r := range results {
		if r.NextStep != "" {
			recs = append(recs, r.NextStep)
		}
	}
	return uniqueStrings(recs)
}

func dedupeValidationResults(results []validationResult) []validationResult {
	seen := map[string]bool{}
	out := make([]validationResult, 0, len(results))
	for _, r := range results {
		key := r.Kind + "\x00" + r.Target + "\x00" + r.Status
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func runArduinoCompile(args map[string]any) (string, error) {
	sketch, err := requirePath(args, "sketch_dir")
	if err != nil {
		return "", err
	}
	fqbn := strArg(args, "fqbn", "")
	if fqbn == "" {
		return "", errors.New("fqbn is required")
	}
	cmdArgs := []string{"compile", "-b", fqbn}
	if p := strArg(args, "port", ""); p != "" {
		cmdArgs = append(cmdArgs, "-p", p)
	}
	if boolArg(args, "export_binaries", false) {
		cmdArgs = append(cmdArgs, "--export-binaries")
	}
	if boolArg(args, "verbose", false) {
		cmdArgs = append(cmdArgs, "-v")
	}
	// 项目自带库:脚手架把依赖(如 LVGL)放进 <sketch>/libraries,arduino-cli 默认不扫这个
	// 子目录;自动带上 --libraries,让库随项目走——换台电脑、不全局装库也能编。
	if libDir := filepath.Join(sketch, "libraries"); exists(libDir) {
		cmdArgs = append(cmdArgs, "--libraries", libDir)
	}
	cmdArgs = append(cmdArgs, sketch)
	return runCommandText("arduino-cli", cmdArgs, "", timeoutArg(args, "timeout_seconds", defaultTimeout))
}

// arduinoCoreURLs 返回第三方 core 需要的 board manager URL(esp32/esp8266/rp2040);官方
// arduino:avr 不需要,返回空。
func arduinoCoreURLs(core string) string {
	switch {
	case strings.HasPrefix(core, "esp32"):
		return "https://espressif.github.io/arduino-esp32/package_esp32_index.json"
	case strings.HasPrefix(core, "esp8266"):
		return "https://arduino.esp8266.com/stable/package_esp8266com_index.json"
	case strings.HasPrefix(core, "rp2040"):
		return "https://github.com/earlephilhower/arduino-pico/releases/download/global/package_rp2040_index.json"
	default:
		return ""
	}
}

// runArduinoCoreInstall 安装一个 arduino-cli 开发板 core(先 update-index 再 install),
// 修复"工具已装但 core 未装"的首次编译墙。第三方 core 自动带上 board manager URL。
func runArduinoCoreInstall(args map[string]any) (string, error) {
	core := strings.TrimSpace(strArg(args, "core", ""))
	if core == "" {
		return "", errors.New("core 必填(如 arduino:avr / esp32:esp32 / esp8266:esp8266 / rp2040:rp2040)")
	}
	timeout := timeoutArg(args, "timeout_seconds", 600*time.Second)
	urls := arduinoCoreURLs(core)
	var b strings.Builder

	idxArgs := []string{"core", "update-index"}
	if urls != "" {
		idxArgs = append(idxArgs, "--additional-urls", urls)
	}
	idxOut, err := runCommandText("arduino-cli", idxArgs, "", timeout)
	b.WriteString(idxOut)
	if err != nil {
		return b.String(), fmt.Errorf("core update-index 失败: %w", err)
	}

	instArgs := []string{"core", "install", core}
	if urls != "" {
		instArgs = append(instArgs, "--additional-urls", urls)
	}
	instOut, err := runCommandText("arduino-cli", instArgs, "", timeout)
	b.WriteString("\n")
	b.WriteString(instOut)
	if err != nil {
		return b.String(), fmt.Errorf("core install %s 失败: %w", core, err)
	}
	b.WriteString("\n\n✅ core 安装完成,现在重新调用 arduino_compile(FQBN 不变)即可。")
	return b.String(), nil
}

func runArduinoUpload(args map[string]any) (string, error) {
	sketch, err := requirePath(args, "sketch_dir")
	if err != nil {
		return "", err
	}
	fqbn := strArg(args, "fqbn", "")
	port := strArg(args, "port", "")
	if fqbn == "" || port == "" {
		return "", errors.New("fqbn and port are required")
	}
	cmdArgs := []string{"upload", sketch, "-p", port, "-b", fqbn}
	if boolArg(args, "verify", false) {
		cmdArgs = append(cmdArgs, "--verify")
	}
	if buildPath := strArg(args, "build_path", ""); buildPath != "" {
		cmdArgs = append(cmdArgs, "--build-path", buildPath)
	}
	return runCommandText("arduino-cli", cmdArgs, "", timeoutArg(args, "timeout_seconds", defaultTimeout))
}

// runArduinoOTAUpload 走 arduino-cli 网络口烧录(底层 espota),把固件通过 WiFi 推给
// 已经跑着 ArduinoOTA 的板子。address 填板子 IP 或 mDNS 名,不需要 USB。
func runArduinoOTAUpload(args map[string]any) (string, error) {
	sketch, err := requirePath(args, "sketch_dir")
	if err != nil {
		return "", err
	}
	fqbn := strArg(args, "fqbn", "")
	address := strArg(args, "address", "")
	if fqbn == "" || address == "" {
		return "", errors.New("fqbn and address are required")
	}
	cmdArgs := []string{"upload", sketch, "-p", address, "-b", fqbn}
	if pwd := strArg(args, "password", ""); pwd != "" {
		cmdArgs = append(cmdArgs, "--upload-field", "password="+pwd)
	}
	if buildPath := strArg(args, "build_path", ""); buildPath != "" {
		cmdArgs = append(cmdArgs, "--build-path", buildPath)
	}
	return runCommandText("arduino-cli", cmdArgs, "", timeoutArg(args, "timeout_seconds", 180*time.Second))
}

// runFirmwarePublish 把固件发布到远端固件服务器(NAS/VPS 上的 nginx),供云端拉取 OTA:
// scp 固件到 <ssh_host>:<remote_dir>/<project>/firmware.bin,再写 version.txt。
// 没给 bin_path 就先用 sketch_dir+fqbn 现编。板子(刷了 agent)下次轮询会自动拉取升级。
func runFirmwarePublish(args map[string]any) (string, error) {
	project := strArg(args, "project_name", "")
	version := strArg(args, "version", "")
	sshHost := strArg(args, "ssh_host", "")
	remoteDir := strArg(args, "remote_dir", "")
	baseURL := strArg(args, "base_url", "")
	if project == "" || version == "" || sshHost == "" || remoteDir == "" {
		return "", errors.New("project_name, version, ssh_host, remote_dir are required")
	}
	binPath := strArg(args, "bin_path", "")
	if binPath == "" {
		sketch := strArg(args, "sketch_dir", "")
		fqbn := strArg(args, "fqbn", "")
		if sketch == "" || fqbn == "" {
			return "", errors.New("provide bin_path, or sketch_dir+fqbn to compile")
		}
		buildDir, err := os.MkdirTemp("", "fwpub-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(buildDir)
		cmdArgs := []string{"compile", "--fqbn", fqbn, "--output-dir", buildDir, sketch}
		// 与 runArduinoCompile 一致:项目自带库(<sketch>/libraries)必须显式带上,否则
		// 自带依赖(如 LVGL)的项目「编译」过、而「发布固件」的内联编译会找不到库而失败。
		if libDir := filepath.Join(sketch, "libraries"); exists(libDir) {
			cmdArgs = append(cmdArgs, "--libraries", libDir)
		}
		if _, cerr := runCommandText("arduino-cli", cmdArgs, "", 240*time.Second); cerr != nil {
			return "", fmt.Errorf("compile failed: %w", cerr)
		}
		binPath, err = findSketchBin(buildDir)
		if err != nil {
			return "", err
		}
	}
	remoteProjectDir := strings.TrimRight(remoteDir, "/") + "/" + project
	// 远端建项目目录
	if _, err := runCommandText("ssh", []string{sshHost, "mkdir -p " + shellArg(remoteProjectDir)}, "", 30*time.Second); err != nil {
		return "", fmt.Errorf("ssh mkdir failed: %w", err)
	}
	// 原子发布:先传到临时名,再 mv 覆盖,避免板子正好拉到"写了一半"的固件。
	// 顺序也讲究:先让 firmware.bin 完整就位,最后才更新 version.txt——板子是先读版本号
	// 再下固件,所以它看到新版本时固件一定已经完整在位。
	// 临时名带唯一后缀(纳秒时间戳):防两个客户端并发发布到同一项目目录时写坏同一个
	// firmware.bin.tmp——本进程互斥锁拦不住另一台机器,每个 scp 写各自的临时文件、mv 只
	// 转正各自那份完整文件。binTmp 与 verTmp 共用同一 nonce 便于配对。
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	binTmp := remoteProjectDir + "/firmware.bin.tmp." + nonce
	if _, err := runCommandText("scp", []string{binPath, sshHost + ":" + shellArg(binTmp)}, "", 150*time.Second); err != nil {
		return "", fmt.Errorf("scp firmware failed: %w", err)
	}
	mvBin := "mv -f " + shellArg(binTmp) + " " + shellArg(remoteProjectDir+"/firmware.bin")
	if _, err := runCommandText("ssh", []string{sshHost, mvBin}, "", 30*time.Second); err != nil {
		return "", fmt.Errorf("ssh activate firmware failed: %w", err)
	}
	// 最后原子更新版本号(板子靠它判断要不要更新):同样先写临时名再 mv
	verTmp := remoteProjectDir + "/version.txt.tmp." + nonce
	verCmd := "printf %s " + shellArg(version) + " > " + shellArg(verTmp) +
		" && mv -f " + shellArg(verTmp) + " " + shellArg(remoteProjectDir+"/version.txt")
	if _, err := runCommandText("ssh", []string{sshHost, verCmd}, "", 30*time.Second); err != nil {
		return "", fmt.Errorf("ssh write version failed: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/" + project
	return fmt.Sprintf("已发布 %s v%s\n  固件: %s/firmware.bin\n  版本: %s/version.txt = %s\n板子(已刷 agent)会在下次轮询时自动拉取升级。", project, version, url, url, version), nil
}

// findSketchBin 在编译输出目录里找主固件 .ino.bin(排除 bootloader/partitions/merged)。
func findSketchBin(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".ino.bin") &&
			!strings.Contains(n, ".bootloader.") &&
			!strings.Contains(n, ".partitions.") &&
			!strings.Contains(n, ".merged.") {
			return filepath.Join(dir, n), nil
		}
	}
	return "", errors.New("compiled firmware .bin not found in build dir")
}

func runArduinoMonitor(args map[string]any) (string, error) {
	port := strArg(args, "port", "")
	if port == "" {
		return "", errors.New("port is required")
	}
	baud := intArg(args, "baud", 115200)
	seconds := intArg(args, "seconds", defaultLogSeconds)
	// seconds<=0(模型显式传 0)会让 time.Duration 算出 0,落进 runCommandText 的 90s
	// 兜底超时;而 arduino-cli monitor 不自退 → 白卡满 90s。回退到默认采样秒数(F3)。
	if seconds <= 0 {
		seconds = defaultLogSeconds
	}
	cmdArgs := []string{"monitor", "-p", port, "--config", fmt.Sprintf("baudrate=%d", baud), "--timestamp"}
	if fqbn := strArg(args, "fqbn", ""); fqbn != "" {
		cmdArgs = append(cmdArgs, "-b", fqbn)
	}
	// monitor 必须撑住 stdin(keepStdinOpen=true):arduino-cli monitor 读到 /dev/null
	// 的 EOF 会约 1s 自退、采不到任何串口(实测 CH340 ESP32)。撑住后它会跑满采样秒数,
	// 再被采样超时正常杀掉——所以下面把“timed out 且有输出”当成成功路径。
	out, err := runCommandTextOpts("arduino-cli", cmdArgs, "", time.Duration(seconds)*time.Second, true)
	if err != nil {
		if strings.Contains(err.Error(), "timed out") {
			if commandOutputHasBody(out) {
				return out, nil
			}
			// 跑满窗口仍无输出:才是真的硬件侧问题,给排查指引而不是干瘪的 timeout。
			return out, errors.New(serialNoOutputGuidance)
		}
		return out, err
	}
	if !commandOutputHasBody(out) {
		return out, errors.New(serialNoOutputGuidance)
	}
	return out, nil
}

// serialNoOutputGuidance 是「采不到串口输出」时给模型的明确指引。实测(贪吃蛇会话)
// 模型采不到输出后会自己用 bash 的 screen/cu/cat/timeout 反复瞎试十几次(还撞上
// macOS 没有 timeout 命令),既慢又徒劳。这里直接劝阻、给唯一合理下一步。
const serialNoOutputGuidance = "串口没采到输出(采集已用 arduino-cli 内置超时正常结束,不是命令失败)。" +
	"没输出通常是硬件侧:板子没在运行、没接好、波特率不符,或固件缺 Serial.begin。" +
	"不要再用 bash 的 screen / cu / cat / timeout 反复尝试采集——这些在本机不可靠,且 macOS 默认没有 timeout 命令。" +
	"如果确认开发板已接好且程序在跑,检查波特率和接线后,最多再重试一次 arduino_monitor_sample;" +
	"否则如实报告「串口无输出,真机运行待确认」,不要伪造串口证据。"

func runPlatformIO(args map[string]any) (string, error) {
	dir, err := requirePath(args, "project_dir")
	if err != nil {
		return "", err
	}
	cmdArgs := []string{"run", "-d", dir}
	if env := strArg(args, "environment", ""); env != "" {
		cmdArgs = append(cmdArgs, "-e", env)
	}
	targets := strSliceArg(args, "targets")
	monitor := false
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t == "monitor" {
			monitor = true
		}
		cmdArgs = append(cmdArgs, "-t", t)
	}
	if p := strArg(args, "upload_port", ""); p != "" {
		cmdArgs = append(cmdArgs, "--upload-port", p)
	}
	if p := strArg(args, "monitor_port", ""); p != "" {
		cmdArgs = append(cmdArgs, "--monitor-port", p)
	}
	defaultTO := defaultTimeout
	if monitor {
		defaultTO = 12 * time.Second
	}
	out, err := runCommandText("pio", cmdArgs, dir, timeoutArg(args, "timeout_seconds", defaultTO))
	if err != nil && monitor && strings.Contains(err.Error(), "timed out") && commandOutputHasBody(out) {
		return out, nil
	}
	if monitor && err == nil && !commandOutputHasBody(out) {
		return out, errors.New("platformio monitor produced no runtime output; verify port, baud rate, board reset, and firmware Serial output")
	}
	return out, err
}

func runESPIDF(args map[string]any) (string, error) {
	dir, err := requirePath(args, "project_dir")
	if err != nil {
		return "", err
	}
	action := strArg(args, "action", "")
	cmdArgs := []string{"-C", dir}
	monitor := false
	switch action {
	case "set_target":
		target := strArg(args, "target", "")
		if target == "" {
			return "", errors.New("target is required for set_target")
		}
		cmdArgs = append(cmdArgs, "set-target", target)
	case "build":
		cmdArgs = append(cmdArgs, "build")
	case "flash":
		cmdArgs = appendIDFPort(cmdArgs, args)
		cmdArgs = append(cmdArgs, "flash")
	case "monitor":
		cmdArgs = appendIDFPort(cmdArgs, args)
		cmdArgs = appendIDFBaud(cmdArgs, args)
		cmdArgs = append(cmdArgs, "monitor")
		monitor = true
	case "flash_monitor":
		cmdArgs = appendIDFPort(cmdArgs, args)
		cmdArgs = appendIDFBaud(cmdArgs, args)
		cmdArgs = append(cmdArgs, "flash", "monitor")
		monitor = true
	case "clean":
		cmdArgs = append(cmdArgs, "clean")
	case "fullclean":
		cmdArgs = append(cmdArgs, "fullclean")
	default:
		return "", fmt.Errorf("unsupported ESP-IDF action %q", action)
	}
	defaultTO := defaultTimeout
	if monitor {
		defaultTO = 12 * time.Second
	}
	out, err := runESPIDFCommandText(cmdArgs, dir, timeoutArg(args, "timeout_seconds", defaultTO))
	if err != nil && monitor && strings.Contains(err.Error(), "timed out") && commandOutputHasBody(out) {
		return out, nil
	}
	if monitor && err == nil && !commandOutputHasBody(out) {
		return out, errors.New("ESP-IDF monitor produced no runtime output; verify port, baud rate, board reset, and ESP_LOG/printf output")
	}
	return out, err
}

func runESPIDFMCPConfig(args map[string]any) (string, error) {
	cwd, _ := os.Getwd()
	dir := strArg(args, "project_dir", cwd)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	name := strArg(args, "server_name", "esp-idf-tools")
	useEIM := boolArg(args, "use_eim", hasEIM() && !resolveLocalESPIDF().Available)
	hint := espIDFMCPHint(abs, name, useEIM)
	status := map[string]any{
		"projectDir": abs,
		"serverName": name,
		"available": map[string]bool{
			"eim":          hasEIM(),
			"idf.py":       hasCommand("idf.py"),
			"localEspIdf":  resolveLocalESPIDF().Available,
			"espIdfRunner": hasESPIDF(),
		},
		"snippets": hint,
		"note":     "Espressif ESP-IDF v6.0+ provides an official local stdio Tools MCP server. Use EIM when the AI client does not inherit the ESP-IDF shell environment.",
	}
	return prettyJSON(status), nil
}

func runMPRemote(args map[string]any) (string, error) {
	script, err := requirePath(args, "script")
	if err != nil {
		return "", err
	}
	cmdArgs := []string{}
	if device := strArg(args, "device", ""); device != "" {
		cmdArgs = append(cmdArgs, "connect", device)
	}
	cmdArgs = append(cmdArgs, "run", script)
	if boolArg(args, "no_follow", false) {
		cmdArgs = append(cmdArgs, "--no-follow")
	}
	out, err := runCommandText("mpremote", cmdArgs, filepath.Dir(script), timeoutArg(args, "timeout_seconds", 30*time.Second))
	requireOutput := boolArg(args, "require_output", !boolArg(args, "no_follow", false))
	if err == nil && requireOutput && !commandOutputHasBody(out) {
		return out, errors.New("mpremote produced no runtime output; add print() diagnostics, verify the script is running, or set require_output=false for background-only runs")
	}
	return out, err
}

func runSSHDeploy(args map[string]any) (string, error) {
	host := strArg(args, "host", "")
	localPath := strArg(args, "local_path", "")
	remotePath := strArg(args, "remote_path", "")
	if host == "" || localPath == "" || remotePath == "" {
		return "", errors.New("host, local_path, and remote_path are required")
	}
	user := strArg(args, "user", "root")
	localAbs, err := filepath.Abs(localPath)
	if err != nil {
		return "", err
	}
	to := user + "@" + host + ":" + remotePath
	sshPort := intArg(args, "ssh_port", 22)
	identityFile := strArg(args, "identity_file", "")
	connectTimeout := intArg(args, "connect_timeout", 8)
	// 密码认证经 sshpass 完成(行空板默认 root/dfrobot 这类设备没有预配密钥);
	// 传了 password 就不能开 BatchMode,否则 ssh 直接拒绝密码交互。
	password := strArg(args, "password", "")
	if password != "" {
		if _, lookErr := exec.LookPath("sshpass"); lookErr != nil {
			return "", errors.New("提供了 password 但本机没有 sshpass(密码登录需要它)。安装:`brew install esolitos/ipa/sshpass`;或先 `ssh-copy-id " + user + "@" + host + "` 配免密后去掉 password 重试。")
		}
	}
	run := func(name string, cmdArgs []string, timeout time.Duration) (string, error) {
		if password != "" {
			cmdArgs = append([]string{"-p", password, name}, cmdArgs...)
			name = "sshpass"
		}
		return runCommandText(name, cmdArgs, "", timeout)
	}
	commonArgs := sshCommonArgs(connectTimeout, identityFile, password == "")
	scpArgs := append([]string{}, commonArgs...)
	if sshPort > 0 {
		scpArgs = append(scpArgs, "-P", strconv.Itoa(sshPort))
	}
	if boolArg(args, "recursive", false) {
		scpArgs = append(scpArgs, "-r")
	}
	scpArgs = append(scpArgs, localAbs, to)
	timeout := timeoutArg(args, "timeout_seconds", defaultTimeout)
	out, err := run("scp", scpArgs, timeout)
	if err != nil {
		return out, sshAuthGuidance(out, err)
	}
	remoteCmd := strArg(args, "command", "")
	if remoteCmd == "" {
		return out, nil
	}
	sshArgs := append([]string{}, commonArgs...)
	if sshPort > 0 {
		sshArgs = append(sshArgs, "-p", strconv.Itoa(sshPort))
	}
	sshArgs = append(sshArgs, user+"@"+host, remoteCmd)
	sshOut, err := run("ssh", sshArgs, timeout)
	if err != nil {
		err = sshAuthGuidance(sshOut, err)
	}
	combined := out + "\n--- remote command ---\n" + sshOut
	if err == nil && boolArg(args, "require_output", true) && !commandOutputHasBody(sshOut) {
		return combined, errors.New("ssh remote command produced no runtime output; add print/log output, verify the process is running, or set require_output=false for deploy-only commands")
	}
	return combined, err
}

// sshCommonArgs 组装 ssh/scp 公共参数。batch=false 表示走密码认证(sshpass),
// 此时不能加 BatchMode=yes(它会直接禁掉密码交互)。
// known_hosts 固定写到系统临时目录:行空板实测中,写 ~/.ssh/known_hosts 会被
// 桌面端 bash 沙箱拒绝(mkstemp: Operation not permitted),每次连接都污染输出
// 甚至造成挂起;教学场景设备都在内网,临时 known_hosts 足够。
func sshCommonArgs(connectTimeout int, identityFile string, batch bool) []string {
	if connectTimeout <= 0 {
		connectTimeout = 8
	}
	args := []string{}
	if batch {
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+filepath.Join(os.TempDir(), "onecreat_ssh_known_hosts"),
		"-o", "ConnectTimeout="+strconv.Itoa(connectTimeout),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=1",
	)
	if identityFile != "" {
		args = append(args, "-i", identityFile)
	}
	return args
}

// sshAuthGuidance 在认证失败时给模型明确的下一步,防止它用 bash 反复裸试 ssh
// (行空板实测:模型撞 Permission denied 后 bash 试了十几次,纯靠运气发现 sshpass)。
func sshAuthGuidance(out string, err error) error {
	if err == nil {
		return nil
	}
	text := out + " " + err.Error()
	if !strings.Contains(text, "Permission denied") && !strings.Contains(text, "permission denied") {
		return err
	}
	return fmt.Errorf("%w\nSSH 认证失败——这类设备多为密码登录:重新调用 ssh_deploy_run 并传 password 参数"+
		"(行空板默认 root/dfrobot);或先 ssh-copy-id 配好免密再重试。"+
		"不要用 bash 反复裸试 ssh,交互式密码提示在这里无法工作。", err)
}

// --- prompts/resources ---

func promptList() []map[string]any {
	return []map[string]any{
		{
			"name":        "hardware_plan",
			"description": "Plan a student-friendly AI hardware project with board roles, data flow, and verification steps.",
			"arguments": []map[string]any{
				{"name": "project", "description": "Project idea or problem", "required": true},
			},
		},
		{
			"name":        "hardware_debug",
			"description": "Analyze build, flash, serial, or SSH logs for a hardware project.",
			"arguments": []map[string]any{
				{"name": "platform", "description": "Arduino, ESP32, ESP-IDF, MaixCAM, Unihiker, or RPi", "required": true},
			},
		},
		{
			"name":        "hardware_review",
			"description": "Review firmware for teaching clarity, hardware safety, and explainability.",
			"arguments": []map[string]any{
				{"name": "path", "description": "File or directory to review", "required": true},
			},
		},
	}
}

func getPrompt(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	var text string
	switch p.Name {
	case "hardware_plan":
		project := p.Arguments["project"]
		if project == "" {
			project = "the current student project"
		}
		text = "为以下学生 AI 硬件项目做技术规划：" + project + "\n\n要求：先确认平台角色，再画数据流方向；优先使用 UART/I2C/SPI/MQTT/HTTP 中最简单可靠的通信方式；代码和方案必须适合 1-9 年级学生答辩解释；列出最小可验证原型和逐步验证命令。"
	case "hardware_debug":
		platform := p.Arguments["platform"]
		if platform == "" {
			platform = "current hardware platform"
		}
		text = "请调试 " + platform + " 硬件项目。先调用 hardware_detect，再调用 hardware_project_audit 检查 manifest、接线说明和验证流程，然后调用 hardware_project_validate 做自动编译或语法检查；每完成一段验证都调用 hardware_evidence_record 写入证据，再调用 hardware_evidence_status 汇总本机/实机状态；读取相关代码和最近日志；区分项目上下文缺失、编译错误、烧录错误、串口无输出、端口占用、库缺失、供电/接线问题；给出最小修复和下一条验证命令。"
	case "hardware_review":
		path := p.Arguments["path"]
		if path == "" {
			path = "."
		}
		text = "请审查硬件项目代码 " + path + "。重点检查：中文注释是否能让学生逐行解释；魔数是否提取为常量；是否有阻塞式 delay、未处理错误、危险引脚、串口波特率不一致、ESP32 ADC2+WiFi 冲突、ESP-IDF 事件处理遗漏。请按严重程度列出问题。"
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown prompt: " + p.Name}
	}
	return map[string]any{
		"description": p.Name,
		"messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": text}},
		},
	}, nil
}

var resourceContents = map[string]string{
	"doc://hardware-platform-guide": strings.TrimSpace(`
# AI 硬件编程平台指南

优先级：
1. Arduino Nano/UNO：执行层控制器，适合电机、传感器、简单逻辑。
2. ESP32：通信 + 轻量边缘推理，适合 WiFi/BLE、MQTT/HTTP、Q-Learning。
3. Unihiker：主控 + UI + 协调，适合 Python、屏幕交互、API 调用。
4. MaixCAM：视觉处理，适合 MaixPy 物体识别、颜色追踪、人脸/手势检测。
5. Raspberry Pi：复杂计算主控，适合 OpenCV、MediaPipe、YOLOv8、多模型推理。

常见数据流：
MaixCAM/摄像头 --UART/I2C--> ESP32/Arduino --执行动作
Unihiker --UI/API/协调--> ESP32/Arduino/MaixCAM

教学要求：
- 每个函数开头说明这个函数做什么。
- 关键逻辑逐行中文注释。
- 魔数提取为命名常量。
- 先做最小可验证原型，再叠加 AI/联网/展示功能。

验证顺序：
hardware_detect -> hardware_project_audit -> hardware_project_validate -> hardware_evidence_record -> hardware_evidence_status -> upload/flash -> monitor/log -> hardware_evidence_record -> hardware_evidence_status -> 根据真实输出修复。
`),
	"doc://esp-idf-mcp-guide": strings.TrimSpace(`
# ESP-IDF 官方 MCP 接入

ESP-IDF v6.0+ 提供官方本地 stdio Tools MCP server，可让 AI 客户端执行 set-target、build、flash、status 等 ESP-IDF 项目操作。

推荐方式：
[[plugins]]
name = "esp-idf-tools"
command = "eim"
args = ["run", "idf.py mcp-server"]
env = { IDF_MCP_WORKSPACE_FOLDER = "/path/to/project" }

已激活 ESP-IDF 环境时也可以：
[[plugins]]
name = "esp-idf-tools"
command = "idf.py"
args = ["-C", "/path/to/project", "mcp-server"]

Reasonix 也支持项目根目录 .mcp.json。官方 MCP 适合 ESP-IDF v6.0+ 项目；本插件的 esp_idf_run 是直接 idf.py fallback。
`),
}

func resourceList() []map[string]any {
	return []map[string]any{
		{"uri": "doc://hardware-platform-guide", "name": "AI hardware platform guide", "description": "AI hardware platform roles and teaching rules.", "mimeType": "text/markdown"},
		{"uri": "doc://esp-idf-mcp-guide", "name": "ESP-IDF official MCP guide", "description": "Config snippets for Espressif ESP-IDF Tools MCP server.", "mimeType": "text/markdown"},
	}
}

func readResource(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	text, ok := resourceContents[p.URI]
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource: " + p.URI}
	}
	return map[string]any{
		"contents": []map[string]any{{"uri": p.URI, "mimeType": "text/markdown", "text": text}},
	}, nil
}

// --- command helpers ---

func runCommandText(name string, args []string, dir string, timeout time.Duration) (string, error) {
	return runCommandTextOpts(name, args, dir, timeout, false)
}

// ctxEOFReader 是个永不返回数据、直到 ctx 结束才 EOF 的 stdin。给交互式 monitor 撑住
// stdin,避免它读到 /dev/null 的即时 EOF 而秒退;ctx 结束(超时杀进程)后返回 EOF,
// 让 exec 的 stdin 拷贝 goroutine 能收尾、cmd.Run 正常返回,不泄漏 goroutine。
type ctxEOFReader struct{ ctx context.Context }

func (r ctxEOFReader) Read(p []byte) (int, error) {
	<-r.ctx.Done()
	return 0, io.EOF
}

// runCommandTextOpts 跑外部命令并捕获输出。keepStdinOpen=true 时给子进程一个永不 EOF
// 的 stdin,专给 arduino-cli monitor 这类“交互式、读到 stdin EOF 就自退”的命令用:
// 否则 Go 的 exec 默认把 stdin 接 /dev/null,monitor 读到 EOF 约 1s 就退,在 CH340
// ESP32 这类板上一个字节都采不到(2026-06 实测)。撑住 stdin 后它会一直跑到超时被杀,
// 我们才拿到这段采样输出。普通命令(编译/上传)保持 keepStdinOpen=false,自然退出。
func runCommandTextOpts(name string, args []string, dir string, timeout time.Duration, keepStdinOpen bool) (string, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		// 带上安装指引:弱模型可能跳过 hardware_detect 直接调动作工具,撞到 not-found 时
		// 也要能自愈,而不是只有一句干瘪报错(#3)。
		if hint := toolInstallHint(name); hint != "" {
			return "", fmt.Errorf("%s 不在 PATH 上(未安装或未配置)。%s", name, hint)
		}
		return "", fmt.Errorf("%s not found on PATH", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	setKillGroup(cmd) // 超时杀整个进程组,monitor 不留占串口的孤儿子进程(F2)
	if keepStdinOpen {
		cmd.Stdin = ctxEOFReader{ctx: ctx} // 撑住 stdin,monitor 才不会读 /dev/null EOF 秒退
	}
	if dir != "" {
		cmd.Dir = dir
	}
	var buf limitedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	out := strings.TrimRight(buf.String(), "\n")
	header := "$ " + shellJoin(append([]string{name}, args...))
	if dir != "" {
		header = "(cwd: " + dir + ")\n" + header
	}
	if ctx.Err() == context.DeadlineExceeded {
		return header + "\n" + out, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		return header + "\n" + out, fmt.Errorf("%s failed: %w", name, err)
	}
	return header + "\n" + out, nil
}

func runESPIDFCommandText(args []string, dir string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if path, err := exec.LookPath("idf.py"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		setKillGroup(cmd) // idf.py monitor fork 出 esp_idf_monitor,超时要杀整组(F2)
		if dir != "" {
			cmd.Dir = dir
		}
		var buf limitedBuffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err = cmd.Run()
		out := strings.TrimRight(buf.String(), "\n")
		header := "$ " + shellJoin(append([]string{"idf.py"}, args...))
		if dir != "" {
			header = "(cwd: " + dir + ")\n" + header
		}
		if ctx.Err() == context.DeadlineExceeded {
			return header + "\n" + out, fmt.Errorf("idf.py timed out after %s", timeout)
		}
		if err != nil {
			return header + "\n" + out, fmt.Errorf("idf.py failed: %w", err)
		}
		return header + "\n" + out, nil
	}
	local := resolveLocalESPIDF()
	if !local.Available {
		return "", errors.New("ESP-IDF not found; set REASONIX_ESP_IDF_PATH or activate idf.py on PATH")
	}
	return runLocalESPIDFCommand(local, args, dir, timeout)
}

func runLocalESPIDFCommand(local espIDFEnv, args []string, dir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	script := local.ESPIDFShellScript(args, dir)
	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	setKillGroup(cmd) // bash -lc … exec idf.py monitor 同样 fork 孙进程,超时杀整组(F2)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf limitedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimRight(buf.String(), "\n")
	header := "$ bash -lc " + strconv.Quote(script)
	if dir != "" {
		header = "(cwd: " + dir + ")\n" + header
	}
	if ctx.Err() == context.DeadlineExceeded {
		return header + "\n" + out, fmt.Errorf("esp-idf timed out after %s", timeout)
	}
	if err != nil {
		return header + "\n" + out, fmt.Errorf("esp-idf failed: %w", err)
	}
	return header + "\n" + out, nil
}

type limitedBuffer struct {
	b bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.b.Len() < maxCommandOutput {
		remaining := maxCommandOutput - l.b.Len()
		if len(p) > remaining {
			l.b.Write(p[:remaining])
			l.b.WriteString("\n...[output truncated]...\n")
			return len(p), nil
		}
		l.b.Write(p)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.b.String() }

// shellJoin 仅用于「展示」(report 里的建议命令、输出 header):用 strconv.Quote 生成
// 可读的双引号形式。绝不能用它拼真正喂给 `bash -lc` 的命令行——双引号内 bash 仍会展开
// $()/反引号,会命令注入。真执行的拼接用 shellJoinArgs(单引号转义)(F1)。
func shellJoin(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\n\"'\\$`") {
			quoted[i] = strconv.Quote(p)
		} else {
			quoted[i] = p
		}
	}
	return strings.Join(quoted, " ")
}

func appendIDFPort(args []string, in map[string]any) []string {
	if p := strArg(in, "port", ""); p != "" {
		args = append(args, "-p", p)
	}
	return args
}

func appendIDFBaud(args []string, in map[string]any) []string {
	baud := intArg(in, "baud", 115200)
	if baud > 0 {
		args = append(args, "-b", strconv.Itoa(baud))
	}
	return args
}

// --- detection helpers ---

type espIDFEnv struct {
	Available     bool
	IDFPath       string
	Python        string
	PythonEnv     string
	DYLDPath      string
	Source        string
	Command       string
	ActivationCmd string
}

func hasESPIDF() bool {
	if hasCommand("idf.py") {
		return true
	}
	return resolveLocalESPIDF().Available
}

func hasEIM() bool {
	if hasCommand("eim") {
		return true
	}
	return resolveEIMPath() != ""
}

func detectESPIDFToolchain() toolchainReport {
	report := toolchainReport{
		Name:    "ESP-IDF idf.py",
		Command: "idf.py --version",
		Hint:    "Install ESP-IDF, activate the environment, or set REASONIX_ESP_IDF_PATH for GUI launches.",
	}
	if path, err := exec.LookPath("idf.py"); err == nil {
		report.Available = true
		report.Path = path
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "--version")
		out, err := cmd.CombinedOutput()
		if err == nil || len(out) > 0 {
			report.Version = firstLine(strings.TrimSpace(string(out)))
		}
		return report
	}
	local := resolveLocalESPIDF()
	if !local.Available {
		return report
	}
	report.Available = true
	report.Path = local.IDFPath
	report.Command = local.Command + " --version"
	report.Hint = "Using local ESP-IDF environment via " + local.Source + "."
	out, err := runLocalESPIDFCommand(local, []string{"--version"}, local.IDFPath, 8*time.Second)
	if err == nil || out != "" {
		report.Version = firstLineWithPrefix(out, "ESP-IDF ")
		if report.Version == "" {
			report.Version = firstLineContaining(out, "ESP-IDF")
		}
	}
	return report
}

func detectEIMToolchain() toolchainReport {
	report := toolchainReport{
		Name:    "Espressif EIM",
		Command: "eim --version",
		Hint:    "Install EIM 0.8.1+ for ESP-IDF v6.0 Tools MCP support, or set REASONIX_EIM.",
	}
	path := resolveEIMPath()
	if path == "" {
		return report
	}
	report.Available = true
	report.Path = path
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err == nil || len(out) > 0 {
		report.Version = firstLine(strings.TrimSpace(string(out)))
	}
	return report
}

// toolInstallHint 给常见硬件工具链一句按 OS 区分的安装指引。过去安装提示偏 macOS/brew,
// Windows 老师照着会报错;这里按当前 OS 给对应命令(#3)。
func toolInstallHint(name string) string {
	mac, win, linux := "", "", ""
	switch name {
	case "arduino-cli":
		mac, win, linux = "brew install arduino-cli", "winget install ArduinoSA.CLI(或 choco install arduino-cli)", "见 arduino.github.io/arduino-cli 安装脚本"
	case "pio", "platformio":
		mac, win, linux = "brew install platformio(或 pip install platformio)", "pip install platformio(需先装 Python)", "pip install platformio"
	case "idf.py":
		mac, win, linux = "按 ESP-IDF 官方 get-started 安装并 source export.sh", "用 ESP-IDF Windows Installer(含 idf.py)", "ESP-IDF install.sh 后 source export.sh"
	case "mpremote":
		mac, win, linux = "pip install mpremote", "pip install mpremote", "pip install mpremote"
	case "sshpass":
		mac, win, linux = "brew install esolitos/ipa/sshpass", "Windows 无 sshpass:改用 ssh 密钥免密,或在 WSL 里运行", "apt install sshpass"
	default:
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		return "安装方式(Windows):" + win
	case "linux":
		return "安装方式(Linux):" + linux
	default:
		return "安装方式(macOS):" + mac
	}
}

func detectToolchain(label string, versionCmd []string, hint string) toolchainReport {
	report := toolchainReport{Name: label, Command: strings.Join(versionCmd, " "), Hint: hint}
	if len(versionCmd) == 0 {
		return report
	}
	path, err := exec.LookPath(versionCmd[0])
	if err != nil {
		return report
	}
	report.Available = true
	report.Path = path
	if len(versionCmd) == 1 {
		return report
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, versionCmd[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil || len(out) > 0 {
		report.Version = firstLine(strings.TrimSpace(string(out)))
	}
	return report
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func resolveEIMPath() string {
	if path := strings.TrimSpace(os.Getenv("REASONIX_EIM")); path != "" && isExecutableFile(path) {
		return path
	}
	if path, err := exec.LookPath("eim"); err == nil {
		return path
	}
	for _, path := range localToolCandidates("tools/eim/eim") {
		if isExecutableFile(path) {
			return path
		}
	}
	return ""
}

func resolveLocalESPIDF() espIDFEnv {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("REASONIX_DISABLE_LOCAL_ESP_IDF")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("REASONIX_DISABLE_LOCAL_ESP_IDF")), "true") {
		return espIDFEnv{}
	}
	idfPath := strings.TrimSpace(os.Getenv("REASONIX_ESP_IDF_PATH"))
	source := "REASONIX_ESP_IDF_PATH"
	if idfPath == "" || !isDir(filepath.Join(idfPath, "tools")) {
		source = "local search"
		idfPath = findLocalESPIDFPath()
	}
	if idfPath == "" || !exists(filepath.Join(idfPath, "export.sh")) || !exists(filepath.Join(idfPath, "tools", "idf.py")) {
		return espIDFEnv{}
	}
	pythonEnv := strings.TrimSpace(os.Getenv("REASONIX_IDF_PYTHON_ENV_PATH"))
	if pythonEnv == "" {
		pythonEnv = findLocalIDFPythonEnv(idfPath)
	}
	python := strings.TrimSpace(os.Getenv("REASONIX_IDF_PYTHON"))
	if python == "" && pythonEnv != "" {
		python = filepath.Join(pythonEnv, "bin", "python")
	}
	if python == "" || !isExecutableFile(python) {
		return espIDFEnv{}
	}
	dyld := strings.TrimSpace(os.Getenv("REASONIX_DYLD_LIBRARY_PATH"))
	if dyld == "" && isDir("/opt/homebrew/opt/expat/lib") {
		dyld = "/opt/homebrew/opt/expat/lib"
	}
	idfPath = filepath.Clean(idfPath)
	return espIDFEnv{
		Available: true,
		IDFPath:   idfPath,
		Python:    python,
		PythonEnv: pythonEnv,
		DYLDPath:  dyld,
		Source:    source,
		Command:   python + " " + filepath.Join(idfPath, "tools", "idf.py"),
	}
}

func findLocalESPIDFPath() string {
	candidates := []string{}
	candidates = append(candidates, localToolCandidates("tools/esp/*/esp-idf")...)
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "esp", "esp-idf"),
			filepath.Join(home, "esp-idf"),
		)
	}
	for _, pattern := range candidates {
		matches := []string{pattern}
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ = filepath.Glob(pattern)
			sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		}
		for _, candidate := range matches {
			if exists(filepath.Join(candidate, "export.sh")) && exists(filepath.Join(candidate, "tools", "idf.py")) {
				return filepath.Clean(candidate)
			}
		}
	}
	return ""
}

func findLocalIDFPythonEnv(idfPath string) string {
	if home, err := os.UserHomeDir(); err == nil {
		version := filepath.Base(filepath.Dir(idfPath))
		for _, candidate := range []string{
			filepath.Join(home, ".espressif", "tools", "python", version, "venv"),
			filepath.Join(home, ".espressif", "python_env", "idf"+strings.TrimPrefix(version, "v")+"_py3.13_env"),
			filepath.Join(home, ".espressif", "python_env", "idf"+strings.TrimPrefix(version, "v")+"_py3.12_env"),
			filepath.Join(home, ".espressif", "python_env", "idf"+strings.TrimPrefix(version, "v")+"_py3.11_env"),
			filepath.Join(home, ".espressif", "python_env", "idf"+strings.TrimPrefix(version, "v")+"_py3.10_env"),
		} {
			if isExecutableFile(filepath.Join(candidate, "bin", "python")) {
				return filepath.Clean(candidate)
			}
		}
	}
	return ""
}

func localToolCandidates(relPattern string) []string {
	cwd, err := os.Getwd()
	if err != nil {
		return []string{}
	}
	candidates := []string{}
	dir := filepath.Clean(cwd)
	for {
		candidates = append(candidates, filepath.Join(dir, relPattern))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return candidates
}

func (env espIDFEnv) ESPIDFShellScript(args []string, dir string) string {
	commands := []string{}
	if env.PythonEnv != "" {
		commands = append(commands, "export IDF_PYTHON_ENV_PATH="+shellQuote(env.PythonEnv))
	}
	if bootstrap := pythonBootstrapShellCommand(); bootstrap != "" {
		commands = append(commands, bootstrap)
	}
	if env.DYLDPath != "" {
		commands = append(commands, "export DYLD_LIBRARY_PATH="+shellQuote(env.DYLDPath+":${DYLD_LIBRARY_PATH:-}"))
	}
	commands = append(commands, "source "+shellQuote(filepath.Join(env.IDFPath, "export.sh"))+" >/dev/null")
	if dir != "" {
		commands = append(commands, "cd "+shellQuote(dir))
	}
	cmdParts := append([]string{env.Python, filepath.Join(env.IDFPath, "tools", "idf.py")}, args...)
	// 这一行是真正喂给 `bash -lc` 执行的:必须用 POSIX 单引号转义(shellArg),不能用
	// shellJoin 的 strconv.Quote——双引号内 bash 仍会展开 $()/反引号/$VAR,target/port
	// 来自模型/工具输入(无 enum 约束)时 `esp32$(touch /tmp/PWNED)` 会变任意命令执行(F1)。
	commands = append(commands, "exec "+shellJoinArgs(cmdParts))
	return strings.Join(commands, " && ")
}

func (env espIDFEnv) ESPIDFMCPServerScript(projectDir string) string {
	return env.ESPIDFShellScript([]string{"mcp-server"}, projectDir)
}

func pythonBootstrapShellCommand() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	lines := []string{
		"import platform",
		"import subprocess",
		"_reasonix_original_mac_ver = platform.mac_ver",
		"def _reasonix_mac_ver():",
		"    value = _reasonix_original_mac_ver()",
		"    if value[0]:",
		"        return value",
		"    try:",
		"        release = subprocess.check_output([\"/usr/bin/sw_vers\", \"-productVersion\"], text=True).strip()",
		"    except Exception:",
		"        release = \"10.15\"",
		"    return (release, (\"\", \"\", \"\"), platform.machine())",
		"platform.mac_ver = _reasonix_mac_ver",
	}
	printfArgs := append([]string{"printf", "%s\n"}, lines...)
	return strings.Join([]string{
		"REASONIX_IDF_BOOTSTRAP=${TMPDIR:-/tmp}/reasonix-idf-python-bootstrap",
		"mkdir -p \"$REASONIX_IDF_BOOTSTRAP\"",
		shellJoinArgs(printfArgs) + " > \"$REASONIX_IDF_BOOTSTRAP/sitecustomize.py\"",
		"export PYTHONPATH=\"$REASONIX_IDF_BOOTSTRAP:${PYTHONPATH:-}\"",
	}, " && ")
}

func shellJoinArgs(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellArg(p)
	}
	return strings.Join(quoted, " ")
}

func shellArg(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\n\"'\\$`!&;()<>|*?[{") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func shellQuote(value string) string {
	return strconv.Quote(value)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func listArduinoBoards() []boardReport {
	if !hasCommand("arduino-cli") {
		return []boardReport{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "arduino-cli", "board", "list", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return []boardReport{}
	}
	return parseArduinoBoardList(out)
}

func parseArduinoBoardList(out []byte) []boardReport {
	var parsed struct {
		DetectedPorts []struct {
			Port struct {
				Address    string            `json:"address"`
				Protocol   string            `json:"protocol"`
				Properties map[string]string `json:"properties"`
			} `json:"port"`
			MatchingBoards []struct {
				Name string `json:"name"`
				FQBN string `json:"fqbn"`
			} `json:"matching_boards"`
		} `json:"detected_ports"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return []boardReport{}
	}
	boards := []boardReport{}
	for _, item := range parsed.DetectedPorts {
		port := item.Port.Address
		if port == "" || !isLikelyHardwareSerialPort(port) {
			continue
		}
		if len(item.MatchingBoards) == 0 {
			boards = append(boards, boardReport{
				Port:       port,
				Protocol:   item.Port.Protocol,
				BoardName:  "Unknown",
				Properties: compactProperties(item.Port.Properties),
			})
			continue
		}
		for _, board := range item.MatchingBoards {
			boards = append(boards, boardReport{
				Port:       port,
				Protocol:   item.Port.Protocol,
				BoardName:  board.Name,
				FQBN:       board.FQBN,
				Core:       coreFromFQBN(board.FQBN),
				Properties: compactProperties(item.Port.Properties),
			})
		}
	}
	sort.Slice(boards, func(i, j int) bool {
		if boards[i].Port == boards[j].Port {
			return boards[i].FQBN < boards[j].FQBN
		}
		return boards[i].Port < boards[j].Port
	})
	return boards
}

func listPlatformIODevices() []deviceReport {
	if !hasCommand("pio") {
		return []deviceReport{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pio", "device", "list", "--json-output")
	out, err := cmd.Output()
	if err != nil {
		return []deviceReport{}
	}
	return parsePlatformIODeviceList(out)
}

func parsePlatformIODeviceList(out []byte) []deviceReport {
	var parsed []struct {
		Port        string `json:"port"`
		Description string `json:"description"`
		HWID        string `json:"hwid"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return []deviceReport{}
	}
	devices := []deviceReport{}
	for _, item := range parsed {
		if item.Port == "" || !isLikelyHardwareSerialPort(item.Port) {
			continue
		}
		devices = append(devices, deviceReport{Port: item.Port, Description: item.Description, HWID: item.HWID})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Port < devices[j].Port })
	return devices
}

func compactProperties(props map[string]string) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(props[key])
		if value == "" {
			continue
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ", ")
}

func coreFromFQBN(fqbn string) string {
	parts := strings.Split(fqbn, ":")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + ":" + parts[1]
}

func listSerialPorts() []string {
	if runtime.GOOS == "windows" {
		return listWindowsSerialPorts()
	}
	patterns := []string{
		"/dev/cu.usb*", "/dev/cu.SLAB*", "/dev/cu.wchusb*", "/dev/cu.Bluetooth*",
		"/dev/ttyUSB*", "/dev/ttyACM*", "/dev/tty.usb*", "/dev/cu.*",
	}
	seen := map[string]bool{}
	ports := []string{}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if !isLikelyHardwareSerialPort(m) {
				continue
			}
			if !seen[m] {
				seen[m] = true
				ports = append(ports, m)
			}
		}
	}
	sort.Strings(ports)
	return ports
}

// listWindowsSerialPorts 枚举 Windows 上的 COM 口。以前这里直接返回空,Windows 老师在装好
// arduino-cli/pio 之前根本看不到串口、烧录键锁死(国内机房绝大多数是 Windows)。用
// PowerShell 的 SerialPort.GetPortNames()——不依赖第三方库、最可靠;回退到注册表
// SERIALCOMM(reg query)。返回如 ["COM3","COM5"](#3a)。
func listWindowsSerialPorts() []string {
	if out, err := runCommandText("powershell", []string{"-NoProfile", "-Command", "[System.IO.Ports.SerialPort]::GetPortNames()"}, "", 8*time.Second); err == nil {
		if ports := parseWindowsCOMPorts(out); len(ports) > 0 {
			return ports
		}
	}
	if out, err := runCommandText("reg", []string{"query", `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM`}, "", 8*time.Second); err == nil {
		if ports := parseWindowsCOMPorts(out); len(ports) > 0 {
			return ports
		}
	}
	return []string{}
}

// parseWindowsCOMPorts 从命令输出里抽出形如 COM<数字> 的端口名(去重排序)。
func parseWindowsCOMPorts(output string) []string {
	seen := map[string]bool{}
	var ports []string
	for _, tok := range strings.Fields(strings.ReplaceAll(output, "\r", " ")) {
		if isWindowsCOMPort(tok) && !seen[tok] {
			seen[tok] = true
			ports = append(ports, tok)
		}
	}
	sort.Strings(ports)
	return ports
}

func isWindowsCOMPort(s string) bool {
	if len(s) < 4 || !strings.EqualFold(s[:3], "COM") {
		return false
	}
	for _, c := range s[3:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isLikelyHardwareSerialPort(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.Contains(base, "bluetooth") || strings.Contains(base, "debug-console") {
		return false
	}
	return true
}

func detectProjectTypes(dir string) []string {
	out := []string{}
	if exists(filepath.Join(dir, "platformio.ini")) {
		out = append(out, "platformio")
	}
	if exists(filepath.Join(dir, "sdkconfig")) || (exists(filepath.Join(dir, "CMakeLists.txt")) && exists(filepath.Join(dir, "main", "CMakeLists.txt"))) {
		out = append(out, "esp_idf")
	}
	if len(findRootArduinoSketchDirs(dir)) > 0 {
		out = append(out, "arduino")
	}
	if hasPythonEntrypoint(dir) {
		// 先按 import 把 Python 细分到具体子平台(行空板/MaixCAM/树莓派/真 MicroPython)。
		// 同样是 .py,这几个平台用的库完全不同;只看到 .py 就一律当成 micropython,
		// 会让弱模型走错脚手架和部署路径。识别得到就把具体平台排在前面,
		// 仍保留 python_or_micropython 兜底,旧的匹配逻辑不受影响。
		if specific := detectPythonPlatform(dir); specific != "" {
			out = append(out, specific)
		}
		out = append(out, "python_or_micropython")
	}
	if len(out) == 0 {
		out = append(out, "unknown")
	}
	return out
}

func detectHardwareProjectCandidates(root string) []projectCandidateReport {
	root = filepath.Clean(root)
	out := []projectCandidateReport{}
	seen := map[string]bool{}
	const maxCandidates = 12
	const maxDepth = 4

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && shouldSkipProjectDir(d.Name()) {
			return filepath.SkipDir
		}
		if depthFromRoot(root, path) > maxDepth {
			return filepath.SkipDir
		}
		types := projectTypesWithoutUnknown(detectProjectTypes(path))
		if len(types) == 0 {
			return nil
		}
		if seen[path] {
			return nil
		}
		seen[path] = true
		out = append(out, projectCandidateReport{
			Dir:   path,
			Kind:  strings.Join(types, ","),
			Entry: projectCandidateEntry(path, types),
		})
		if len(out) >= maxCandidates {
			return filepath.SkipDir
		}
		if path != root {
			return filepath.SkipDir
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir == root {
			return true
		}
		if out[j].Dir == root {
			return false
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

func projectTypesWithoutUnknown(types []string) []string {
	out := []string{}
	for _, typ := range types {
		if typ != "" && typ != "unknown" {
			out = append(out, typ)
		}
	}
	return out
}

func projectCandidateEntry(dir string, types []string) string {
	for _, typ := range types {
		switch typ {
		case "platformio":
			return filepath.Join(dir, "platformio.ini")
		case "arduino":
			sketches := findRootINOSketches(dir)
			if len(sketches) > 0 {
				return sketches[0]
			}
		case "esp_idf":
			return filepath.Join(dir, "CMakeLists.txt")
		case "python_or_micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python", "micropython":
			if entry := pythonEntrypointPath(dir); entry != "" {
				return entry
			}
		}
	}
	return ""
}

func depthFromRoot(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func formatCandidateDirs(root string, dirs []string, limit int) string {
	if len(dirs) == 0 {
		return "无"
	}
	out := []string{}
	for i, dir := range dirs {
		if i >= limit {
			out = append(out, fmt.Sprintf("另有 %d 个", len(dirs)-limit))
			break
		}
		rel, err := filepath.Rel(root, dir)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			out = append(out, rel)
		} else {
			out = append(out, dir)
		}
	}
	return strings.Join(out, "、")
}

func hasPythonEntrypoint(dir string) bool {
	return pythonEntrypointPath(dir) != ""
}

// pythonEntrypointPath 返回项目里第一个存在的 Python 入口文件的绝对路径(没有则空串)。
func pythonEntrypointPath(dir string) string {
	for _, rel := range []string{
		"boot.py",
		"main.py",
		filepath.Join("src", "main.py"),
		filepath.Join("src", "boot.py"),
		filepath.Join("src", "app.py"),
	} {
		full := filepath.Join(dir, rel)
		if exists(full) {
			return full
		}
	}
	return ""
}

// detectPythonPlatform 读取 Python 入口文件的 import,判断具体的 Python 子平台。
// 命中返回 unihiker_python / maixcam_python / raspberry_pi_python / micropython,
// 无法判断时返回空串(交给上层用 python_or_micropython 兜底)。
func detectPythonPlatform(dir string) string {
	entry := pythonEntrypointPath(dir)
	if entry == "" {
		return ""
	}
	data, err := os.ReadFile(entry)
	if err != nil {
		return ""
	}
	src := strings.ToLower(string(data))
	switch {
	case strings.Contains(src, "from maix import") || strings.Contains(src, "import maix"):
		return "maixcam_python" // MaixCAM (K230) MaixPy v4
	case strings.Contains(src, "pinpong") || strings.Contains(src, "from unihiker"):
		return "unihiker_python" // 行空板:GPIO 用 pinpong,UI 用 unihiker
	case strings.Contains(src, "rpi.gpio") ||
		strings.Contains(src, "import gpiozero") || strings.Contains(src, "from gpiozero") ||
		strings.Contains(src, "picamera") || strings.Contains(src, "import lgpio"):
		return "raspberry_pi_python" // 树莓派:gpiozero/RPi.GPIO/picamera2
	case strings.Contains(src, "from machine import") || strings.Contains(src, "import machine"):
		return "micropython" // ESP32 等跑真 MicroPython(machine 模块)
	}
	return ""
}

func findArduinoSketchDirs(dir string) []string {
	seen := map[string]bool{}
	out := []string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// 跳过 libraries/(脚手架放自带依赖如 LVGL 的地方):里面的示例 .ino 不是本项目的
		// sketch,扫进来会被误当项目编译(之前 B 端瓦力 就把 LVGL 示例也编了)。
		if path != dir && d.IsDir() && (shouldSkipProjectDir(d.Name()) || d.Name() == "libraries") {
			return filepath.SkipDir
		}
		if d.IsDir() || strings.ToLower(filepath.Ext(path)) != ".ino" {
			return nil
		}
		parent := filepath.Dir(path)
		if seen[parent] {
			return nil
		}
		seen[parent] = true
		out = append(out, parent)
		return nil
	})
	sort.Strings(out)
	return out
}

func findRootArduinoSketchDirs(dir string) []string {
	if len(findRootINOSketches(dir)) == 0 {
		return nil
	}
	return []string{dir}
}

func findRootINOSketches(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := []string{}
	for _, entry := range entries {
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".ino" {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files
}

func chooseRootINO(projectDir, source string) (string, error) {
	if strings.TrimSpace(source) != "" {
		path := source
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectDir, path)
		}
		path = filepath.Clean(path)
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || strings.Contains(rel, string(filepath.Separator)) {
			return "", errors.New("source_ino must be a root-level .ino inside the project directory")
		}
		if strings.ToLower(filepath.Ext(path)) != ".ino" {
			return "", errors.New("source_ino must point to a .ino file")
		}
		if !exists(path) {
			return "", errors.New("source_ino not found: " + path)
		}
		return path, nil
	}
	sketches := findRootINOSketches(projectDir)
	switch len(sketches) {
	case 0:
		return "", errors.New("no root-level .ino file found")
	case 1:
		return sketches[0], nil
	default:
		return "", errors.New("multiple root-level .ino files found; pass source_ino")
	}
}

func convertINOToPlatformIOCPP(body string) string {
	if strings.Contains(body, "<Arduino.h>") || strings.Contains(body, "\"Arduino.h\"") {
		return body
	}
	return "#include <Arduino.h>\n\n" + body
}

func quotedHeaderIncludes(body string) []string {
	includes := []string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#include") {
			continue
		}
		start := strings.Index(line, "\"")
		if start < 0 {
			continue
		}
		end := strings.Index(line[start+1:], "\"")
		if end < 0 {
			continue
		}
		include := line[start+1 : start+1+end]
		if safeRelativeHeader(include) {
			includes = append(includes, include)
		}
	}
	return uniqueStrings(includes)
}

func safeRelativeHeader(include string) bool {
	include = strings.TrimSpace(include)
	if include == "" || filepath.IsAbs(include) || strings.Contains(include, "\\") {
		return false
	}
	clean := filepath.Clean(include)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return false
	}
	return clean == include
}

func platformIOHeaderCopyActions(projectDir string, includes []string) []repairAction {
	actions := []repairAction{}
	for _, include := range includes {
		rootHeader := filepath.Join(projectDir, include)
		includeHeader := filepath.Join(projectDir, "include", include)
		if !exists(rootHeader) || exists(includeHeader) {
			continue
		}
		actions = append(actions, repairAction{
			Kind:    "copy",
			Path:    includeHeader,
			Status:  "planned",
			Message: "把根目录被引用的头文件复制到 PlatformIO include/ 搜索路径。",
		})
	}
	return actions
}

func findPlatformIOSourceFiles(dir string) []string {
	srcDir := filepath.Join(dir, "src")
	files := []string{}
	_ = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != srcDir && d.IsDir() && shouldSkipProjectDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".c", ".cc", ".cpp", ".cxx", ".ino":
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func findPythonFiles(dir string) []string {
	files := []string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != dir && d.IsDir() && shouldSkipProjectDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || strings.ToLower(filepath.Ext(path)) != ".py" {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files
}

func shouldSkipProjectDir(name string) bool {
	switch name {
	case ".git", ".pio", ".venv", "venv", "__pycache__", "node_modules", "build", "dist":
		return true
	default:
		return false
	}
}

func pythonCommand() string {
	for _, name := range []string{"python3", "python"} {
		if hasCommand(name) {
			return name
		}
	}
	return ""
}

func firstCommandLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "$ ") {
			return strings.TrimPrefix(line, "$ ")
		}
	}
	return ""
}

func commandOutputHasBody(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "(cwd: ") || strings.HasPrefix(line, "--- ") {
			continue
		}
		return true
	}
	return false
}

func firstLineContaining(output, needle string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, needle) {
			return line
		}
	}
	return firstLine(strings.TrimSpace(output))
}

func firstLineWithPrefix(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// networkBoardReport 描述"不走串口"的网络型板卡(行空板等):它们通过 USB 网络
// 或 WiFi 以 SSH 方式连接,串口列表永远是空的——必须单独探测并明示,否则用户
// (实测连创始人本人)会误以为板子没连上。
type networkBoardReport struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
	Notes     string `json:"notes,omitempty"`
}

// detectNetworkBoards 只探已知出厂地址(TCP 拨 SSH 端口,800ms 超时),
// 不广播、不扫网段;只返回可达的,避免面板挂一排灰色噪音。
func detectNetworkBoards() []networkBoardReport {
	known := []networkBoardReport{
		{Name: "Unihiker 行空板", Host: "10.1.2.3", Port: 22,
			Notes: "USB 网络模式出厂地址;SSH 默认 root/dfrobot,部署运行用 ssh_deploy_run"},
	}
	out := []networkBoardReport{}
	for _, b := range known {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(b.Host, strconv.Itoa(b.Port)), 800*time.Millisecond)
		if err != nil {
			continue
		}
		conn.Close()
		b.Reachable = true
		out = append(out, b)
	}
	return out
}

func recommendations(r detectReport) []string {
	rec := []string{}
	tool := map[string]bool{}
	for _, t := range r.Toolchains {
		tool[t.Name] = t.Available
	}
	for _, nb := range r.NetworkBoards {
		rec = append(rec, "检测到 "+nb.Name+"（网络连接 "+nb.Host+"，SSH 可达）——该板卡不走串口，「串口无设备」属正常；"+nb.Notes+"。")
	}
	if len(r.SerialPorts) == 0 && len(r.NetworkBoards) == 0 {
		rec = append(rec, "未发现常见 USB 串口；请检查数据线、驱动、开发板供电，或手动指定端口。")
	}
	if len(r.Boards) > 0 {
		first := r.Boards[0]
		if first.FQBN != "" {
			rec = append(rec, "检测到 Arduino 兼容开发板，可用端口 "+first.Port+" 和 FQBN "+first.FQBN+" 进行编译/上传。")
		}
	} else if len(r.Devices) > 0 {
		rec = append(rec, "检测到 PlatformIO 串口设备；请根据板卡型号确认 board/environment 后再上传。")
	}
	if contains(r.ProjectTypes, "esp_idf") && tool["Espressif EIM"] {
		rec = append(rec, "ESP-IDF 项目建议优先接入官方 Tools MCP：使用 esp_idf_mcp_config 生成配置。")
	}
	if contains(r.ProjectTypes, "platformio") && !tool["PlatformIO"] {
		rec = append(rec, "检测到 platformio.ini，但 pio 不在 PATH——没有它无法编译/烧录。安装：`brew install platformio`（macOS）或 `pipx install platformio`；装好后重新运行 hardware_detect 确认。")
	}
	if contains(r.ProjectTypes, "arduino") && !tool["arduino-cli"] {
		rec = append(rec, "检测到 Arduino sketch，但 arduino-cli 不在 PATH——没有它无法编译/烧录。安装：`brew install arduino-cli`（macOS），随后 `arduino-cli core install arduino:avr`（Nano/UNO）或 `arduino-cli core install esp32:esp32`（ESP32）；装好后重新运行 hardware_detect 确认。")
	}
	return rec
}

func espIDFMCPHint(projectDir, serverName string, useEIM bool) map[string]string {
	if serverName == "" {
		serverName = "esp-idf-tools"
	}
	var tomlSnippet string
	var mcpSnippet string
	local := resolveLocalESPIDF()
	if useEIM && hasEIM() {
		eim := resolveEIMPath()
		if eim == "" {
			eim = "eim"
		}
		commandLine := "idf.py mcp-server"
		tomlSnippet = fmt.Sprintf("[[plugins]]\nname = %q\ncommand = %q\nargs = [\"run\", %q]\nenv = { IDF_MCP_WORKSPACE_FOLDER = %q }\n", serverName, eim, commandLine, projectDir)
		mcpSnippet = fmt.Sprintf("{\n  \"mcpServers\": {\n    %q: {\n      \"command\": %q,\n      \"args\": [\"run\", %q],\n      \"env\": { \"IDF_MCP_WORKSPACE_FOLDER\": %q }\n    }\n  }\n}", serverName, eim, commandLine, projectDir)
	} else if local.Available {
		script := local.ESPIDFMCPServerScript(projectDir)
		tomlSnippet = fmt.Sprintf("[[plugins]]\nname = %q\ncommand = \"bash\"\nargs = [\"-lc\", %q]\n", serverName, script)
		mcpSnippet = fmt.Sprintf("{\n  \"mcpServers\": {\n    %q: {\n      \"command\": \"bash\",\n      \"args\": [\"-lc\", %q]\n    }\n  }\n}", serverName, script)
	} else {
		tomlSnippet = fmt.Sprintf("[[plugins]]\nname = %q\ncommand = \"idf.py\"\nargs = [\"-C\", %q, \"mcp-server\"]\n", serverName, projectDir)
		mcpSnippet = fmt.Sprintf("{\n  \"mcpServers\": {\n    %q: {\n      \"command\": \"idf.py\",\n      \"args\": [\"-C\", %q, \"mcp-server\"]\n    }\n  }\n}", serverName, projectDir)
	}
	return map[string]string{"reasonix.toml": tomlSnippet, ".mcp.json": mcpSnippet}
}

// --- scaffold templates ---

func scaffoldArduino(name, board string) map[string]string {
	sketchDir := name
	return map[string]string{
		"README.md": hardwareReadme(name, "Arduino", board, verificationCommand("arduino", board, name)),
		filepath.ToSlash(filepath.Join(sketchDir, sketchDir+".ino")): `// ` + name + ` - Arduino 教学示例
// 目标：先验证串口输出，再逐步连接传感器和执行器。

const int LED_PIN = 13;                 // 板载 LED 引脚
const unsigned long LOOP_INTERVAL = 500; // 主循环间隔，单位：毫秒

unsigned long last_loop_time = 0;        // 记录上一次执行任务的时间
bool led_on = false;                     // 记录 LED 当前状态

// setup 函数在开发板上电或复位后只运行一次
void setup() {
  Serial.begin(115200);                  // 打开串口，方便观察程序运行状态
  pinMode(LED_PIN, OUTPUT);              // 把 LED 引脚设置为输出模式
  Serial.println("system ready");        // 输出启动提示，证明烧录成功
}

// loop 函数会一直重复运行
void loop() {
  unsigned long now = millis();          // 读取当前运行时间

  // 到达固定时间间隔后，再执行一次任务，避免使用 delay 阻塞程序
  if (now - last_loop_time >= LOOP_INTERVAL) {
    last_loop_time = now;                // 更新时间戳
    led_on = !led_on;                    // 反转 LED 状态
    digitalWrite(LED_PIN, led_on ? HIGH : LOW);
    Serial.println(led_on ? "led:on" : "led:off");
  }
}
`,
	}
}

func scaffoldPlatformIO(name, board string) map[string]string {
	env := platformIOEnv(board)
	boardID := platformIOBoardID(board)
	return map[string]string{
		"README.md":      hardwareReadme(name, "PlatformIO", boardID, verificationCommand("platformio", board, name)),
		"platformio.ini": platformIOINI(name, boardID, env),
		"include/pins.h": `#ifndef PINS_H
#define PINS_H

// 引脚配置集中放在这里，方便学生答辩时说明每根线的作用。
const int PIN_LED = 2;       // ESP32 常见板载 LED 引脚，实际项目可按接线修改
const int PIN_UART_RX = 16;  // 外设串口 RX
const int PIN_UART_TX = 17;  // 外设串口 TX

#endif
`,
		"include/config.h": `#ifndef CONFIG_H
#define CONFIG_H

// 系统参数集中放在这里，避免代码中出现难解释的魔数。
const unsigned long SERIAL_BAUD = 115200;
const unsigned long LOOP_INTERVAL = 500;

#endif
`,
		"src/main.cpp": `#include <Arduino.h>
#include "pins.h"
#include "config.h"

unsigned long last_loop_time = 0;  // 记录上一次任务执行时间
bool led_on = false;               // 记录 LED 状态

// setup 函数负责初始化串口和引脚
void setup() {
  Serial.begin(SERIAL_BAUD);
  pinMode(PIN_LED, OUTPUT);
  Serial.println("platformio project ready");
}

// loop 函数负责周期性执行主任务
void loop() {
  unsigned long now = millis();

  // 使用非阻塞计时，后续可以同时处理传感器、通信和执行器
  if (now - last_loop_time >= LOOP_INTERVAL) {
    last_loop_time = now;
    led_on = !led_on;
    digitalWrite(PIN_LED, led_on ? HIGH : LOW);
    Serial.println(led_on ? "led:on" : "led:off");
  }
}
`,
	}
}

func scaffoldESPIDF(name, board string) map[string]string {
	return map[string]string{
		"README.md": hardwareReadme(name, "ESP-IDF", board, verificationCommand("esp_idf", board, name)),
		"CMakeLists.txt": `cmake_minimum_required(VERSION 3.16)
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(` + name + `)
`,
		"main/CMakeLists.txt": `idf_component_register(SRCS "main.c"
                    INCLUDE_DIRS ".")
`,
		"main/main.c": `#include <stdio.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"

static const char *TAG = "` + name + `";
static const int LOOP_INTERVAL_MS = 1000;  // 主任务间隔，单位：毫秒

// app_main 是 ESP-IDF 程序入口，负责初始化系统并启动主循环
void app_main(void)
{
    int counter = 0;  // 记录循环次数，方便串口观察程序是否持续运行

    ESP_LOGI(TAG, "ESP-IDF project ready");

    while (1) {
        counter++;
        ESP_LOGI(TAG, "loop counter: %d", counter);
        vTaskDelay(pdMS_TO_TICKS(LOOP_INTERVAL_MS));
    }
}
`,
		"main/Kconfig.projbuild": `menu "` + name + `"

config ONEUP_SAMPLE_INTERVAL_MS
    int "Sample interval in milliseconds"
    default 1000
    help
        教学示例参数：主循环采样间隔。

endmenu
`,
	}
}

func scaffoldMicroPython(name, board string) map[string]string {
	return map[string]string{
		"README.md": hardwareReadme(name, "MicroPython", board, verificationCommand("micropython", board, name)),
		"src/main.py": `import time
from machine import Pin

SERIAL_BAUD = 115200
LOOP_INTERVAL = 0.5
LED_PIN = 2

led = Pin(LED_PIN, Pin.OUT)
led_on = False


def toggle_led():
    """切换 LED 状态，并输出当前状态"""
    global led_on
    led_on = not led_on
    led.value(1 if led_on else 0)
    print("led:on" if led_on else "led:off")


def main():
    """程序入口：周期性切换 LED，用于验证开发板运行正常"""
    print("micropython project ready")
    while True:
        toggle_led()
        time.sleep(LOOP_INTERVAL)


main()
`,
	}
}

func scaffoldLinuxPython(name, board, label, defaultTarget string) map[string]string {
	return map[string]string{
		"README.md": hardwareReadme(name, label, board, verificationCommand(labelToPlatform(label), board, name)),
		"requirements.txt": `# 按项目需要添加依赖，例如：
# opencv-python
# numpy
`,
		"src/main.py": `import time

LOOP_INTERVAL = 1.0


def read_sensor():
    """读取传感器数据；第一版先返回模拟值，接线后再替换为真实读取"""
    value = 0
    return value


def update_display(value):
    """更新屏幕或终端显示，让学生能看到程序运行状态"""
    print(f"sensor value: {value}")


def main():
    """程序入口：读取数据、显示数据，并保留清晰的主循环结构"""
    print("python hardware project ready")
    while True:
        value = read_sensor()
        update_display(value)
        time.sleep(LOOP_INTERVAL)


if __name__ == "__main__":
    main()
`,
	}
}

func scaffoldMaixCAM(name, board string) map[string]string {
	return map[string]string{
		"README.md": hardwareReadme(name, "MaixCAM / MaixPy", board, verificationCommand("maixcam_python", board, name)),
		"src/main.py": `from maix import camera, display, image, time


def detect_color(frame):
    """检测画面中的颜色区域；第一版先保留接口，后续按项目颜色阈值补充"""
    return None


def main():
    """程序入口：打开摄像头，实时显示画面，为视觉识别项目打底"""
    cam = camera.Camera(320, 240)
    disp = display.Display()

    while True:
        frame = cam.read()
        result = detect_color(frame)
        if result is not None:
            print(result)
        disp.show(frame)
        time.sleep_ms(10)


main()
`,
	}
}

func hardwareReadme(name, platform, board, verify string) string {
	return fmt.Sprintf(strings.Join([]string{
		"# %s",
		"",
		"平台：%s",
		"开发板：%s",
		"",
		"## 目标",
		"",
		"这个项目是 AI 硬件编程的第一版脚手架。先完成最小可验证原型，再逐步加入传感器、执行器、视觉识别或联网功能。",
		"",
		"## 目录",
		"",
		"- src/：主程序代码",
		"- include/：引脚和配置常量（如果该平台需要）",
		"- docs/：技术方案、接线说明、研究日志",
		"- tests/：验证脚本或串口测试记录",
		"- hardware_manifest.json：AI 可读取的硬件项目元数据",
		"",
		"## 验证命令",
		"",
		"```bash",
		"%s",
		"```",
		"",
		"## 教学要求",
		"",
		"- 每个函数开头说明这个函数做什么。",
		"- 关键逻辑使用中文注释。",
		"- 魔数提取为命名常量。",
		"- 学生需要能解释每一行代码的作用。",
		"",
	}, "\n"), name, platform, board, verify)
}

func commonHardwareProjectFiles(name, platform, board, verify string) map[string]string {
	files := map[string]string{
		"hardware_manifest.json":      hardwareManifest(name, platform, board, verify),
		"docs/wiring.md":              wiringGuide(name, platform, board),
		"docs/verification.md":        verificationGuide(name, platform, board, verify),
		"tests/hardware_checklist.md": hardwareChecklist(name, platform, board, verify),
	}
	profile, ok := findBoardProfile(board, platform)
	if ok {
		files["docs/board_profile.md"] = boardProfileMarkdown(profile)
	}
	// 自动注入已校验的模块地面真值(库/引脚/I2C 地址/平台 API),
	// 从默认连接解析;冷门板(MaixCAM/行空板)的板名会命中平台 API。
	if md := moduleSpecsForConnections(defaultHardwareConnections(platform, board), board); md != "" {
		files["docs/module_specs.md"] = md
	}
	files["docs/failure_patterns.md"] = failurePatternsMarkdown(platform)
	return files
}

func standardHardwareContextFiles(name, platform, board, verify string) map[string]string {
	files := commonHardwareProjectFiles(name, platform, board, verify)
	files["README.md"] = hardwareReadme(name, platform, board, verify)
	return files
}

func standardHardwareContextFilesForProject(projectDir, name, platform, board, verify string) map[string]string {
	files := standardHardwareContextFiles(name, platform, board, verify)
	entrypoints := contextEntrypoints(projectDir, platform, name)
	files["hardware_manifest.json"] = hardwareManifestWithEntrypoints(name, platform, board, verify, entrypoints)
	return files
}

func hardwareManifest(name, platform, board, verify string) string {
	return hardwareManifestWithEntrypoints(name, platform, board, verify, platformEntrypoints(platform, name))
}

func hardwareManifestWithEntrypoints(name, platform, board, verify string, entrypoints []string) string {
	return hardwareManifestWithEntrypointsAndConnections(name, platform, board, verify, entrypoints, nil)
}

// hardwareManifestWithEntrypointsAndConnections 允许调用方覆盖默认 connections。
// 用于学生在 scaffold 时声明了模块名(如 ["DHT11","SSD1306"])——
// modulesToConnections 会把它们解析成真实连接,manifest 里的 connections 不再是占位。
func hardwareManifestWithEntrypointsAndConnections(name, platform, board, verify string, entrypoints []string, connections []hardwareConnection) string {
	if len(entrypoints) == 0 {
		entrypoints = platformEntrypoints(platform, name)
	}
	if connections == nil {
		connections = defaultHardwareConnections(platform, board)
	}
	data := struct {
		SchemaVersion      string               `json:"schemaVersion"`
		ProjectName        string               `json:"projectName"`
		Platform           string               `json:"platform"`
		Board              string               `json:"board"`
		Entrypoints        []string             `json:"entrypoints"`
		Layout             []map[string]string  `json:"layout"`
		PreferredProtocols []string             `json:"preferredProtocols"`
		Connections        []hardwareConnection `json:"connections"`
		MCPTools           []string             `json:"mcpTools"`
		Verification       map[string]any       `json:"verification"`
		TeachingRules      []string             `json:"teachingRules"`
		AIWorkflow         []string             `json:"aiWorkflow"`
	}{
		SchemaVersion:      "reasonix-hardware-project/v1",
		ProjectName:        name,
		Platform:           platform,
		Board:              board,
		Entrypoints:        entrypoints,
		Layout:             hardwareProjectLayoutWithEntrypoints(name, entrypoints),
		PreferredProtocols: []string{"UART", "I2C", "SPI", "WiFi/MQTT/HTTP"},
		Connections:        connections,
		MCPTools:           []string{"hardware_detect", "hardware_board_profile", "hardware_project_context", "hardware_project_audit", "hardware_project_validate", "hardware_repair_catalog", "hardware_evidence_record", "hardware_evidence_status", "hardware_device_verify_plan", uploadToolForPlatform(platform), monitorToolForPlatform(platform)},
		Verification: map[string]any{
			"localCommand":       verify,
			"minimumLocalGate":   "compile_or_syntax",
			"realHardwareChecks": []string{"detect_board", "upload_or_deploy", "serial_or_runtime_log", "wiring_and_power"},
		},
		TeachingRules: []string{"中文注释", "命名常量", "函数职责清晰", "学生能逐行解释"},
		AIWorkflow: []string{
			"先调用 hardware_board_profile 读取板卡默认引脚、电压风险、工具链和验证流程。",
			"写传感器/模块/视觉/舵机代码前,先读 docs/module_specs.md(已校验的库、引脚、I2C 地址、平台 API);需要更多模块时调用 hardware_module_spec，不要凭记忆猜库名和引脚。",
			"先读取 hardware_manifest.json 判断平台、入口文件和验证命令。",
			"如果 README、docs/wiring.md、docs/verification.md、docs/board_profile.md、docs/failure_patterns.md 或 tests/hardware_checklist.md 缺失，先调用 hardware_project_context 补齐。",
			"遇到 audit/validate 失败时，先调用 hardware_repair_catalog 对照失败模式；有自动修复工具时先做最小修复。",
			"修改引脚或协议后同步更新 docs/wiring.md。",
			"提交前运行 hardware_project_audit 和 hardware_project_validate，用 hardware_evidence_record 记录证据，再用 hardware_device_verify_plan 生成实机命令，最后用 hardware_evidence_status 判断是否仍缺真实硬件阶段。",
		},
	}
	b, _ := json.MarshalIndent(data, "", "  ")
	return string(b) + "\n"
}

func wiringGuide(name, platform, board string) string {
	rows := strings.Join(defaultWiringRows(platform), "\n")
	connectionRows := markdownConnectionRows(defaultHardwareConnections(platform, board))
	return fmt.Sprintf(`# %s 接线说明

平台：%s
开发板：%s

## 接线原则

1. 先只连接开发板和 USB，确认能编译、烧录或运行。
2. 再连接一个传感器，验证输入数据稳定后再接执行器。
3. 板间通信优先 UART；同板多设备优先 I2C；高速设备使用 SPI。
4. ESP32、MaixCAM、Unihiker、Raspberry Pi 通常是 3.3V 逻辑，接 5V 模块前必须确认电平兼容。

## 默认信号

- 串口波特率：115200
- 日志关键字：ready、led:on、led:off、sensor value
- 第一轮目标：看到稳定日志，不追求完整功能。
- AI 修改引脚、通信协议或外设数量后，必须同步更新本文件和 hardware_manifest.json。

## 平台默认建议

%s

## Manifest 连接清单

以下内容来自 'hardware_manifest.json' 的 'connections' 字段。AI 修改引脚、协议、电压或外设后，必须同步更新这里和 manifest。

| 模块 | 角色 | 协议 | 引脚/接口 | 电压 | 备注 |
|------|------|------|-----------|------|------|
%s

## 现场记录表

| 模块 | 引脚/接口 | 电压 | 作用 | 已验证 |
|------|-----------|------|------|--------|
| 开发板 | USB | 5V | 供电和下载 | 否 |
| 传感器 | 待填写 | 待确认 | 输入数据 | 否 |
| 执行器 | 待填写 | 待确认 | 输出动作 | 否 |
`, name, platform, board, rows, connectionRows)
}

func verificationGuide(name, platform, board, verify string) string {
	toolSequence := strings.Join(hardwareToolSequence(platform), "\n")
	return fmt.Sprintf(`# %s 验证流程

平台：%s
开发板：%s

## 1. 本机验证

~~~bash
%s
~~~

也可以在 Reasonix 中调用 'hardware_project_validate'，让 MCP 自动选择编译或语法检查流程。

## 2. 连接真实硬件后

1. 运行 'hardware_detect'，确认串口或 SSH 目标。
2. 运行 'hardware_device_verify_plan'，生成当前项目的实机验证命令和缺失参数。
3. 编译通过后再 upload、flash、mpremote 或 ssh deploy。
4. 采集串口或运行日志，至少保留 5 行稳定输出。
5. 如果没有输出，优先检查端口、波特率、供电、接线和复位。

## 3. Reasonix MCP 工具顺序

%s

每完成一次 audit、compile/syntax、upload/flash、monitor、mpremote 或 ssh deploy，都调用 'hardware_evidence_record' 把命令、状态和关键输出写入 tests/hardware_evidence.jsonl。

## 4. 失败判断

- 编译失败：先看缺失库、板卡型号、语法错误。
- 烧录失败：先看端口、驱动、BOOT 按键、串口是否被占用。
- 无串口输出：先看波特率、程序是否重启、供电和日志关键字。
- 传感器数据异常：先断开执行器，只保留一个传感器做最小验证。

## 5. 答辩记录

- 学生能说明每个模块的作用。
- 学生能说明每条关键日志代表什么。
- 学生能说明失败时下一步检查什么。
`, name, platform, board, verify, toolSequence)
}

func hardwareChecklist(name, platform, board, verify string) string {
	return fmt.Sprintf(`# %s 硬件检查清单

平台：%s
开发板：%s

## 本机验证

- [ ] 已运行 '%s'。
- [ ] 已运行 'hardware_project_validate'，结果为 passed。
- [ ] README、docs/wiring.md、docs/verification.md、hardware_manifest.json 已随代码变化更新。

## 真实硬件验证

- [ ] 真实开发板已连接并能被工具检测到。
- [ ] 烧录、上传或部署命令执行成功。
- [ ] 串口、屏幕或终端能看到稳定日志。
- [ ] 所有传感器数值在合理范围内，不是固定 0、NaN 或空值。
- [ ] 执行器动作方向和速度安全。
- [ ] 接线照片、串口日志和实验记录已保存。

## 学生答辩检查

- [ ] 学生能说明每个函数做什么。
- [ ] 学生能说明每个引脚或接口为什么这样接。
- [ ] 学生能说明失败时下一步如何定位问题。

## 验证证据记录

使用 'hardware_evidence_record' 追加 compile、upload、monitor、ssh 等证据；结构化日志保存在 'tests/hardware_evidence.jsonl'。
`, name, platform, board, verify)
}

func platformEntrypoints(platform, name string) []string {
	switch platform {
	case "arduino":
		return []string{filepath.ToSlash(filepath.Join(name, name+".ino"))}
	case "platformio":
		return []string{"src/main.cpp", "include/pins.h", "include/config.h"}
	case "esp_idf":
		return []string{"main/main.c", "main/CMakeLists.txt", "CMakeLists.txt"}
	default:
		return []string{"src/main.py"}
	}
}

func hardwareProjectLayout(platform, name string) []map[string]string {
	return hardwareProjectLayoutWithEntrypoints(name, platformEntrypoints(platform, name))
}

func hardwareProjectLayoutWithEntrypoints(name string, entrypoints []string) []map[string]string {
	layout := []map[string]string{
		{"path": "README.md", "purpose": "项目说明、平台、验证命令和教学要求"},
		{"path": "docs/wiring.md", "purpose": "接线、电压、通信协议和引脚记录"},
		{"path": "docs/verification.md", "purpose": "本机验证、真实硬件验证和失败定位流程"},
		{"path": "docs/board_profile.md", "purpose": "板卡默认引脚、风险引脚、电压和工具链约束"},
		{"path": "docs/failure_patterns.md", "purpose": "常见硬件失败模式和自动/手动修复规则"},
		{"path": "tests/hardware_checklist.md", "purpose": "真实硬件检查清单和答辩证据"},
		{"path": "hardware_manifest.json", "purpose": "AI 读取的平台、入口文件和验证元数据"},
	}
	for _, entrypoint := range entrypoints {
		layout = append(layout, map[string]string{"path": entrypoint, "purpose": "平台入口代码或配置"})
	}
	return layout
}

func contextEntrypoints(projectDir, platform, name string) []string {
	switch platform {
	case "platformio":
		srcFiles := findPlatformIOSourceFiles(projectDir)
		if len(srcFiles) > 0 {
			return relativeProjectPaths(projectDir, srcFiles)
		}
		return []string{"src/main.cpp"}
	case "arduino":
		sketchDirs := findArduinoSketchDirs(projectDir)
		out := []string{}
		for _, sketchDir := range sketchDirs {
			sketchName := filepath.Base(sketchDir) + ".ino"
			sketchPath := filepath.Join(sketchDir, sketchName)
			if exists(sketchPath) {
				if rel := relativeProjectPath(projectDir, sketchPath); rel != "" {
					out = append(out, rel)
				}
			}
		}
		if len(out) > 0 {
			sort.Strings(out)
			return out
		}
	case "esp_idf":
		candidates := []string{"main/main.c", "main/CMakeLists.txt", "CMakeLists.txt"}
		existing := existingRelativePaths(projectDir, candidates)
		if len(existing) > 0 {
			return existing
		}
	default:
		candidates := []string{"src/main.py"}
		existing := existingRelativePaths(projectDir, candidates)
		if len(existing) > 0 {
			return existing
		}
	}
	return platformEntrypoints(platform, name)
}

func existingRelativePaths(projectDir string, candidates []string) []string {
	out := []string{}
	for _, rel := range candidates {
		if exists(filepath.Join(projectDir, filepath.FromSlash(rel))) {
			out = append(out, rel)
		}
	}
	return out
}

func relativeProjectPaths(projectDir string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if rel := relativeProjectPath(projectDir, path); rel != "" {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

func relativeProjectPath(projectDir, path string) string {
	rel, err := filepath.Rel(projectDir, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func defaultHardwareConnections(platform, board string) []hardwareConnection {
	switch platform {
	case "arduino":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB", Pins: []string{"USB"}, Voltage: "5V", Notes: "供电、编译上传和串口日志。"},
			{Name: "板载 LED", Role: "actuator", Protocol: "GPIO", Pins: []string{"D13"}, Voltage: "5V", Notes: "第一版最小输出验证，不需要外接 LED。"},
		}
	case "platformio":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB", Pins: []string{"USB"}, Voltage: "5V", Notes: "供电、上传和串口日志。"},
			{Name: "板载 LED", Role: "actuator", Protocol: "GPIO", Pins: []string{"GPIO2"}, Voltage: "3.3V", Notes: "ESP32 DevKit 常见板载 LED；不同开发板可改 include/pins.h。"},
			{Name: "预留 UART 总线", Role: "communication", Protocol: "UART", Pins: []string{"GPIO16", "GPIO17", "GND"}, Voltage: "3.3V", Notes: "用于和 Arduino/MaixCAM/Unihiker 通信，接线前确认实际板卡引脚。"},
		}
	case "esp_idf":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB", Pins: []string{"USB"}, Voltage: "5V", Notes: "供电、flash 和 monitor。"},
			{Name: "ESP-IDF 日志串口", Role: "debug", Protocol: "USB_SERIAL", Pins: []string{"USB"}, Voltage: "3.3V", Notes: "通过 idf.py monitor 采集日志。"},
		}
	case "micropython":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB", Pins: []string{"USB"}, Voltage: "5V", Notes: "供电、mpremote 运行和 REPL。"},
			{Name: "板载 LED", Role: "actuator", Protocol: "GPIO", Pins: []string{"GPIO2"}, Voltage: "3.3V", Notes: "第一版最小输出验证。"},
		}
	case "unihiker_python":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB_OR_WIFI", Pins: []string{"USB", "WiFi"}, Voltage: "5V", Notes: "供电、SSH 部署和屏幕交互。"},
			{Name: "板载屏幕", Role: "display", Protocol: "INTERNAL", Voltage: "3.3V", Notes: "用于课堂演示和状态反馈。"},
			{Name: "预留 UART 总线", Role: "communication", Protocol: "UART", Pins: []string{"TX", "RX", "GND"}, Voltage: "3.3V", Notes: "用于和 ESP32/Arduino 协调。"},
		}
	case "maixcam_python":
		return []hardwareConnection{
			{Name: board, Role: "vision_controller", Protocol: "USB_OR_WIFI", Pins: []string{"USB", "WiFi"}, Voltage: "5V", Notes: "供电、SSH/IDE 部署和调试。"},
			{Name: "内置摄像头", Role: "sensor", Protocol: "CSI", Voltage: "3.3V", Notes: "第一版视觉采集输入。"},
			{Name: "预留 UART 输出", Role: "communication", Protocol: "UART", Pins: []string{"TX", "RX", "GND"}, Voltage: "3.3V", Notes: "向 ESP32/Arduino 输出识别结果。"},
		}
	case "raspberry_pi_python":
		return []hardwareConnection{
			{Name: board, Role: "controller", Protocol: "USB_C_OR_GPIO_POWER", Pins: []string{"USB-C", "5V", "GND"}, Voltage: "5V", Notes: "供电、SSH 部署和主控计算。"},
			{Name: "预留 I2C 总线", Role: "sensor_bus", Protocol: "I2C", Pins: []string{"GPIO2/SDA", "GPIO3/SCL", "GND"}, Voltage: "3.3V", Notes: "连接传感器前确认上拉和电平兼容。"},
		}
	default:
		return []hardwareConnection{{Name: board, Role: "controller", Protocol: "USB", Pins: []string{"USB"}, Voltage: "5V", Notes: "第一版只验证最小主循环。"}}
	}
}

// builtInBoardProfiles 现在从共享数据驱动注册表读取(boards.json),不再硬编码。
func builtInBoardProfiles() []boardProfile { return boards.Profiles() }

func filterBoardProfiles(platform string) []boardProfile {
	profiles := builtInBoardProfiles()
	if platform == "" || platform == "auto" {
		return profiles
	}
	filtered := []boardProfile{}
	for _, profile := range profiles {
		if profile.Platform == platform || (platform == "micropython" && strings.Contains(profile.ID, "esp32")) {
			filtered = append(filtered, profile)
		}
	}
	return filtered
}

func boardProfileSummaries(profiles []boardProfile) []boardProfileSummary {
	out := make([]boardProfileSummary, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, boardProfileSummary{
			ID:               profile.ID,
			Label:            profile.Label,
			Platform:         profile.Platform,
			Board:            profile.Board,
			DefaultFramework: profile.DefaultFramework,
			Aliases:          profile.Aliases,
		})
	}
	return out
}

func findBoardProfile(board, platform string) (boardProfile, bool) {
	boardKey := normalizeBoardProfileKey(board)
	profiles := filterBoardProfiles(platform)
	if boardKey == "" && len(profiles) == 1 {
		return profiles[0], true
	}
	for _, profile := range profiles {
		if boardProfileMatches(profile, boardKey) {
			return profile, true
		}
	}
	if platform != "" && platform != "auto" {
		for _, profile := range profiles {
			if profile.Board == defaultBoard(platform) {
				return profile, true
			}
		}
	}
	return boardProfile{}, false
}

func boardProfileMatches(profile boardProfile, boardKey string) bool {
	return boards.Matches(profile, boardKey)
}

func normalizeBoardProfileKey(value string) string {
	return boards.Normalize(value)
}

func boardProfileMarkdown(profile boardProfile) string {
	defaultPins := boardPinMarkdownRows(profile.DefaultPins)
	riskyPins := boardPinMarkdownRows(profile.RiskyPins)
	return fmt.Sprintf(`# %s 板卡 Profile

平台：%s
开发方式：%s
板卡 ID：%s
逻辑电平：%s
默认波特率：%d

## 供电与下载

- 下载方式：%s
- 供电说明：%s

## 推荐协议

%s

## 默认引脚

| 名称 | 引脚 | 协议 | 方向 | 电压 | 说明 |
|------|------|------|------|------|------|
%s

## 风险引脚和限制

| 名称 | 引脚 | 协议 | 方向 | 电压 | 说明 |
|------|------|------|------|------|------|
%s

## 工具链

%s

## 验证流程

%s

## 常见失败

%s

## 教学提示

%s
`, profile.Label, profile.Platform, profile.DefaultFramework, profile.Board, profile.LogicVoltage, profile.DefaultBaud, profile.UploadMethod, profile.PowerNotes, markdownBullets(profile.RecommendedProtocols), defaultPins, riskyPins, markdownBullets(profile.Toolchains), markdownNumbered(profile.ValidationFlow), markdownBullets(profile.CommonFailures), markdownBullets(profile.TeachingNotes))
}

func boardPinMarkdownRows(pins []boardPinRule) string {
	if len(pins) == 0 {
		return "| - | - | - | - | - | - |"
	}
	rows := make([]string, 0, len(pins))
	for _, pin := range pins {
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			markdownCell(pin.Name),
			markdownCell(strings.Join(pin.Pins, ", ")),
			markdownCell(pin.Protocol),
			markdownCell(pin.Direction),
			markdownCell(pin.Voltage),
			markdownCell(pin.Notes),
		))
	}
	return strings.Join(rows, "\n")
}

func markdownBullets(items []string) string {
	if len(items) == 0 {
		return "- 待填写"
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, "- "+item)
	}
	return strings.Join(lines, "\n")
}

func markdownNumbered(items []string) string {
	if len(items) == 0 {
		return "1. 待填写"
	}
	lines := make([]string, 0, len(items))
	for i, item := range items {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, item))
	}
	return strings.Join(lines, "\n")
}

func repairRulesForPlatform(platform string) []repairRule {
	rules := []repairRule{}
	for _, rule := range builtInRepairRules() {
		if platform == "" || platform == "auto" || contains(rule.Platforms, platform) || contains(rule.Platforms, "all") {
			rules = append(rules, rule)
		}
	}
	return rules
}

func builtInRepairRules() []repairRule {
	return []repairRule{
		{
			Code:             "platformio_root_ino_to_src_main",
			Title:            "PlatformIO 项目把 .ino 放在根目录，导致 Nothing to build 或模型改错入口",
			Platforms:        []string{"platformio"},
			DetectedBy:       []string{"hardware_project_audit kind=project_layout failed", "hardware_project_validate kind=platformio_layout failed", "PlatformIO output contains Nothing to build"},
			AutoRepairTool:   "hardware_project_repair",
			AutoRepair:       "platformio_root_ino_to_src_main",
			ManualSteps:      []string{"把根目录 .ino 内容迁移到 src/main.cpp", "补充 #include <Arduino.h>", "把根目录被引用的本地头文件复制到 include/", "将原 .ino 移入 .onecreat-backup", "重新运行 hardware_project_validate"},
			EvidenceRequired: []string{"compile"},
		},
		{
			Code:             "missing_project_context",
			Title:            "缺少 hardware_manifest、接线说明、验证说明或硬件检查清单",
			Platforms:        []string{"all"},
			DetectedBy:       []string{"hardware_project_audit kind=manifest failed", "hardware_project_audit kind=file failed", "hardware_project_audit kind=connections failed"},
			AutoRepairTool:   "hardware_project_context",
			ManualSteps:      []string{"调用 hardware_project_context 补齐缺失上下文", "读取 docs/board_profile.md 再出接线方案", "更新真实引脚和外设后重新 audit"},
			EvidenceRequired: []string{"audit"},
		},
		{
			Code:      "arduino_core_not_installed",
			Title:     "编译失败：arduino-cli 已装但开发板 core 未安装（全新环境首次编译最常见）",
			Platforms: []string{"arduino"}, // 仅 arduino-cli 路径;PlatformIO 自管 core,不适用

			DetectedBy:     []string{"Platform .* not found", "Platform .* not installed", "platform is not installed", "Error during build: Platform", "Unknown FQBN", "core .* not installed", "no instance of FQBN"},
			AutoRepairTool: "arduino_core_install",
			ManualSteps: []string{
				"这不是代码问题：arduino-cli 在 PATH 里，但对应开发板的 core 没装。先用 arduino-cli core list 看已装的 core。",
				"直接调用 mcp__hardware__arduino_core_install core=<对应core>，它会自动 update-index 并安装（联网下载，几分钟）。对应关系：Arduino UNO/Nano/Mega → arduino:avr；ESP32 全系 → esp32:esp32；ESP8266 → esp8266:esp8266；树莓派 Pico/RP2040 → rp2040:rp2040。",
				"ESP32/ESP8266/RP2040 的 core 需要第三方 board manager URL，arduino_core_install 已内置这些 URL，不必手动配。",
				"core 装好后重新调用 arduino_compile（FQBN 不变），编译应能通过；再继续 upload/monitor。",
			},
			EvidenceRequired: []string{"compile"},
		},
		{
			Code:             "missing_library_dependency",
			Title:            "编译失败：缺少 Arduino/PlatformIO/Python 依赖库",
			Platforms:        []string{"arduino", "platformio", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"},
			DetectedBy:       []string{"No such file or directory", "library not found", "ModuleNotFoundError", "ImportError"},
			ManualSteps:      []string{"先确认代码实际使用的库名", "PlatformIO 项目优先写入 lib_deps，而不是让用户手动装库", "Arduino CLI 项目给出 arduino-cli lib install 命令", "Python 项目更新 requirements.txt", "重新编译或做语法检查"},
			EvidenceRequired: []string{"compile", "syntax"},
		},
		{
			Code:             "serial_port_unavailable",
			Title:            "烧录或串口失败：端口不存在、被占用或驱动缺失",
			Platforms:        []string{"arduino", "platformio", "esp_idf", "micropython"},
			DetectedBy:       []string{"No such file or directory", "Could not open port", "Resource busy", "Permission denied", "Failed to connect"},
			ManualSteps:      []string{"调用 hardware_detect 重新列出端口", "关闭 Arduino IDE/PlatformIO 串口监视器", "macOS 选 /dev/cu.*，Windows 选 COMx", "ESP32 上传失败时提示按住 BOOT 再重试", "成功后采集 monitor 输出"},
			EvidenceRequired: []string{"upload", "monitor"},
		},
		{
			Code:             "wifi_ap_not_reachable",
			Title:            "ESP32 WiFi AP 或网页项目烧录成功但手机访问不到",
			Platforms:        []string{"platformio", "arduino", "esp_idf"},
			DetectedBy:       []string{"serial log has AP started but phone cannot open page", "browser cannot open 192.168.4.1", "no HTTP request log"},
			ManualSteps:      []string{"串口必须打印 SSID、密码、AP IP 和 HTTP server started", "确认手机连接的是 ESP32 AP，不是公司 WiFi", "优先使用 2.4GHz channel 1/6/11", "把网页逻辑放在手机浏览器，ESP32 只做 AP + HTTP server", "记录手机访问和串口请求日志"},
			EvidenceRequired: []string{"upload", "monitor", "manual"},
		},
		{
			Code:             "unsafe_pin_or_voltage",
			Title:            "接线风险：3.3V IO 直接接 5V 信号、启动绑带脚或输入限定脚误用",
			Platforms:        []string{"platformio", "esp_idf", "micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"},
			DetectedBy:       []string{"hardware_project_audit kind=connection warning", "hardware_project_audit kind=pin_conflict warning", "board profile riskyPins"},
			ManualSteps:      []string{"先读取 hardware_board_profile", "更新 hardware_manifest.json connections", "同步 docs/wiring.md", "真实接线前要求用户确认电压和引脚", "必要时加入电平转换或换引脚"},
			EvidenceRequired: []string{"audit", "manual"},
		},
	}
}

func failurePatternsMarkdown(platform string) string {
	rules := repairRulesForPlatform(platform)
	lines := []string{
		"# 硬件失败模式与修复规则",
		"",
		"AI 遇到编译、烧录、串口或接线问题时，先对照本文件，不要直接猜测代码。",
		"",
	}
	for _, rule := range rules {
		lines = append(lines,
			"## "+rule.Code,
			"",
			"- 标题："+rule.Title,
			"- 平台："+strings.Join(rule.Platforms, ", "),
			"- 检测信号："+strings.Join(rule.DetectedBy, "；"),
		)
		if rule.AutoRepairTool != "" {
			lines = append(lines, "- 自动修复："+rule.AutoRepairTool+" "+rule.AutoRepair)
		} else {
			lines = append(lines, "- 自动修复：暂不支持，按手动步骤处理")
		}
		lines = append(lines,
			"- 手动步骤：",
			markdownNumbered(rule.ManualSteps),
			"- 需要证据："+strings.Join(rule.EvidenceRequired, ", "),
			"",
		)
	}
	return strings.Join(lines, "\n")
}

func markdownConnectionRows(connections []hardwareConnection) string {
	rows := make([]string, 0, len(connections))
	for _, conn := range connections {
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			markdownCell(conn.Name),
			markdownCell(conn.Role),
			markdownCell(conn.Protocol),
			markdownCell(strings.Join(conn.Pins, ", ")),
			markdownCell(conn.Voltage),
			markdownCell(conn.Notes),
		))
	}
	return strings.Join(rows, "\n")
}

func markdownCell(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return strings.ReplaceAll(value, "|", "/")
}

func defaultWiringRows(platform string) []string {
	switch platform {
	case "arduino":
		return []string{
			"- Arduino UNO/Nano 第一版默认使用 D13 板载 LED，不需要外接 LED。",
			"- 外接传感器建议先用 A0 或 I2C，再扩展到电机、舵机等执行器。",
			"- 电机类负载必须使用独立电源和驱动板，不要直接接 IO 引脚。",
		}
	case "platformio", "micropython":
		return []string{
			"- ESP32 第一版默认使用 GPIO2 板载 LED，实际板卡不同可在代码常量中修改。",
			"- 板间通信建议 UART：RX=GPIO16，TX=GPIO17，GND 必须共地。",
			"- I2C 默认可从 SDA=GPIO21、SCL=GPIO22 开始，接线前核对具体开发板。",
		}
	case "esp_idf":
		return []string{
			"- ESP-IDF 第一版先验证日志输出，不默认占用外设引脚。",
			"- 增加 WiFi、BLE 或传感器前，先在 menuconfig/sdkconfig 中确认配置。",
			"- 需要烧录时先确认 target 与开发板一致，例如 esp32、esp32s3。",
		}
	case "unihiker_python":
		return []string{
			"- Unihiker 第一版先通过屏幕或终端输出验证主控逻辑。",
			"- 板间通信建议 UART 连接 ESP32/Arduino，由 Unihiker 负责 UI 和协调。",
			"- 部署前确认 SSH 地址，默认示例使用 root@10.1.2.3。",
		}
	case "maixcam_python":
		return []string{
			"- MaixCAM 第一版先验证摄像头采集和屏幕显示。",
			"- 视觉结果建议通过 UART 发给 ESP32/Arduino 执行动作。",
			"- 调试颜色、手势或目标检测前，先确认光照和摄像头画面稳定。",
		}
	case "raspberry_pi_python":
		return []string{
			"- Raspberry Pi 第一版先通过终端日志验证 Python 主循环。",
			"- 摄像头、GPIO、I2C 外设需要分别单独验证，再组合进主程序。",
			"- 部署前确认 SSH 地址，默认示例使用 pi@raspberrypi.local。",
		}
	default:
		return []string{"- 第一版先验证最小主循环，再逐步接入传感器、执行器和通信模块。"}
	}
}

func hardwareToolSequence(platform string) []string {
	switch platform {
	case "arduino":
		return []string{
			"1. `hardware_detect`：查找 Arduino CLI、板卡和串口。",
			"2. `hardware_device_verify_plan`：生成 FQBN、端口和实机验证命令。",
			"3. `arduino_compile`：先编译，确认 FQBN 正确。",
			"4. `arduino_upload`：有真实板卡后再烧录。",
			"5. `arduino_monitor_sample`：采集串口输出并保存证据。",
		}
	case "platformio":
		return []string{
			"1. `hardware_detect`：查找 PlatformIO 环境和串口。",
			"2. `hardware_device_verify_plan`：生成 environment、端口和实机验证命令。",
			"3. `platformio_run` targets=`[]`：先编译。",
			"4. `platformio_run` targets=`[\"upload\"]`：有真实板卡后再上传。",
			"5. `platformio_run` targets=`[\"monitor\"]`：采集串口输出。",
		}
	case "esp_idf":
		return []string{
			"1. `hardware_detect`：查找 ESP-IDF 和官方 MCP 状态。",
			"2. `hardware_device_verify_plan`：生成 target、端口和实机验证命令。",
			"3. `esp_idf_run` action=`build`：先完成本机编译。",
			"4. `esp_idf_run` action=`flash`：有真实 ESP32 后再烧录。",
			"5. `esp_idf_run` action=`monitor`：采集运行日志。",
		}
	case "micropython":
		return []string{
			"1. `hardware_project_validate`：先做 Python 语法检查。",
			"2. `hardware_detect`：查找串口设备。",
			"3. `hardware_device_verify_plan`：生成 mpremote device 和实机验证命令。",
			"4. `mpremote_run`：把 main.py 放到真实板卡运行。",
			"5. 读取 REPL/串口输出，确认程序持续运行。",
		}
	case "unihiker_python", "raspberry_pi_python", "maixcam_python":
		return []string{
			"1. `hardware_project_validate`：先做 Python 语法检查。",
			"2. `hardware_detect`：确认 ssh/scp 工具可用。",
			"3. `hardware_device_verify_plan`：生成 SSH host/user 和实机验证命令。",
			"4. `ssh_deploy_run`：部署到真实设备并执行。",
			"5. 保存终端、屏幕或摄像头运行证据。",
		}
	default:
		return []string{"1. `hardware_detect`。", "2. `hardware_project_validate`。", "3. 根据平台执行上传、部署和日志采集。"}
	}
}

func uploadToolForPlatform(platform string) string {
	switch platform {
	case "arduino":
		return "arduino_upload"
	case "platformio":
		return "platformio_run"
	case "esp_idf":
		return "esp_idf_run"
	case "micropython":
		return "mpremote_run"
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return "ssh_deploy_run"
	default:
		return "hardware_project_validate"
	}
}

func monitorToolForPlatform(platform string) string {
	switch platform {
	case "arduino":
		return "arduino_monitor_sample"
	case "platformio":
		return "platformio_run"
	case "esp_idf":
		return "esp_idf_run"
	case "micropython":
		return "mpremote_run"
	case "unihiker_python", "maixcam_python", "raspberry_pi_python":
		return "ssh_deploy_run"
	default:
		return "hardware_project_validate"
	}
}

func verificationCommand(platform, board, name string) string {
	switch platform {
	case "arduino":
		return "arduino-cli compile -b " + arduinoFQBN(board) + " " + name
	case "platformio":
		return "pio run -e " + platformIOEnv(board)
	case "esp_idf":
		return "idf.py set-target " + espIDFTarget(board) + " && idf.py build"
	case "micropython":
		return "mpremote run src/main.py"
	case "unihiker_python":
		return "scp -r src/ root@10.1.2.3:/root/" + name + "/"
	case "maixcam_python":
		return "scp src/main.py root@maixcam-xxxx.local:/root/"
	case "raspberry_pi_python":
		return "scp -r src/ pi@raspberrypi.local:/root/" + name + "/"
	default:
		return "mcp__hardware__hardware_project_validate"
	}
}

func labelToPlatform(label string) string {
	switch label {
	case "Unihiker":
		return "unihiker_python"
	case "Raspberry Pi":
		return "raspberry_pi_python"
	default:
		return strings.ToLower(strings.ReplaceAll(label, " ", "_"))
	}
}

func platformIOINI(name, board, env string) string {
	platform := "espressif32"
	if strings.HasPrefix(board, "nano") {
		platform = "atmelavr"
	}
	return fmt.Sprintf(`; PlatformIO Configuration
; Project: %s
; Generated by reasonix-hardware-mcp

[env:%s]
platform = %s
board = %s
framework = arduino
monitor_speed = 115200
upload_speed = 921600

build_flags =
    -D PROJECT_NAME=\"%s\"
`, name, env, platform, board, name)
}

// --- generic helpers ---

func writeTextFile(path, body string, overwrite bool) error {
	if !overwrite {
		if exists(path) {
			return fmt.Errorf("%s already exists; pass overwrite=true to replace it", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func requireInsideWorkspace(path string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absCWD, path)
	if err != nil {
		return err
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)) {
		return nil
	}
	return fmt.Errorf("refusing to write outside workspace %s: %s", absCWD, path)
}

func requirePath(args map[string]any, key string) (string, error) {
	v := strArg(args, key, "")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func strArg(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return def
		}
		return s
	}
	return def
}

func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

func boolArg(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed
		}
	}
	return def
}

func timeoutArg(args map[string]any, key string, def time.Duration) time.Duration {
	seconds := intArg(args, key, 0)
	if seconds <= 0 {
		return def
	}
	return time.Duration(seconds) * time.Second
}

func strSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func cleanProjectName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	return b.String()
}

// 以下板卡→工具链映射全部委托给共享数据驱动注册表(boards.json)。新增/改板只改
// JSON,这五个函数和桌面端孪生函数都自动跟上,不再需要手工对齐多处 switch。
func defaultBoard(platform string) string   { return boards.DefaultBoard(platform) }
func platformIOEnv(board string) string     { return boards.PlatformIOEnv(board) }
func platformIOBoardID(board string) string { return boards.PlatformIOBoard(board) }
func arduinoFQBN(board string) string       { return boards.ArduinoFQBN(board) }
func espIDFTarget(board string) string      { return boards.ESPIDFTarget(board) }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func uniqueStrings(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
