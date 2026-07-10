package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBoardManifest 写一个只关心 board 字段的最小 hardware_manifest.json。
// auditManifestFile 只要 JSON 合法且含 board 就会填上 Board,不要求其它必填项齐全。
func writeBoardManifest(t *testing.T, dir, board string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"board":"` + board + `"}`
	if err := os.WriteFile(filepath.Join(dir, "hardware_manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileFQBNWithManifest(t *testing.T) {
	cases := []struct {
		name          string
		manifestBoard string // "" = 不写 manifest
		manifestInSub bool   // true = manifest 在父目录,sketch 在子目录
		caller        string
		wantFQBN      string
		wantNote      bool
	}{
		{
			name:          "错 fqbn 被 manifest 校正",
			manifestBoard: "esp32s3",
			caller:        "esp32:esp32:esp32",
			wantFQBN:      "esp32:esp32:esp32s3",
			wantNote:      true,
		},
		{
			name:          "芯片核心一致时保留调用方选项",
			manifestBoard: "esp32s3",
			caller:        "esp32:esp32:esp32s3:PSRAM=opi,PartitionScheme=huge_app",
			wantFQBN:      "esp32:esp32:esp32s3:PSRAM=opi,PartitionScheme=huge_app",
			wantNote:      false,
		},
		{
			// 护栏失效必须有感知:FQBN 原样直通,但提示模型 manifest 缺失、无法校验芯片。
			name:     "没有 manifest 时直通但带失效提示",
			caller:   "esp32:esp32:esp32",
			wantFQBN: "esp32:esp32:esp32",
			wantNote: true,
		},
		{
			name:          "sketch 在子目录时向上找 manifest",
			manifestBoard: "esp32s3",
			manifestInSub: true,
			caller:        "esp32:esp32:esp32",
			wantFQBN:      "esp32:esp32:esp32s3",
			wantNote:      true,
		},
		{
			name:          "已一致的普通板卡不加噪音",
			manifestBoard: "uno",
			caller:        "arduino:avr:uno",
			wantFQBN:      "arduino:avr:uno",
			wantNote:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sketch := root
			if tc.manifestBoard != "" {
				if tc.manifestInSub {
					// manifest 在项目根,sketch 在子目录 ESP32-S3-LCD-ColorBlocks/
					writeBoardManifest(t, root, tc.manifestBoard)
					sketch = filepath.Join(root, "ESP32-S3-LCD-ColorBlocks")
					if err := os.MkdirAll(sketch, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					writeBoardManifest(t, root, tc.manifestBoard)
				}
			}

			gotFQBN, gotNote := reconcileFQBNWithManifest(sketch, tc.caller)
			if gotFQBN != tc.wantFQBN {
				t.Fatalf("fqbn = %q, want %q", gotFQBN, tc.wantFQBN)
			}
			if (gotNote != "") != tc.wantNote {
				t.Fatalf("note = %q, wantNote = %v", gotNote, tc.wantNote)
			}
		})
	}
}

// 平台一致性护栏推广:此前 FQBN 校正只挂 arduino-cli 出口,PlatformIO env 与
// ESP-IDF set-target 对 manifest.board 完全没设防。
func TestReconcilePIOEnvNote(t *testing.T) {
	root := t.TempDir()
	writeBoardManifest(t, root, "esp32s3")
	if note := reconcilePIOEnvNote(root, "esp32dev"); !strings.Contains(note, "不符") || !strings.Contains(note, "esp32s3") {
		t.Errorf("S3 项目 + esp32dev env 应给不符警告, got %q", note)
	}
	if note := reconcilePIOEnvNote(root, "esp32-s3-devkitc-1"); note != "" {
		t.Errorf("芯片家族一致不应告警, got %q", note)
	}
	if note := reconcilePIOEnvNote(root, "my_custom_env"); note != "" {
		t.Errorf("识别不出 env 芯片家族时不应误伤, got %q", note)
	}
	if note := reconcilePIOEnvNote(t.TempDir(), "esp32dev"); note != "" {
		t.Errorf("无 manifest 时 pio 路径不加噪音, got %q", note)
	}
	if note := reconcilePIOEnvNote(root, ""); note != "" {
		t.Errorf("未指定 env 时无从比对, got %q", note)
	}
}

func TestReconcileIDFTargetWithManifest(t *testing.T) {
	root := t.TempDir()
	writeBoardManifest(t, root, "esp32s3")
	target, note := reconcileIDFTargetWithManifest(root, "esp32")
	if target != "esp32s3" || note == "" {
		t.Errorf("S3 项目 set-target esp32 应被修正为 esp32s3, got target=%q note=%q", target, note)
	}
	target, note = reconcileIDFTargetWithManifest(root, "esp32s3")
	if target != "esp32s3" || note != "" {
		t.Errorf("已一致不应加噪音, got target=%q note=%q", target, note)
	}
	// 没有 IDF 映射的板(行空板)不乱改。
	root2 := t.TempDir()
	writeBoardManifest(t, root2, "unihiker")
	if target, note := reconcileIDFTargetWithManifest(root2, "esp32"); target != "esp32" || note != "" {
		t.Errorf("无 IDF 映射的板不应改 target, got target=%q note=%q", target, note)
	}
	// 无 manifest 原样直通。
	if target, note := reconcileIDFTargetWithManifest(t.TempDir(), "esp32"); target != "esp32" || note != "" {
		t.Errorf("无 manifest 应直通, got target=%q note=%q", target, note)
	}
}

func TestChipFamily(t *testing.T) {
	cases := map[string]string{
		"esp32s3":            "esp32s3",
		"esp32-s3-devkitc-1": "esp32s3",
		"esp32dev":           "esp32",
		"nodemcuv2":          "esp8266",
		"pico":               "rp2040",
		"bluepill_f103c8":    "stm32",
		"uno":                "avr",
		"my_custom_env":      "",
	}
	for in, want := range cases {
		if got := chipFamily(in); got != want {
			t.Errorf("chipFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFQBNChip(t *testing.T) {
	cases := map[string]string{
		"esp32:esp32:esp32":             "esp32",
		"esp32:esp32:esp32s3":           "esp32s3",
		"esp32:esp32:esp32s3:PSRAM=opi": "esp32s3",
		"arduino:avr:uno":               "uno",
		"uno":                           "uno",
	}
	for in, want := range cases {
		if got := fqbnChip(in); got != want {
			t.Fatalf("fqbnChip(%q) = %q, want %q", in, got, want)
		}
	}
}
