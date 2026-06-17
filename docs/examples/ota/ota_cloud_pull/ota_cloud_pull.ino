// ────────────────────────────────────────────────────────────
// OTA 底座 ③：云端拉取 agent（真·远程，跨网段）
//
// 作用：板子定时去一个固件服务器查"有没有新版本"，有就自己下载并安装。
//       板子可以在学生家、教室任何地方，只要能上网就能被远程更新——
//       你在 onecreat 里编译好新固件、发布到服务器，板子自己拉下来。
//
// 这就是"底座写一次、刷一次、之后永久远程"的那个 agent：
//   - 开学时把全班板子用 USB 刷一遍这个 agent（一次性）；
//   - 之后所有更新都走服务器拉取，再不用插 USB。
//
// 服务器先用你的 NAS（你的服务器IP），跑通后再换 VPS——只改下面两个 URL 即可。
//
// 服务器上要放两个文件：
//   version.txt   ：一行版本号，比如  1.0.1
//   firmware.bin  ：编译出来的固件（onecreat 发布上去）
// 板子比对版本号，不一样就拉 firmware.bin 装上。
// ────────────────────────────────────────────────────────────

#include <WiFi.h>
#include <HTTPClient.h>
#include <HTTPUpdate.h>

// ↓↓↓ 改成你自己的 WiFi ↓↓↓
const char* WIFI_SSID     = "你的WiFi名称";
const char* WIFI_PASSWORD = "你的WiFi密码";

// ↓↓↓ 固件服务器：NAS 上的 nginx(端口 9000),按项目分子目录 ↓↓↓
// 部署见本目录 README 的「NAS 固件服务器」一节。以后换 VPS 只改这两行域名。
const char* FW_VERSION_URL = "http://你的服务器IP:9000/你的项目/version.txt";
const char* FW_BIN_URL     = "http://你的服务器IP:9000/你的项目/firmware.bin";

// 这块板子当前烧的是哪个版本（每次发布新固件时把它改大）
const char* CURRENT_VERSION = "1.0.0";

const unsigned long CHECK_INTERVAL_MS = 30000;  // 每 30 秒查一次更新
unsigned long lastCheck = 0;

void setup() {
  Serial.begin(115200);

  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  Serial.print("正在连 WiFi");
  while (WiFi.status() != WL_CONNECTED) {
    delay(500);
    Serial.print(".");
  }
  Serial.printf("\n已连上，IP = %s，当前固件版本 = %s\n",
                WiFi.localIP().toString().c_str(), CURRENT_VERSION);
  checkForUpdate();  // 开机先查一次
}

void loop() {
  // 每隔 CHECK_INTERVAL_MS 查一次更新（不阻塞你自己的程序）
  if (millis() - lastCheck > CHECK_INTERVAL_MS) {
    lastCheck = millis();
    checkForUpdate();
  }

  // ↓↓↓ 你自己的程序写在这里 ↓↓↓
}

// 查服务器上的版本号，和本机不一样就下载安装新固件
void checkForUpdate() {
  if (WiFi.status() != WL_CONNECTED) return;

  WiFiClient client;
  HTTPClient http;

  // 1. 取服务器最新版本号
  http.begin(client, FW_VERSION_URL);
  int code = http.GET();
  if (code != HTTP_CODE_OK) {
    Serial.printf("查版本失败（HTTP %d），稍后再试\n", code);
    http.end();
    return;
  }
  String latest = http.getString();
  latest.trim();
  http.end();

  // 2. 版本一样就不用更新
  if (latest == CURRENT_VERSION) {
    Serial.printf("已是最新（%s），无需更新\n", CURRENT_VERSION);
    return;
  }

  // 3. 有新版本：下载并安装 firmware.bin
  Serial.printf("发现新版本 %s（当前 %s），开始下载安装…\n", latest.c_str(), CURRENT_VERSION);
  t_httpUpdate_return ret = httpUpdate.update(client, FW_BIN_URL);
  switch (ret) {
    case HTTP_UPDATE_OK:
      Serial.println("更新成功，重启中…");  // 装好会自动重启
      break;
    case HTTP_UPDATE_FAILED:
      Serial.printf("更新失败：%s\n", httpUpdate.getLastErrorString().c_str());
      break;
    case HTTP_UPDATE_NO_UPDATES:
      Serial.println("服务器没有可用更新");
      break;
  }
}
