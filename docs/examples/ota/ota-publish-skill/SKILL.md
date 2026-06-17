---
name: ota-publish
description: 把当前硬件项目编译并发布固件到远程服务器(NAS/VPS),让刷了 OTA agent 的 ESP32 板子自动拉取升级。用户说"发布固件 / 远程更新板子 / 推新固件 / OTA 发布"时用。
allowed-tools: mcp__hardware__hardware_detect, mcp__hardware__hardware_board_profile, mcp__hardware__firmware_publish
---

# 发布固件到远程(OTA 云端拉取)

帮用户把固件**不插 USB、远程**推给 ESP32 板子。原理:板子已刷「云端拉取 agent」底座,
会定时去固件服务器查版本,变了就自己下载升级。这个 skill 负责"编译 + 发布到服务器"那一步。

## 前提(不满足就先说明)
- 板子已经刷过「云端拉取 agent」底座(onecreat 硬件面板 →「远程烧录 → 新建 OTA 项目 → 云端拉取」生成,第一次 USB 烧一次)。
- 固件服务器默认是用户的 NAS:`http://你的服务器IP:9000/`,目录 `/share/Public/onecreat-firmware/<项目>/`(nginx 容器 `onecreat-fw`)。换 VPS 时改下面的参数。

## 步骤
1. 调 `mcp__hardware__hardware_detect` 确认当前硬件项目目录;板卡 fqbn 默认 `esp32:esp32:esp32`(必要时 `hardware_board_profile` 确认)。
2. 问清两件事:
   - **project_name**:服务器上的项目文件夹名,**必须和板子 agent 里 URL 用的那个一致**(否则板子查的是别的项目)。
   - **version**:新版本号,**要比板子当前版本大**(如 `1.0.2`),否则板子不会更新。
3. 调 `mcp__hardware__firmware_publish`:
   - `project_name`、`version`
   - `ssh_host=nas`、`remote_dir=/share/Public/onecreat-firmware`、`base_url=http://你的服务器IP:9000`
   - `sketch_dir=<当前项目目录>`、`fqbn=<板卡 fqbn>`(不传 bin_path 就让它现编)
4. 把返回的固件 URL、版本号如实转述给用户。
5. 提醒:板子(已刷 agent)会在**下次轮询**(底座默认 30 秒)自动拉取升级;同时记得把板子 agent 源码里的 `CURRENT_VERSION` 在下次编译时对齐,避免反复升级。

## 注意
- 换 VPS:把 `ssh_host` / `remote_dir` / `base_url` 改成 VPS 的,并同步改板子 agent 里的两个 URL,逻辑不变。
- `firmware_publish` 真失败(scp / 编译报错)就**如实报告**,不要假称已发布。
