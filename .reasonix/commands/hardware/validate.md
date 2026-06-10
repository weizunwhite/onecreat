---
description: Automatically validate a hardware project before flashing
argument-hint: [project path/platform/board]
---
自动验证硬件项目：$ARGUMENTS

流程：
1. 先调用 `mcp__hardware__hardware_detect` 查看当前目录、工具链和串口。
2. 调用 `mcp__hardware__hardware_project_audit` 检查硬件项目上下文是否完整：
   - `hardware_manifest.json`
   - `docs/wiring.md`
   - `docs/verification.md`
   - `tests/hardware_checklist.md`
   - 平台入口文件
3. 调用 `mcp__hardware__hardware_project_validate` 自动判断项目类型并执行本机可做的验证：
   - Arduino：编译 sketch
   - PlatformIO：执行 `pio run`
   - ESP-IDF：有 `idf.py` 时 build；没有时说明如何安装或接入官方 MCP
   - Python/MicroPython/Unihiker/MaixCAM/RPi：执行 Python 语法检查
4. 调用 `mcp__hardware__hardware_evidence_record`，把审计和验证结果追加到 `tests/hardware_evidence.jsonl` 和 `tests/hardware_checklist.md`。
5. 调用 `mcp__hardware__hardware_evidence_status`，根据状态判断：
   - `hardware_verified`：可以说真实硬件验证证据齐全。
   - `hardware_pending`：只能说本机验证已通过，真实硬件阶段未完成。
   - `failed` / `local_pending`：继续修复或补验证。
6. 如果审计或验证结果是 `failed`，读取相关文件，做最小修复，再重新调用对应工具。
7. 如果结果是 `passed`，再判断是否有真实开发板串口或 SSH 设备；有设备才继续 upload/flash/monitor，并继续记录 evidence。
8. 输出要区分：项目上下文已审计、编译/语法已验证、证据已记录、因缺工具跳过、因缺真实硬件未验证。
