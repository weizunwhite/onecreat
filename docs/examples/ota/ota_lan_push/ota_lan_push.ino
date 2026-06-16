// ────────────────────────────────────────────────────────────
// OTA 底座 ①：局域网直推（ArduinoOTA）
//
// 作用：板子连上 WiFi 后，在局域网里"现身"成一个可烧录目标。
//       之后电脑（onecreat / Arduino IDE / arduino-cli）就能用 WiFi 把新
//       程序推给它，不用插 USB。
//
// 关键前提：
//   1) 第一次必须用 USB 烧这段底座（空板子做不到无线）。
//   2) 之后每次新程序都要带上这段 ArduinoOTA 代码，否则推一次就失去无线能力。
//
// 适用：师生在【同一个 WiFi】下，课堂最常见。
// ────────────────────────────────────────────────────────────

#include <WiFi.h>
#include <ArduinoOTA.h>

// ↓↓↓ 改成你自己的 WiFi（板子和电脑要在同一个 WiFi 下）↓↓↓
const char* WIFI_SSID     = "你的WiFi名称";
const char* WIFI_PASSWORD = "你的WiFi密码";

const char* OTA_HOSTNAME = "esp32-onecreat";  // 板子在网络上的名字（电脑端按这个找它）
const char* OTA_PASSWORD = "oneup1234";       // 烧录口令，防别人往你板子乱推

const int LED_PIN = 2;  // 板载 LED，用来肉眼确认程序在跑

void setup() {
  Serial.begin(115200);
  pinMode(LED_PIN, OUTPUT);

  // 1. 连 WiFi
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  Serial.print("正在连 WiFi");
  while (WiFi.status() != WL_CONNECTED) {
    delay(500);
    Serial.print(".");
  }
  Serial.printf("\n已连上，IP 地址 = %s\n", WiFi.localIP().toString().c_str());

  // 2. 启动 OTA 监听（设好名字和口令）
  ArduinoOTA.setHostname(OTA_HOSTNAME);
  ArduinoOTA.setPassword(OTA_PASSWORD);
  ArduinoOTA.begin();
  Serial.println("OTA 已就绪：现在可以用 WiFi 烧录了（在 onecreat 选 WiFi 口）");
}

void loop() {
  // 关键：每一圈都要处理 OTA 请求，这一行千万别删，否则无线烧录会失效
  ArduinoOTA.handle();

  // ↓↓↓ 你自己的程序写在这里（示例：LED 每秒闪一下，证明板子活着）↓↓↓
  digitalWrite(LED_PIN, millis() / 1000 % 2);
}
