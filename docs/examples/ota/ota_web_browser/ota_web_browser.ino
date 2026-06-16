// ────────────────────────────────────────────────────────────
// OTA 底座 ②：浏览器拖拽（零依赖手写版）
//
// 作用：板子开一个网页 http://板子IP/，选中 onecreat 编译出的 .bin 点上传，
//       就把固件刷进去。学生自助、不用装任何工具，浏览器就行。
//
// 关键前提（同①）：第一次 USB 烧底座；之后每个新程序都要带上这段 Web 服务代码。
//
// 为什么不用 ElegantOTA：这里只用 ESP32 内置的 WebServer + Update，零额外库、
//       无 AGPL 许可证负担、学生能逐行读懂；upload 走标准 multipart 表单，
//       onecreat 也能直接用 HTTP 推（已真机验证）。
//
// 适用：学生自助刷机、临时给某块板子换程序。
// ────────────────────────────────────────────────────────────

#include <WiFi.h>
#include <WebServer.h>
#include <Update.h>

// ↓↓↓ 改成你自己的 WiFi ↓↓↓
const char* WIFI_SSID     = "你的WiFi名称";
const char* WIFI_PASSWORD = "你的WiFi密码";

const char* FW_TAG = "v1.0";  // 当前固件版本，显示在网页上，方便确认刷没刷成功

WebServer server(80);  // 网页服务跑在 80 端口

// 升级页面：显示当前版本 + 一个选文件上传的表单
const char* PAGE = R"HTML(<!DOCTYPE html><html><head><meta charset="utf-8">
<title>onecreat 固件升级</title></head>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto">
<h2>onecreat 固件升级</h2><p>当前固件：<b>%TAG%</b></p>
<form method="POST" action="/update" enctype="multipart/form-data">
<input type="file" name="firmware" accept=".bin"><br><br>
<input type="submit" value="上传并升级"></form></body></html>)HTML";

void handleRoot() {
  String page = PAGE;
  page.replace("%TAG%", FW_TAG);
  server.send(200, "text/html; charset=utf-8", page);
}

// 上传完成后的回应：告诉浏览器成没成，然后重启进入新固件
void handleDone() {
  server.send(200, "text/plain; charset=utf-8",
              Update.hasError() ? "升级失败" : "升级成功，正在重启…");
  delay(400);
  ESP.restart();
}

// 接收上传的 .bin，一块一块写进 Flash
void handleUpload() {
  HTTPUpload& up = server.upload();
  if (up.status == UPLOAD_FILE_START) {
    Update.begin(UPDATE_SIZE_UNKNOWN);          // 开始：自动分配空间
  } else if (up.status == UPLOAD_FILE_WRITE) {
    Update.write(up.buf, up.currentSize);       // 中间：写入收到的数据块
  } else if (up.status == UPLOAD_FILE_END) {
    Update.end(true);                           // 结束：收尾校验
  }
}

void setup() {
  Serial.begin(115200);

  // 1. 连 WiFi
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  Serial.print("正在连 WiFi");
  while (WiFi.status() != WL_CONNECTED) { delay(500); Serial.print("."); }
  Serial.printf("\n已连上，IP = %s\n", WiFi.localIP().toString().c_str());

  // 2. 注册网页路由：首页给上传表单，/update 收文件
  server.on("/", HTTP_GET, handleRoot);
  server.on("/update", HTTP_POST, handleDone, handleUpload);  // 第3参收尾,第4参收数据
  server.begin();
  Serial.printf("升级页已就绪：浏览器打开 http://%s/\n", WiFi.localIP().toString().c_str());
}

void loop() {
  server.handleClient();  // 处理网页/上传请求（别删）

  // ↓↓↓ 你自己的程序写在这里 ↓↓↓
}
