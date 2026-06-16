// ────────────────────────────────────────────────────────────
// OTA 底座 ②：浏览器拖拽（ElegantOTA）
//
// 作用：板子开一个网页 http://板子IP/update，手机/电脑浏览器打开，把编译好的
//       .bin 文件拖进去就刷新固件。学生自助，不用装任何工具。
//
// 关键前提（同①）：第一次 USB 烧底座；之后每个新程序都要带上这段 Web 服务代码。
//
// 依赖库（在 Arduino IDE 库管理器或 arduino-cli 装一次）：
//   arduino-cli lib install ElegantOTA
// 说明：本模板用 ESP32 内置的 WebServer（同步版），不需要额外的 Async 库。
//       如果 ElegantOTA 默认走 Async，需要在库配置里把
//       ELEGANTOTA_USE_ASYNC_WEBSERVER 设为 0（onecreat 集成时我会处理好）。
//
// 适用：学生自助刷机、临时给某块板子换程序。
// ────────────────────────────────────────────────────────────

#include <WiFi.h>
#include <WebServer.h>
#include <ElegantOTA.h>

// ↓↓↓ 改成你自己的 WiFi ↓↓↓
const char* WIFI_SSID     = "你的WiFi名称";
const char* WIFI_PASSWORD = "你的WiFi密码";

WebServer server(80);  // 网页服务跑在 80 端口

void setup() {
  Serial.begin(115200);

  // 1. 连 WiFi
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  Serial.print("正在连 WiFi");
  while (WiFi.status() != WL_CONNECTED) {
    delay(500);
    Serial.print(".");
  }
  Serial.printf("\n已连上，IP 地址 = %s\n", WiFi.localIP().toString().c_str());

  // 2. 首页放个提示，告诉学生去 /update 刷机
  server.on("/", []() {
    server.send(200, "text/html; charset=utf-8",
                "<h2>onecreat 固件升级</h2><p>点这里去升级页面：<a href='/update'>/update</a></p>");
  });

  // 3. 挂上 ElegantOTA 的升级页（自动提供 /update 页面）
  ElegantOTA.begin(&server);
  server.begin();
  Serial.printf("升级页已就绪：用浏览器打开 http://%s/update\n",
                WiFi.localIP().toString().c_str());
}

void loop() {
  server.handleClient();  // 处理网页请求
  ElegantOTA.loop();      // 处理固件上传（这两行别删）

  // ↓↓↓ 你自己的程序写在这里 ↓↓↓
}
