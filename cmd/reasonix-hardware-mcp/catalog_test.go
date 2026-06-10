package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogLoads(t *testing.T) {
	if catalogLoadErr != nil {
		t.Fatalf("embedded catalog failed to load: %v", catalogLoadErr)
	}
	if len(catalogModules) == 0 {
		t.Fatal("catalog is empty")
	}
	if len(platformAPIs) == 0 {
		t.Fatal("platform APIs are empty")
	}
}

func hasString(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// 核心回归:舵机库必须按板解析,这正是 P4-flash 答错的点。
func TestServoLibraryResolvesByBoard(t *testing.T) {
	esp := resolveModule("SG90", "esp32dev", "")
	if !esp.Matched {
		t.Fatal("SG90 not matched")
	}
	if !hasString(esp.Libraries, "ESP32Servo") {
		t.Errorf("esp32: expected ESP32Servo, got %v", esp.Libraries)
	}
	for _, l := range esp.Libraries {
		if l == "Servo" {
			t.Errorf("esp32: bare Servo should be dropped, got %v", esp.Libraries)
		}
	}
	if !hasString(esp.Includes, "ESP32Servo.h") {
		t.Errorf("esp32: expected <ESP32Servo.h> include, got %v", esp.Includes)
	}

	avr := resolveModule("SG90", "uno", "")
	if !hasString(avr.Libraries, "Servo") || hasString(avr.Libraries, "ESP32Servo") {
		t.Errorf("avr: expected plain Servo, got %v", avr.Libraries)
	}
}

// 别名 + I2C 地址抽取(P2 那类常见模块)
func TestOLEDAliasAndI2CAddress(t *testing.T) {
	for _, q := range []string{"SSD1306", "oled", "OLED_SSD1306"} {
		r := resolveModule(q, "esp32dev", "")
		if !r.Matched || r.Name != "OLED_SSD1306" {
			t.Fatalf("query %q did not resolve to OLED_SSD1306, got matched=%v name=%q", q, r.Matched, r.Name)
		}
		if r.I2CAddress != "0x3C" {
			t.Errorf("query %q: expected I2C 0x3C, got %q", q, r.I2CAddress)
		}
	}
}

func TestDHT11Resolves(t *testing.T) {
	r := resolveModule("DHT11", "esp32dev", "")
	if !r.Matched || !hasString(r.Libraries, "DHT sensor library") {
		t.Fatalf("DHT11 libraries wrong: matched=%v libs=%v", r.Matched, r.Libraries)
	}
	if !hasString(r.Includes, "DHT.h") {
		t.Errorf("expected <DHT.h>, got %v", r.Includes)
	}
}

// 冷门国产生态:MaixCAM 必须是 K230(不是 K210),且给出 maix.* 正确导入。
func TestMaixCAMPlatformAPI(t *testing.T) {
	r := resolveModule("MaixCAM", "", "")
	if !r.Matched || r.Kind != "platform_api" {
		t.Fatalf("MaixCAM should be a platform_api, got matched=%v kind=%q", r.Matched, r.Kind)
	}
	if !hasString(r.Imports, "from maix import") {
		t.Errorf("expected maix.* import, got %v", r.Imports)
	}
	if !hasString(r.Gotchas, "K230") {
		t.Errorf("expected a K230 gotcha, got %v", r.Gotchas)
	}
	if hasString(r.Gotchas, "fpioa") && !hasString(r.Gotchas, "不要") {
		t.Errorf("K210 老 API 应当被明确劝阻")
	}
}

// 行空板:GPIO 来自 pinpong,不是 unihiker —— P5-pro 正是错在这。
func TestUnihikerPlatformAPI(t *testing.T) {
	r := resolveModule("unihiker", "", "")
	if !r.Matched || r.Kind != "platform_api" {
		t.Fatalf("unihiker should be a platform_api, got matched=%v kind=%q", r.Matched, r.Kind)
	}
	if !hasString(r.Imports, "pinpong") {
		t.Errorf("expected pinpong import, got %v", r.Imports)
	}
	if !hasString(r.Gotchas, "pinpong") {
		t.Errorf("expected a pinpong gotcha, got %v", r.Gotchas)
	}
}

func TestUnmatchedModuleGivesSuggestion(t *testing.T) {
	r := resolveModule("完全不存在的模块xyz", "", "")
	if r.Matched {
		t.Fatal("should not match a nonsense query")
	}
	if len(r.Suggestions) == 0 {
		t.Error("expected a suggestion for an unmatched module")
	}
}

func TestRunModuleSpecEmptyListsAvailable(t *testing.T) {
	out, err := runModuleSpec(map[string]any{})
	if err != nil {
		t.Fatalf("runModuleSpec: %v", err)
	}
	var report moduleSpecReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(report.Available) == 0 {
		t.Error("empty call should list available modules")
	}
}

func TestRunModuleSpecRoundTrip(t *testing.T) {
	out, err := runModuleSpec(map[string]any{
		"modules": []any{"SG90", "OLED_SSD1306"},
		"board":   "esp32dev",
	})
	if err != nil {
		t.Fatalf("runModuleSpec: %v", err)
	}
	var report moduleSpecReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(report.Modules) != 2 {
		t.Fatalf("expected 2 module results, got %d", len(report.Modules))
	}
}

// 注入:已有项目里声明了真实模块 -> 解析出规格;占位连接被跳过(不制造噪音)
func TestModuleSpecsForConnections(t *testing.T) {
	conns := []hardwareConnection{
		{Name: "DHT11", Role: "sensor", Protocol: "OneWire"},
		{Name: "板载 LED", Role: "actuator", Protocol: "GPIO"}, // 占位,目录里没有
		{Name: "SG90", Role: "actuator", Protocol: "PWM"},
	}
	md := moduleSpecsForConnections(conns, "esp32dev")
	if !strings.Contains(md, "DHT.h") {
		t.Error("应解析出 DHT11 的头文件")
	}
	if !strings.Contains(md, "ESP32Servo") {
		t.Error("应按 esp32 板解析出 ESP32Servo")
	}
	if strings.Contains(md, "板载 LED") {
		t.Error("未匹配的占位连接不应出现在规格文档里")
	}
}

// 注入:MaixCAM 脚手架的默认连接(板名=maixcam)应自动带出 K230 平台 API
func TestMaixcamScaffoldInjectsPlatformAPI(t *testing.T) {
	conns := defaultHardwareConnections("maixcam_python", "maixcam")
	md := moduleSpecsForConnections(conns, "maixcam")
	if !strings.Contains(md, "K230") || !strings.Contains(md, "from maix import") {
		t.Errorf("MaixCAM 脚手架应自动注入 K230/maix.* 平台 API,得到:\n%s", md)
	}
}

// 注入:主流 Arduino 脚手架的占位连接不应产生 module_specs(避免噪音)
func TestMainstreamScaffoldNoNoise(t *testing.T) {
	conns := defaultHardwareConnections("arduino", "uno")
	if md := moduleSpecsForConnections(conns, "uno"); md != "" {
		t.Errorf("arduino 默认占位连接不应产生模块规格,得到:\n%s", md)
	}
}

// 编译错误蒸馏:P4-flash 实测的根因错误必须直接给出 ESP32Servo 修法
func TestDistillServoHError(t *testing.T) {
	// 这是 P4-flash 真实的编译日志摘要
	output := `In file included from /tmp/sketch.ino:2:0:
/private/tmp/dsk_test/sketches/p4_flash_b1/p4_flash_b1.ino:2:10: fatal error: Servo.h: No such file or directory
 #include <Servo.h>
          ^~~~~~~~~
compilation terminated.
Error during build: exit status 1`
	root, fix := distillBuildError("arduino", output)
	if !strings.Contains(root, "Servo.h") || !strings.Contains(root, "fatal error") {
		t.Errorf("根因应抽出 'fatal error ... Servo.h',得到 %q", root)
	}
	if !strings.Contains(fix, "ESP32Servo") {
		t.Errorf("ESP32 上 Servo.h 缺失应直接给 ESP32Servo 修法,得到 %q", fix)
	}
}

func TestDistillLibraryMissing(t *testing.T) {
	for _, tc := range []struct {
		name, output, mustContain string
	}{
		{"SSD1306", "fatal error: Adafruit_SSD1306.h: No such file or directory", "Adafruit SSD1306"},
		{"DHT", "fatal error: DHT.h: No such file or directory", "DHT sensor library"},
		{"ModuleNotFound", "ModuleNotFoundError: No module named 'pinpong'", "pip install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fix := distillBuildError("arduino", tc.output)
			if !strings.Contains(fix, tc.mustContain) {
				t.Errorf("%s 应给出 %q 修法,得到 %q", tc.name, tc.mustContain, fix)
			}
		})
	}
}

