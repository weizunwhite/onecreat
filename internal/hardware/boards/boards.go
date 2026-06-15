// Package boards is the single, data-driven source of truth for hardware board
// facts shared by the hardware MCP server (cmd/reasonix-hardware-mcp) and the
// desktop app. Every board — its profile (pins/risk/voltage/teaching), its
// toolchain mappings (Arduino FQBN, PlatformIO env/board, ESP-IDF target) and
// its sensor-catalog pin key — lives in one embedded boards.json. Adding a board
// is a JSON edit, not seven scattered Go switch edits, and the MCP↔desktop FQBN
// drift (which used to need hand-aligned twin functions) is eliminated because
// both call the same lookups here.
package boards

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed boards.json
var raw []byte

// PinRule is one pin/protocol fact for a board (mirror of the MCP's boardPinRule).
type PinRule struct {
	Name      string   `json:"name"`
	Pins      []string `json:"pins"`
	Protocol  string   `json:"protocol"`
	Direction string   `json:"direction,omitempty"`
	Voltage   string   `json:"voltage"`
	Notes     string   `json:"notes"`
}

// Board is one board's full profile + toolchain mappings (mirror of the MCP's
// boardProfile). Field names/json tags MUST match so the MCP can alias it.
type Board struct {
	ID                   string    `json:"id"`
	Label                string    `json:"label"`
	Platform             string    `json:"platform"`
	Board                string    `json:"board"`
	DefaultFramework     string    `json:"defaultFramework"`
	ArduinoFQBN          string    `json:"arduinoFqbn,omitempty"`
	PlatformIOEnv        string    `json:"platformioEnv,omitempty"`
	PlatformIOBoard      string    `json:"platformioBoard,omitempty"`
	ESPIDFTarget         string    `json:"espIdfTarget,omitempty"`
	CatalogPinKey        string    `json:"catalogPinKey,omitempty"`
	UploadMethod         string    `json:"uploadMethod"`
	DefaultBaud          int       `json:"defaultBaud"`
	LogicVoltage         string    `json:"logicVoltage"`
	PowerNotes           string    `json:"powerNotes"`
	RecommendedProtocols []string  `json:"recommendedProtocols"`
	DefaultPins          []PinRule `json:"defaultPins"`
	RiskyPins            []PinRule `json:"riskyPins"`
	Toolchains           []string  `json:"toolchains"`
	ValidationFlow       []string  `json:"validationFlow"`
	CommonFailures       []string  `json:"commonFailures"`
	TeachingNotes        []string  `json:"teachingNotes"`
	Aliases              []string  `json:"aliases,omitempty"`
}

var registry []Board

func init() {
	if err := json.Unmarshal(raw, &registry); err != nil {
		// 数据是编译期 embed 的;解析失败说明 boards.json 写坏了,必须立刻暴露。
		panic(fmt.Sprintf("boards: parse embedded boards.json: %v", err))
	}
}

// Profiles returns a copy of every board profile (data-driven; was the hardcoded
// builtInBoardProfiles slice).
func Profiles() []Board {
	return append([]Board(nil), registry...)
}

// Normalize lower-cases and strips separators so "ESP32-S3" / "esp32_s3" /
// "esp32 s3" all compare equal.
func Normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer(" ", "", "_", "", "-", "", "/", "", ":", "").Replace(value)
}

// Matches reports whether the board key matches a profile's id/board/platform/
// any toolchain id/alias.
func Matches(b Board, key string) bool {
	if key == "" {
		return false
	}
	candidates := []string{b.ID, b.Label, b.Board, b.Platform, b.ArduinoFQBN, b.PlatformIOEnv, b.PlatformIOBoard, b.ESPIDFTarget}
	candidates = append(candidates, b.Aliases...)
	for _, c := range candidates {
		if Normalize(c) == key {
			return true
		}
	}
	return false
}

