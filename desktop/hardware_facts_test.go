package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用 hardware_board_profile 真实输出形状的 JSON 反序列化进 mirror，再渲染，
// 验证「解析 + 紧凑渲染」整条链：关键字段（板卡、FQBN、电平、风险引脚、常见失败）都要出现。
func TestRenderBoardProfileFactsFromJSON(t *testing.T) {
	js := `{"profile":{"label":"ESP32 Dev Module","arduinoFqbn":"esp32:esp32:esp32",` +
		`"logicVoltage":"3.3V","powerNotes":"5V 模块输出接输入脚前必须电平转换",` +
		`"recommendedProtocols":["UART","I2C"],` +
		`"defaultPins":[{"name":"I2C","pins":["SDA=21","SCL=22"],"notes":"默认硬件 I2C"}],` +
		`"riskyPins":[{"name":"strapping","pins":["GPIO0","GPIO2","GPIO15"],"notes":"启动时别接强上/下拉"}],` +
		`"commonFailures":["上传时需要按 BOOT","5V 模块直接接 3.3V IO"],` +
		`"teachingNotes":["先验证 Serial 日志再接外设"]}}`
	var bp boardProfileMirror
	if err := json.Unmarshal([]byte(js), &bp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := renderBoardProfileFacts(bp)
	for _, want := range []string{"ESP32 Dev Module", "esp32:esp32:esp32", "3.3V", "SDA=21", "GPIO0", "上传时需要按 BOOT", "先验证 Serial"} {
		if !strings.Contains(got, want) {
			t.Fatalf("板卡事实缺少 %q，实际：\n%s", want, got)
		}
	}
}

// platform_api 才是 flash 最易幻觉处：正确 import、坑、最小示例都要渲染出来。
func TestRenderPlatformAPIFactsFromJSON(t *testing.T) {
	js := `{"modules":[{"matched":true,"kind":"platform_api","function":"ESP32 Arduino",` +
		`"imports":["#include <ESP32Servo.h>"],"gotchas":["PWM 用 LEDC，不要 analogWrite"],` +
		`"snippet":"ledcAttach(pin, freq, resolution);"}]}`
	var ms moduleSpecMirror
	if err := json.Unmarshal([]byte(js), &ms); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := renderPlatformAPIFacts(ms)
	for _, want := range []string{"ESP32 Arduino", "LEDC", "ledcAttach", "ESP32Servo"} {
		if !strings.Contains(got, want) {
			t.Fatalf("平台 API 事实缺少 %q，实际：\n%s", want, got)
		}
	}

	// 普通元器件模块（kind=module，非 platform_api）不应被当平台 API 渲染。
	var ms2 moduleSpecMirror
	if err := json.Unmarshal([]byte(`{"modules":[{"matched":true,"kind":"module","function":"DHT11"}]}`), &ms2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := renderPlatformAPIFacts(ms2); got != "" {
		t.Fatalf("非 platform_api 应渲染为空，实际：%q", got)
	}
}

// 证据导出：读 tests/hardware_evidence.jsonl，汇总成可粘进竞赛材料的 Markdown。
// 空目录返回空串(不报错)；有记录时关键字段(阶段中文名/状态/命令/输出)都要出现。
func TestHardwareEvidenceExport(t *testing.T) {
	app := &App{}

	// 没有证据文件 → 空串、无错
	empty := t.TempDir()
	if md, err := app.HardwareEvidenceExport(empty); err != nil || md != "" {
		t.Fatalf("无证据文件应返回空串无错，得到 md=%q err=%v", md, err)
	}

	// 写两条证据记录再导出
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"timestampUtc":"2026-06-06T04:30:00Z","platform":"arduino","board":"arduino_uno","stage":"compile","status":"passed","summary":"1 passed","command":"arduino-cli compile -b arduino:avr:uno"}
{"timestampUtc":"2026-06-06T04:35:00Z","platform":"arduino","board":"arduino_uno","stage":"monitor","status":"passed","summary":"serial ok","outputExcerpt":"distance=18cm\nbuzzer ON"}
`
	if err := os.WriteFile(filepath.Join(dir, "tests", "hardware_evidence.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	md, err := app.HardwareEvidenceExport(dir)
	if err != nil {
		t.Fatalf("export error: %v", err)
	}
	for _, want := range []string{"共 2 条", "编译/语法", "串口/运行日志", "arduino-cli compile", "distance=18cm"} {
		if !strings.Contains(md, want) {
			t.Fatalf("导出 Markdown 缺少 %q：\n%s", want, md)
		}
	}
}

// 串口输出里若含 ``` 不能提前闭合代码块：动态围栏要比内容里最长反引号串更长，
// 且原样保留内容。
func TestEvidenceOutputWithBackticksUsesLongerFence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	// outputExcerpt 含三个反引号
	line := "{\"timestampUtc\":\"2026-06-06T05:00:00Z\",\"platform\":\"arduino\",\"stage\":\"monitor\",\"status\":\"passed\",\"outputExcerpt\":\"log start ``` log end\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "tests", "hardware_evidence.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	md, err := (&App{}).HardwareEvidenceExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "````") {
		t.Fatalf("含 ``` 的输出应使用更长围栏(>=4)，实际：\n%s", md)
	}
	if !strings.Contains(md, "log start ``` log end") {
		t.Fatalf("围栏内必须原样保留输出内容，实际：\n%s", md)
	}
}

// 端到端：用真实 hardware MCP 二进制跑 esp32_arduino，验证整条链
// （board_profile + module_spec → 解析 → 渲染）真能压出 LEDC / 3.3V 这些
// flash 最易写错的事实。二进制没构建时跳过，不阻塞 CI。
func TestHardwareBoardFactsESP32EndToEnd(t *testing.T) {
	bin, err := filepath.Abs(filepath.Join("..", "bin", "reasonix-hardware-mcp"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("real hardware MCP binary not built (../bin/reasonix-hardware-mcp); skipping end-to-end")
	}
	t.Setenv("REASONIX_HARDWARE_MCP", bin)

	v := NewApp().HardwareBoardFacts("esp32_arduino", "auto")
	if !v.Found {
		t.Fatal("expected facts for esp32_arduino, got Found=false")
	}
	for _, want := range []string{"3.3V", "LEDC"} {
		if !strings.Contains(v.Facts, want) {
			t.Fatalf("facts 缺少 %q，实际：\n%s", want, v.Facts)
		}
	}
}
