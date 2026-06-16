# 串口监视器演示固件

配合 onecreat 桌面端「硬件编程 → 串口监视器」使用的两个 ESP32 演示固件，
用来在课堂上展示 Phase 2（实时曲线）和 Phase 3（交互控件）。

> 板卡：ESP32 Dev Module ｜ 波特率：115200 ｜ 串口监视器里选对端口后点「连接」。

## slider_led_demo —— 滑块调 LED 亮度（Phase 3）

开发板读取串口发来的数字（0~255），设置板载 LED（GPIO2）亮度，并回发 `brightness:数值`。

用法：
1. 烧录后，在串口监视器点「连接」。
2. 点滑块图标打开「交互控件」，拖动「数值」滑块（指令模板 `{v}`，范围 0~255）。
3. LED 亮度跟着变；切到「曲线」视图还能看到一条 brightness 亮度曲线。

> 若板载 LED 不在 GPIO2，可外接一个 LED（串限流电阻）到 GPIO2 与 GND。

## dual_curve_demo —— sin/cos 双曲线（Phase 2）

开发板每 40ms 输出 `sin:数值 cos:数值` 两路数据，演示多路实时曲线。

用法：
1. 烧录后，在串口监视器点「连接」。
2. 切到「曲线」视图，即可看到 sin、cos 两条不同颜色、不同频率的实时波形，
   图例分别显示两条的当前值。

## 烧录方法

Arduino IDE 直接打开 `.ino` 烧录；或用 arduino-cli：

```bash
arduino-cli compile --fqbn esp32:esp32:esp32 slider_led_demo/
arduino-cli upload -p /dev/cu.wchusbserial* --fqbn esp32:esp32:esp32 slider_led_demo/
```

> 烧录前先在串口监视器点「断开」（或关闭面板），否则端口被占用会上传失败。