// find locates a board by id/alias (normalized). ok=false when nothing matches.
func find(board string) (Board, bool) {
	key := Normalize(board)
	if key == "" {
		return Board{}, false
	}
	for _, b := range registry {
		if Matches(b, key) {
			return b, true
		}
	}
	return Board{}, false
}

// platformDefaults maps a platform to its canonical default board id. Platform-
// level (not per-board) config, so it stays a small in-code map.
var platformDefaults = map[string]string{
	"arduino":             "uno",
	"platformio":          "esp32dev",
	"esp_idf":             "esp32",
	"micropython":         "esp32",
	"unihiker_python":     "unihiker",
	"maixcam_python":      "maixcam",
	"raspberry_pi_python": "raspberry_pi",
}

// DefaultBoard returns the canonical board for a platform ("unknown" if none).
func DefaultBoard(platform string) string {
	if b, ok := platformDefaults[platform]; ok {
		return b
	}
	return "unknown"
}

// ArduinoFQBN maps a board id/alias to an arduino-cli FQBN. Registry first; an
// empty board defaults to UNO; an unknown value passes through unchanged (a
// detected real board already carries a full FQBN like esp32:esp32:esp32).
func ArduinoFQBN(board string) string {
	if strings.TrimSpace(board) == "" {
		if b, ok := find("uno"); ok && b.ArduinoFQBN != "" {
			return b.ArduinoFQBN
		}
		return "arduino:avr:uno"
	}
	if b, ok := find(board); ok && b.ArduinoFQBN != "" {
		return b.ArduinoFQBN
	}
	return board
}

// PlatformIOEnv maps a board to its platformio.ini [env:] name (default esp32dev).
func PlatformIOEnv(board string) string {
	if b, ok := find(board); ok && b.PlatformIOEnv != "" {
		return b.PlatformIOEnv
	}
	return "esp32dev"
}

// PlatformIOBoard maps a board to its PlatformIO board id. Registry first; empty
// or esp32 → esp32dev; unknown passes through.
func PlatformIOBoard(board string) string {
	if b, ok := find(board); ok && b.PlatformIOBoard != "" {
		return b.PlatformIOBoard
	}
	switch Normalize(board) {
	case "", "esp32", "esp32dev":
		return "esp32dev"
	default:
		return board
	}
}

// ESPIDFTarget maps a board to its idf.py target (default esp32).
func ESPIDFTarget(board string) string {
	if b, ok := find(board); ok && b.ESPIDFTarget != "" {
		return b.ESPIDFTarget
	}
	return "esp32"
}

// CatalogPinKey maps a board to the sensor-catalog default_pins board key (which
// per-board pin defaults a module reuses). A board may declare catalogPinKey
// explicitly; otherwise a substring fallback preserves the old behavior. Key
// difference from the old hardcoded version: ESP32-C3/C6/H2/S2/C2 resolve to ""
// (no module pins injected) instead of the classic-ESP32 devkit pins, because
// their pinouts differ — injecting devkit pins as their facts misleads the model.
// "" means inject no module pins (the board profile still carries its own facts).
func CatalogPinKey(board string) string {
	if b, ok := find(board); ok && b.CatalogPinKey != "" {
		return b.CatalogPinKey // 显式声明优先,可覆盖下面的子串猜测
	}
	n := Normalize(board)
	switch {
	case n == "":
		return ""
	case strings.Contains(n, "esp32s3") || strings.Contains(n, "s3"):
		return "esp32_s3"
	case strings.Contains(n, "c3"), strings.Contains(n, "c6"), strings.Contains(n, "h2"),
		strings.Contains(n, "s2"), strings.Contains(n, "c2"):
		return "" // ESP32-C3/C6/H2/S2/C2 引脚布局不同于经典 ESP32,宁缺勿错
	case strings.Contains(n, "esp32"):
		return "esp32_devkit"
	case strings.Contains(n, "nano"), strings.Contains(n, "uno"), strings.Contains(n, "avr"):
		return "arduino_nano"
	default:
		return ""
	}
}
