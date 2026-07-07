package main

import (
	"os"
	"path/filepath"
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
			name:     "没有 manifest 时原样直通",
			caller:   "esp32:esp32:esp32",
			wantFQBN: "esp32:esp32:esp32",
			wantNote: false,
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
