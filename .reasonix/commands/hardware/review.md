---
description: Review firmware for teaching clarity and hardware correctness
argument-hint: [path]
---
审查硬件项目代码：$ARGUMENTS

先调用 `mcp__hardware__hardware_project_audit`，确认 manifest、接线说明、验证流程、硬件检查清单和入口文件是否完整。再调用 `mcp__hardware__hardware_evidence_status`，确认当前证据状态是本机已验证、实机待验证，还是实机已验证。

重点：
1. 每个函数是否有中文说明，学生是否能逐行解释。
2. 魔数是否提取为命名常量。
3. Arduino/ESP32 是否避免长时间阻塞式 `delay()`。
4. ESP-IDF 是否使用 `esp_log`、检查错误返回值、清晰处理事件。
5. 多板通信是否说明方向、波特率、消息格式和异常处理。
6. 是否存在危险引脚、串口波特率不一致、端口硬编码、ESP32 ADC2+WiFi 冲突。
7. `tests/hardware_evidence.jsonl` 是否记录了编译、烧录、串口或 SSH 验证证据。
8. 输出按严重程度排序，带文件和行号。
