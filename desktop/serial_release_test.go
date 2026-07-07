package main

import (
	"encoding/json"
	"testing"
)

// shouldReleaseSerial decides whether a tool call is about to grab the local USB
// serial port, so the resident serial monitor must let go first. Table-driven so
// the hit list (and its arg-parsing rules) is pinned.
func TestShouldReleaseSerial(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want bool
	}{
		// Unconditional serial-occupiers.
		{"arduino upload", "arduino_upload", `{}`, true},
		{"arduino monitor sample", "arduino_monitor_sample", `{}`, true},
		{"mpremote run", "mpremote_run", `{}`, true},

		// MCP prefix must be stripped before matching.
		{"prefixed arduino upload", "mcp__reasonix-hardware__arduino_upload", `{}`, true},

		// platformio_run: only upload / monitor targets touch the port.
		{"pio upload", "platformio_run", `{"targets":["upload"]}`, true},
		{"pio monitor", "platformio_run", `{"targets":["monitor"]}`, true},
		{"pio upload+monitor", "platformio_run", `{"targets":["upload","monitor"]}`, true},
		{"pio build only", "platformio_run", `{"targets":["build"]}`, false},
		{"pio no targets", "platformio_run", `{}`, false},

		// esp_idf_run: only flash / monitor / flash_monitor actions touch the port.
		{"idf flash", "esp_idf_run", `{"action":"flash"}`, true},
		{"idf monitor", "esp_idf_run", `{"action":"monitor"}`, true},
		{"idf flash_monitor", "esp_idf_run", `{"action":"flash_monitor"}`, true},
		{"idf build", "esp_idf_run", `{"action":"build"}`, false},
		{"idf set_target", "esp_idf_run", `{"action":"set_target"}`, false},

		// Never local-serial: OTA (WiFi), remote publish, SSH, compile.
		{"arduino ota", "arduino_ota_upload", `{}`, false},
		{"firmware publish", "firmware_publish", `{}`, false},
		{"ssh deploy", "ssh_deploy_run", `{}`, false},
		{"arduino compile", "arduino_compile", `{}`, false},

		// Unrelated tools stay untouched (hot path returns fast, no lock).
		{"read file", "read_file", `{}`, false},
		{"bash", "bash", `{}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldReleaseSerial(c.tool, json.RawMessage(c.args)); got != c.want {
				t.Errorf("shouldReleaseSerial(%q, %s) = %v, want %v", c.tool, c.args, got, c.want)
			}
		})
	}
}
