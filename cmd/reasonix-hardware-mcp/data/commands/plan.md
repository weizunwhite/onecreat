---
description: Plan an AI hardware project with board roles and verification steps
argument-hint: [project idea]
---
为这个学生 AI 硬件项目做规划：$ARGUMENTS

要求：
1. 先明确每块板的角色：Arduino/ESP32/Unihiker/MaixCAM/Raspberry Pi。
2. 如果涉及多板通信，用文字画出数据流方向，例如 `[MaixCAM] --UART--> [ESP32] --PWM--> [电机]`。
3. 通信优先级：板间 UART，同板 I2C，高速 SPI，远程 MQTT/HTTP。
4. 给出最小可验证原型，不要一开始做完整复杂系统。
5. 每一步都要有验证方式：编译、烧录、串口输出、SSH 运行、摄像头预览或模型推理结果。
6. 代码要求中文注释、命名清晰、学生能逐行解释。