func TestDistillCompilePassNoFix(t *testing.T) {
	// 编译通过(无 error)不应误报修法
	output := "Sketch uses 123456 bytes (12%) of program storage space."
	root, fix := distillBuildError("arduino", output)
	if root != "" || fix != "" {
		t.Errorf("无错误时不应蒸馏出 root/fix,得到 root=%q fix=%q", root, fix)
	}
}

// 学生声明的模块清单 -> 解析成真 manifest 连接(不再是占位 LED)
func TestModulesToConnections(t *testing.T) {
	conns := modulesToConnections([]string{"DHT11", "SSD1306"}, "esp32dev")
	if len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conns))
	}
	byName := map[string]hardwareConnection{}
	for _, c := range conns {
		byName[c.Name] = c
	}
	if dht, ok := byName["DHT11"]; !ok || dht.Role != "sensor" {
		t.Errorf("DHT11 应当解析为 sensor 角色,得到 %+v", dht)
	}
	oled, ok := byName["OLED_SSD1306"]
	if !ok {
		t.Fatal("SSD1306 应当解析为 OLED_SSD1306")
	}
	if oled.Role != "display" || oled.Protocol != "I2C" {
		t.Errorf("OLED 应当是 display+I2C,得到 %+v", oled)
	}
	hasSDA := false
	for _, p := range oled.Pins {
		if strings.HasPrefix(p, "SDA=") {
			hasSDA = true
		}
	}
	if !hasSDA {
		t.Errorf("ESP32 板上 OLED 应当带 SDA 引脚,得到 %v", oled.Pins)
	}
}

