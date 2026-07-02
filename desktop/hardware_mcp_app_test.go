package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestResolveHardwareMCPFromEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "reasonix-hardware-mcp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONIX_HARDWARE_MCP", bin)

	got, source, err := resolveHardwareMCP()
	if err != nil {
		t.Fatalf("resolveHardwareMCP() error = %v", err)
	}
	if got != bin {
		t.Fatalf("command = %q, want %q", got, bin)
	}
	if source != "REASONIX_HARDWARE_MCP" {
		t.Fatalf("source = %q, want REASONIX_HARDWARE_MCP", source)
	}
}

func TestResolveHardwareMCPRejectsBadEnv(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-hardware-mcp")
	t.Setenv("REASONIX_HARDWARE_MCP", missing)

	if _, _, err := resolveHardwareMCP(); err == nil {
		t.Fatal("resolveHardwareMCP() should reject a missing env override")
	}
}

func TestHardwareDetectUsesResolvedMCP(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "reasonix-hardware-mcp"+exeSuffix())
	response := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"workspace\":\"/tmp/ws\",\"projectDir\":\"/tmp/ws\",\"projectTypes\":[\"platformio\"],\"serialPorts\":[\"/dev/cu.fake\"],\"boards\":null,\"devices\":null,\"toolchains\":[{\"name\":\"PlatformIO\",\"command\":\"pio --version\",\"available\":true,\"version\":\"PlatformIO Core\"}],\"recommendations\":null,\"espIdfOfficialMcp\":{}}"}],"isError":false}}`
	source := fmt.Sprintf(`package main

import (
	"bufio"
	"fmt"
	"os"
)

func main() {
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println(%s)
}
`, strconv.Quote(response))
	src := filepath.Join(dir, "fake_mcp.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake MCP: %v\n%s", err, out)
	}
	t.Setenv("REASONIX_HARDWARE_MCP", bin)

	got := NewApp().HardwareDetect()
	if !got.Available {
		t.Fatalf("HardwareDetect() unavailable: %s", got.Error)
	}
	if len(got.Toolchains) != 1 || !got.Toolchains[0].Available {
		t.Fatalf("toolchains not parsed: %+v", got.Toolchains)
	}
	if len(got.SerialPorts) != 1 || got.SerialPorts[0] != "/dev/cu.fake" {
		t.Fatalf("serial ports = %+v", got.SerialPorts)
	}
	if got.Recommendations == nil {
		t.Fatal("recommendations should be normalized to an empty slice")
	}
	if got.Boards == nil || got.Devices == nil {
		t.Fatalf("boards/devices should be normalized: boards=%+v devices=%+v", got.Boards, got.Devices)
	}
}

// 前端 boardPresets 里走 Arduino 上传的每个 value，都必须映射成合法 FQBN。
// 这一层(upload 用)与 hardware MCP 的 arduinoFQBN(validate 用)必须同覆盖，
// 否则会出现「编译能过、烧录却拿到非法 FQBN」的撕裂。
func TestArduinoFQBNFromBoardCoversFrontendPresets(t *testing.T) {
	want := map[string]string{
		"arduino_uno":   "arduino:avr:uno",
		"arduino_nano":  "arduino:avr:nano",
		"uno":           "arduino:avr:uno",
		"nano":          "arduino:avr:nano",
		"esp32_arduino": "esp32:esp32:esp32",
	}
	for board, fqbn := range want {
		if got := arduinoFQBNFromBoard(board); got != fqbn {
			t.Errorf("arduinoFQBNFromBoard(%q) = %q, want %q", board, got, fqbn)
		}
	}
}

// TestResolveFlashFQBNPrefersManifestBoard 钉死:烧录/发布解析 FQBN 时项目 manifest.board
// 必须优先于 UI 下拉板型,与「编译」(validateArduinoProject)同一优先级——否则同一项目
// 编译按 manifest、烧录按 UI,会烧错芯片的固件。
func TestResolveFlashFQBNPrefersManifestBoard(t *testing.T) {
	dir := t.TempDir()
	// 无 manifest:回退到 UI 板型。
	if got := resolveFlashFQBN(dir, "esp32_arduino"); got != "esp32:esp32:esp32" {
		t.Fatalf("no manifest: got %q, want esp32:esp32:esp32", got)
	}
	// 有 manifest.board:必须优先于 UI 板型。
	if err := os.WriteFile(filepath.Join(dir, "hardware_manifest.json"), []byte(`{"board":"uno"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveFlashFQBN(dir, "esp32_arduino"); got != "arduino:avr:uno" {
		t.Fatalf("manifest board must win over UI board: got %q, want arduino:avr:uno", got)
	}
	// 两者都空:返回空,让调用方保持各自兜底。
	if got := resolveFlashFQBN(t.TempDir(), ""); got != "" {
		t.Fatalf("empty board: got %q, want empty", got)
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
