package boards

import "testing"

// 锁住数据驱动注册表的关键不变量:新板的工具链映射正确、旧板行为不退化、
// ESP32 变体不再被注入经典 ESP32 的错引脚键。
func TestArduinoFQBN(t *testing.T) {
	cases := map[string]string{
		"":          "arduino:avr:uno", // 空默认 UNO(旧行为)
		"uno":       "arduino:avr:uno",
		"nano":      "arduino:avr:nano",
		"mega":      "arduino:avr:mega",
		"esp32":     "esp32:esp32:esp32",
		"esp32s3":   "esp32:esp32:esp32s3",
		"esp32c3":   "esp32:esp32:esp32c3",
		"esp32c6":   "esp32:esp32:esp32c6",
		"esp8266":   "esp8266:esp8266:nodemcuv2",
		"pico":      "rp2040:rp2040:rpipico",
		"microbit":  "microbit", // 无 arduino FQBN → 透传(走 MakeCode/PlatformIO)
		"stm32":     "stm32",    // 无 arduino FQBN → 透传(走 PlatformIO)
		"unhandled": "unhandled",
	}
	for in, want := range cases {
		if got := ArduinoFQBN(in); got != want {
			t.Errorf("ArduinoFQBN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestESPIDFTarget(t *testing.T) {
	for in, want := range map[string]string{
		"esp32": "esp32", "esp32s3": "esp32s3", "esp32c3": "esp32c3", "esp32c6": "esp32c6",
		"unknown": "esp32", // 兜底
	} {
		if got := ESPIDFTarget(in); got != want {
			t.Errorf("ESPIDFTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlatformIOBoard(t *testing.T) {
	for in, want := range map[string]string{
		"esp32": "esp32dev", "": "esp32dev",
		"esp32s3": "esp32-s3-devkitc-1", "pico": "pico", "stm32": "bluepill_f103c8",
		"nano": "nanoatmega328",
	} {
		if got := PlatformIOBoard(in); got != want {
			t.Errorf("PlatformIOBoard(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogPinKeyPreservesAndImproves(t *testing.T) {
	for in, want := range map[string]string{
		"uno": "arduino_nano", "nano": "arduino_nano", // 旧行为:复用 nano
		"esp32": "esp32_devkit", "esp32dev": "esp32_devkit",
		"esp32s3": "esp32_s3",
		"esp32c3": "", "esp32c6": "", // 改进:C3/C6 不再注入经典 ESP32 错引脚
		"esp8266": "", "pico": "", "stm32": "", "microbit": "", "mega": "",
		"": "",
	} {
		if got := CatalogPinKey(in); got != want {
			t.Errorf("CatalogPinKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProfilesCoverNewFamilies(t *testing.T) {
	want := []string{"esp8266", "rp2040_pico", "stm32_bluepill", "microbit_v2", "esp32_s3", "esp32_c3", "esp32_c6", "arduino_mega"}
	have := map[string]bool{}
	for _, b := range Profiles() {
		have[b.ID] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Errorf("registry missing board %q", id)
		}
	}
	if n := len(Profiles()); n < 15 {
		t.Errorf("expected >=15 boards, got %d", n)
	}
}
