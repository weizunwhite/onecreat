---
name: ota-remote-flash
description: 不插 USB、远程把固件烧进 ESP32 的三种方式 —— 局域网直推 / 浏览器拖拽 / 云端拉取。用户说"远程烧录 / 远程上传固件 / WiFi 烧录 / 不插 USB 更新板子 / OTA / 推新固件 / 发布固件"时用。自包含,只依赖 arduino-cli + curl + ssh。
allowed-tools: Bash, Read
---

# ESP32 远程烧录(OTA)

不插 USB、通过网络把新固件送进 ESP32 的三种方式。核心脚本是本目录的
`scripts/ota_flash.py`(纯 shell 封装,不需要 onecreat 或任何 MCP 服务器)。

## 🔒 铁律:第一次必须 USB

空板子是"哑"的——不知道连哪个 WiFi、去哪查更新。所以**每块板子先用 USB 烧一次对应"底座"**,
之后才能远程。底座模板在 `docs/examples/ota/`(或 onecreat 硬件面板「新建 OTA 项目」生成)。
**每次新固件也要继续带上对应底座代码**,否则一传上去就失去无线能力。先确认用户的板子刷过底座;没刷就先引导刷底座。

## 三种方式怎么选(先帮用户挑对)

| 方式 | 子命令 | 适用 | 跨网段? |
|------|--------|------|---------|
| ① 局域网直推 | `lan-push` | 师生同一 WiFi,课堂最常见 | ❌ 同一局域网 |
| ② 浏览器拖拽 | `web-push` | 学生自助、零依赖 | ❌ 同一局域网 |
| ③ 云端拉取 | `cloud-publish` | 真·远程,板子在任何地方 | ✅ 只要板子能上网 |

①② 要求**运行本 skill 的电脑和板子在同一 WiFi**;③ 不要求(板子自己去服务器拉)。

## 怎么用

先 `python3 scripts/ota_flash.py -h` 看全部参数。无硬件时给任意命令加 `--dry-run` 只打印不执行。

### ① 局域网直推(lan-push)
需要:项目目录、板子 IP(串口日志里有)、OTA 口令(默认 `oneup1234`,要和底座一致)。
```bash
python3 scripts/ota_flash.py lan-push --sketch <项目目录> --ip <板子IP> [--password oneup1234] [--fqbn esp32:esp32:esp32]
```

### ② 浏览器拖拽 / curl(web-push)
需要:板子 IP、项目目录(现编)或已编好的 `.bin`。
```bash
python3 scripts/ota_flash.py web-push --ip <板子IP> --sketch <项目目录>
# 或推一个已编好的 bin:  --bin <firmware.bin>
```

### ③ 云端拉取发布(cloud-publish)—— 真·远程
需要:服务器上的 **project 文件夹名**(必须和板子底座 URL 里的一致)、**version**(要比板子当前大)、
固件服务器的 **ssh 别名**(如 `nas`,要能免密登录)。
```bash
python3 scripts/ota_flash.py cloud-publish \
  --sketch <项目目录> --project <项目名> --version 1.0.2 \
  --host nas [--remote-dir /share/Public/onecreat-firmware] [--base-url http://你的服务器IP:9000]
```
发布后板子会在下次轮询(底座默认 30 秒)自动拉取升级。**提醒用户**把板子底座源码里的
`CURRENT_VERSION` 在下次编译时对齐成刚发布的版本号,避免反复升级。

## 必须问清再动手
- **板子 IP**(①②):问用户,或让其看串口日志 `WiFi OK IP=...`。
- **project 名 + version**(③):project 名错→板子查的是别的项目;version 不比当前大→板子不升级。
- **ssh host**(③):默认 `nas`;换 VPS 就改 `--host`/`--remote-dir`/`--base-url`,逻辑不变。

## 诚实报告
脚本对编译失败 / WiFi 烧录失败 / scp 失败都会 `❌` 退非零并打印真实报错。
**真失败就如实告诉用户,不要假称已烧录/已发布。**

## 在 Codex 或别的 agent 里用
本 skill 自包含:把这个文件夹拷过去,直接 `python3 scripts/ota_flash.py <子命令> ...` 即可,
不依赖 Claude Code 的 skill 机制,也不需要 onecreat。脚本 `-h` 自带完整用法。