// 模块清单为空 -> 不产生 connections(向后兼容,走默认占位)
func TestModulesToConnectionsEmpty(t *testing.T) {
	if conns := modulesToConnections(nil, "esp32dev"); len(conns) != 0 {
		t.Errorf("空模块列表应当返回 0 个连接,得到 %d", len(conns))
	}
	if conns := modulesToConnections([]string{"完全不存在"}, "esp32dev"); len(conns) != 0 {
		t.Errorf("无匹配模块应当返回 0 个连接,得到 %d", len(conns))
	}
}

// 斜杠命令文件 embed 完整,scaffold 输出里能拿到
func TestBundledHardwareCommands(t *testing.T) {
	files := bundledHardwareCommandFiles()
	want := []string{"init.md", "plan.md", "validate.md", "debug.md", "review.md"}
	for _, name := range want {
		key := ".reasonix/commands/hardware/" + name
		body, ok := files[key]
		if !ok {
			t.Errorf("missing bundled command: %s", key)
			continue
		}
		if len(body) < 100 {
			t.Errorf("%s 内容过短:%d 字节", key, len(body))
		}
	}
}

// Markdown 渲染冒烟(2b 注入要用)
func TestModuleSpecMarkdown(t *testing.T) {
	results := []moduleSpecResult{
		resolveModule("SG90", "esp32dev", ""),
		resolveModule("MaixCAM", "", ""),
	}
	md := moduleSpecMarkdown("esp32dev", results)
	if !strings.Contains(md, "ESP32Servo") {
		t.Error("markdown should contain ESP32Servo fact")
	}
	if !strings.Contains(md, "K230") {
		t.Error("markdown should contain MaixCAM K230 gotcha")
	}
}
