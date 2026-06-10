---
description: Debug hardware build, flash, serial, or runtime logs
argument-hint: [platform/log focus]
---
调试硬件项目问题：$ARGUMENTS

流程：
1. 调用 `mcp__hardware__hardware_detect` 查看工具链、串口、项目类型。
2. 调用 `mcp__hardware__hardware_project_audit` 检查 manifest、接线说明、验证流程和硬件检查清单是否完整。
3. 优先调用 `mcp__hardware__hardware_project_validate` 自动完成能在本机做的编译或语法检查。
4. 根据项目类型继续选择工具：
   - Arduino: `arduino_compile` → `arduino_upload` → `arduino_monitor_sample`
   - PlatformIO: `platformio_run`
   - ESP-IDF: 优先说明官方 ESP-IDF MCP；必要时用 `esp_idf_run`
   - MicroPython: `mpremote_run`
   - Unihiker/MaixCAM/RPi: `ssh_deploy_run`
5. 分清错误类型：项目上下文缺失、编译失败、库缺失、端口占用、烧录失败、串口无输出、波特率错误、接线/供电问题。
6. 每完成一次编译、烧录、串口、mpremote 或 SSH 验证，都调用 `mcp__hardware__hardware_evidence_record` 记录命令、结果和关键输出。
7. 调用 `mcp__hardware__hardware_evidence_status` 汇总当前证据状态，避免把本机验证误说成实机通过。
8. 每次只改一个最小问题，然后给出下一条验证命令。
9. 不要忽略学生答辩要求：修复后的代码仍要能逐行解释。
