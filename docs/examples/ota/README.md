# ESP32 远程烧录（OTA）三种方案

不插 USB、通过 WiFi/网络把新程序传给 ESP32 的三个底座固件模板。

> **铁律：第一次必须 USB。** 空板子是"哑"的，不知道要连哪个 WiFi、去哪查更新。
> 所以每种方案都得先用 USB 烧一次"底座"，之后才能甩掉 USB。每个新程序也要
> 继续带上对应的底座代码，否则一旦传上去就失去无线能力。

## 三种方案怎么选

| 模板 | 模型 | 怎么用 | 适用 | 跨网段? |
|------|------|--------|------|---------|
| `ota_lan_push/` | 推（电脑→板子） | 电脑往板子 IP 推固件（ArduinoOTA） | 师生同一 WiFi，课堂常见 | ❌ 同一局域网 |
| `ota_web_browser/` | 拉（浏览器上传） | 浏览器开 `http://板子IP/update` 拖 .bin | 学生自助刷机 | ❌ 同一局域网 |
| `ota_cloud_pull/` | 拉（板子→服务器） | 板子定时去服务器查更新、自己下载 | 真·远程，板子在任何地方 | ✅ 只要能上网 |

## 三步通用流程

1. **填 WiFi**：把每个 `.ino` 顶部的 `WIFI_SSID` / `WIFI_PASSWORD` 改成你的网络。
2. **USB 烧底座**（每块板子一次性）：
   ```bash
   arduino-cli compile --fqbn esp32:esp32:esp32 ota_lan_push/
   arduino-cli upload -p /dev/cu.wchusbserial* --fqbn esp32:esp32:esp32 ota_lan_push/
   ```
3. **之后无线更新**：按下面各方案操作，不再插 USB。

## ① 局域网直推（ota_lan_push）

底座烧好后，板子会出现在网络上。无线烧录：

```bash
# 板子的 IP 在串口里能看到（也可用 esp32-onecreat.local）
arduino-cli upload -p 192.168.x.x --fqbn esp32:esp32:esp32 --upload-field password=oneup1234 你的项目/
```

onecreat 集成后：硬件面板「串口」旁边会多一个 **「WiFi 口」**，自动发现网络上的板子，点烧录就走 WiFi。

## ② 浏览器拖拽（ota_web_browser）

需要先装库：`arduino-cli lib install ElegantOTA`。底座烧好后，浏览器打开
`http://板子IP/update`，把 onecreat 编译出的 `.bin` 拖进去即可。

## ③ 云端拉取（ota_cloud_pull）—— 真·远程

把 agent 底座批量刷进所有板子（一次性），之后流程：

```
onecreat 编译新固件 → 发布 firmware.bin + version.txt 到服务器 → 板子定时拉取更新
```

- 服务器**先用 NAS**（192.168.6.131），跑通后改两个 URL 换成 VPS。
- 发布新版本时：覆盖 `firmware.bin`，并把 `version.txt` 改大（板子靠版本号判断要不要更新）。
- 这套会做成 onecreat 的一键「发布固件」+ 一个 CC skill（编译→发布→板子拉取一条龙）。

## 落地顺序

1. ① 真机跑通"USB 烧一次 → 之后 WiFi 推"→ 集成进 onecreat（WiFi 口）。
2. ② 真机跑通浏览器拖拽 → onecreat 加"打开升级页"入口。
3. ③ NAS 上搭固件分发端点 → agent 真机跑通拉取 → 做成 onecreat 发布 + skill。
