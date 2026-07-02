package main

// 「新建 OTA 项目」脚手架(A 方案):生成一个已含 OTA 底座的项目骨架,学生在
// "你的代码写这里"那段填业务逻辑。不去自动改学生现有代码(脆),而是给干净起点。
// 三种底座对应三种远程烧录:lan=局域网直推 / web=浏览器拖拽 / cloud=NAS 云端拉取。

import (
	"os"
	"path/filepath"
	"strings"
)

// OTAScaffoldInput 是「新建 OTA 项目」的入参。destDir 留空时落到 ~/onecreat-projects。
type OTAScaffoldInput struct {
	DestDir      string `json:"destDir,omitempty"`
	ProjectName  string `json:"projectName"`
	Mode         string `json:"mode"` // lan | web | cloud
	WifiSSID     string `json:"wifiSSID"`
	WifiPassword string `json:"wifiPassword"`
	NasBaseURL   string `json:"nasBaseURL,omitempty"` // cloud 模式:固件服务器基址,默认 NAS
}

// OTAScaffoldResult 返回生成的 .ino 路径。
type OTAScaffoldResult struct {
	OK    bool   `json:"ok"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

// HardwareScaffoldOTA 生成一个含 OTA 底座的项目目录:<dest>/<name>/<name>.ino。
func (a *App) HardwareScaffoldOTA(input OTAScaffoldInput) OTAScaffoldResult {
	name := sanitizeProjectName(input.ProjectName)
	if name == "" {
		return OTAScaffoldResult{Error: "请填项目名(英文字母/数字/下划线)"}
	}
	tmpl, ok := otaTemplates[input.Mode]
	if !ok {
		return OTAScaffoldResult{Error: "未知模式:" + input.Mode + "(应为 lan/web/cloud)"}
	}

	dest := strings.TrimSpace(input.DestDir)
	if dest == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return OTAScaffoldResult{Error: "无法定位用户目录:" + err.Error()}
		}
		dest = filepath.Join(home, "onecreat-projects")
	}
	projectDir := filepath.Join(dest, name)
	if _, err := os.Stat(projectDir); err == nil {
		return OTAScaffoldResult{Error: "目录已存在,换个项目名:" + projectDir}
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return OTAScaffoldResult{Error: "建目录失败:" + err.Error()}
	}

	baseURL := strings.TrimRight(firstNonEmptyStr(input.NasBaseURL, "http://你的固件服务器:9000"), "/")
	code := tmpl
	code = strings.ReplaceAll(code, "%WIFI_SSID%", escapeForC(input.WifiSSID))
	code = strings.ReplaceAll(code, "%WIFI_PASSWORD%", escapeForC(input.WifiPassword))
	code = strings.ReplaceAll(code, "%FW_VERSION_URL%", baseURL+"/"+name+"/version.txt")
	code = strings.ReplaceAll(code, "%FW_BIN_URL%", baseURL+"/"+name+"/firmware.bin")

	inoPath := filepath.Join(projectDir, name+".ino")
	if err := os.WriteFile(inoPath, []byte(code), 0o644); err != nil {
		return OTAScaffoldResult{Error: "写文件失败:" + err.Error()}
	}
	return OTAScaffoldResult{OK: true, Path: projectDir}
}

// sanitizeProjectName 只留字母数字下划线连字符,首字符非字母则前缀 p_(Arduino 工程名要求)。
func sanitizeProjectName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if c := out[0]; !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		out = "p_" + out
	}
	return out
}

// escapeForC 把字符串安全嵌进 C 字符串字面量(转义反斜杠和双引号)。
func escapeForC(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// 三种底座模板(占位符在写入时替换)。和 docs/examples/ota 一致,精简版。
var otaTemplates = map[string]string{
	"lan": `// OTA 底座 ①:局域网直推(ArduinoOTA)。第一次 USB 烧,之后用 onecreat 的「WiFi 烧录」推。
#include <WiFi.h>
#include <ArduinoOTA.h>

const char* WIFI_SSID     = "%WIFI_SSID%";
const char* WIFI_PASSWORD = "%WIFI_PASSWORD%";
const char* OTA_HOSTNAME  = "esp32-onecreat";
const char* OTA_PASSWORD  = "oneup1234";   // onecreat WiFi 烧录时填同一个口令

void setup() {
  Serial.begin(115200);
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  while (WiFi.status() != WL_CONNECTED) { delay(500); Serial.print("."); }
  Serial.printf("\nWiFi OK  IP=%s\n", WiFi.localIP().toString().c_str());
  ArduinoOTA.setHostname(OTA_HOSTNAME);
  ArduinoOTA.setPassword(OTA_PASSWORD);
  ArduinoOTA.begin();
  Serial.println("OTA ready");
}

void loop() {
  ArduinoOTA.handle();   // 别删:一直处理 OTA 请求

  // ↓↓↓ 你的代码写这里 ↓↓↓
}
`,
	"web": `// OTA 底座 ②:浏览器拖拽(零依赖)。第一次 USB 烧,之后浏览器开 http://板子IP/ 传 .bin。
#include <WiFi.h>
#include <WebServer.h>
#include <Update.h>

const char* WIFI_SSID     = "%WIFI_SSID%";
const char* WIFI_PASSWORD = "%WIFI_PASSWORD%";

WebServer server(80);
const char* PAGE = "<!DOCTYPE html><meta charset=utf-8><h2>onecreat 固件升级</h2>"
  "<form method=POST action=/update enctype=multipart/form-data>"
  "<input type=file name=firmware accept=.bin><input type=submit value=上传并升级></form>";

void handleUpload() {
  HTTPUpload& up = server.upload();
  if (up.status == UPLOAD_FILE_START) Update.begin(UPDATE_SIZE_UNKNOWN);
  else if (up.status == UPLOAD_FILE_WRITE) Update.write(up.buf, up.currentSize);
  else if (up.status == UPLOAD_FILE_END) Update.end(true);
}

void setup() {
  Serial.begin(115200);
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  while (WiFi.status() != WL_CONNECTED) { delay(500); Serial.print("."); }
  Serial.printf("\nweb OTA: http://%s/\n", WiFi.localIP().toString().c_str());
  server.on("/", [](){ server.send(200, "text/html; charset=utf-8", PAGE); });
  server.on("/update", HTTP_POST, [](){ server.send(200,"text/plain","ok, rebooting"); delay(400); ESP.restart(); }, handleUpload);
  server.begin();
}

void loop() {
  server.handleClient();   // 别删

  // ↓↓↓ 你的代码写这里 ↓↓↓
}
`,
	"cloud": `// OTA 底座 ③:云端拉取(真·远程)。第一次 USB 烧,之后在 onecreat 点「发布固件」,板子自己拉。
#include <WiFi.h>
#include <HTTPClient.h>
#include <HTTPUpdate.h>
#include <Preferences.h>

const char* WIFI_SSID       = "%WIFI_SSID%";
const char* WIFI_PASSWORD   = "%WIFI_PASSWORD%";
const char* FW_VERSION_URL  = "%FW_VERSION_URL%";
const char* FW_BIN_URL      = "%FW_BIN_URL%";
const char* FACTORY_VERSION = "0.0.0";   // 出厂基线:板子第一次开机用它。故意设成 0.0.0(比任何真发布版本都低),这样第一次"发布固件"(默认 1.0.0)一定不同→板子会拉取升级;若这里等于发布默认版,首发就成静默 no-op

const unsigned long CHECK_INTERVAL_MS = 30000;
unsigned long lastCheck = 0;
Preferences prefs;   // 把"已刷版本"记在板子自己的 NVS,避免刷完又看自己版本不对、无限重刷

// 读板子记下的当前版本(从没刷过就用出厂基线)
String currentVersion() {
  return prefs.getString("ver", FACTORY_VERSION);
}

void checkForUpdate() {
  if (WiFi.status() != WL_CONNECTED) return;
  WiFiClient client; HTTPClient http;
  http.begin(client, FW_VERSION_URL);
  if (http.GET() != HTTP_CODE_OK) { http.end(); return; }
  String latest = http.getString(); latest.trim(); http.end();

  String now = currentVersion();
  if (latest == now) return;                 // 已是最新,不刷

  prefs.putString("ver", latest);            // 先记下目标版本(刷成功会立刻重启,来不及事后再记)
  t_httpUpdate_return ret = httpUpdate.update(client, FW_BIN_URL);
  if (ret == HTTP_UPDATE_FAILED) {
    prefs.putString("ver", now);             // 刷失败:回滚版本记录,下个周期重试
  }
  // 刷成功则已重启进新固件;新固件读到"已刷版本 == 服务器版本",不会再循环
}

void setup() {
  Serial.begin(115200);
  prefs.begin("ota", false);   // 打开 NVS 命名空间 ota(读写)
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  while (WiFi.status() != WL_CONNECTED) { delay(500); Serial.print("."); }
  Serial.printf("\nWiFi OK  v=%s\n", currentVersion().c_str());
  checkForUpdate();
}

void loop() {
  if (millis() - lastCheck > CHECK_INTERVAL_MS) { lastCheck = millis(); checkForUpdate(); }

  // ↓↓↓ 你的代码写这里 ↓↓↓
}
`,
}
