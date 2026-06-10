---
description: Create a teaching-friendly AI hardware project scaffold
argument-hint: [platform] [project_name] [board]
---
创建一个 AI 硬件编程项目脚手架。

参数：
- platform: $1，例如 arduino / platformio / esp_idf / micropython / unihiker_python / maixcam_python / raspberry_pi_python
- project_name: $2，使用英文 snake_case
- board: $3，例如 uno / esp32dev / esp32s3 / esp32c3 / maixcam / unihiker

流程：
1. 先调用 `mcp__hardware__hardware_detect` 检查本机工具链和串口。
2. 如果参数缺失，先问用户，不要猜硬件平台。
3. 调用 `mcp__hardware__hardware_project_scaffold` 创建项目。
4. 创建后调用 `mcp__hardware__hardware_project_audit` 检查 manifest、接线说明、验证流程和硬件检查清单是否完整。
5. 再调用 `mcp__hardware__hardware_project_validate` 做自动编译或语法检查。
6. 调用 `mcp__hardware__hardware_evidence_record`，把本机编译或语法检查结果写入 `tests/hardware_evidence.jsonl` 和 `tests/hardware_checklist.md`。
7. 调用 `mcp__hardware__hardware_evidence_status`，确认当前是 `hardware_pending` 还是 `hardware_verified`。
8. 读取 README 和主程序，确认中文注释、常量命名、函数结构适合学生逐行解释。
9. 给出下一条验证命令；如果是 ESP-IDF 项目，同时调用 `mcp__hardware__esp_idf_mcp_config` 给出官方 MCP 接入配置。
