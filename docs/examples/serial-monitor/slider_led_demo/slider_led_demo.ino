// ────────────────────────────────────────────────────────────
// 串口监视器「滑块调 LED 亮度」演示固件 (ESP32)
//
// 配合 onecreat 串口监视器的「交互控件」使用:
//   在「交互控件」里加一个滑块 → 范围 0~255 → 指令模板填 {v} → 拖动即可调亮度。
//
// 工作流程:
//   收到串口发来的数字(0~255)→ 设置板载 LED 亮度 → 回发 brightness:数值
//   (回发的内容在监视器「曲线」视图里会画成一条实时亮度曲线)
// ────────────────────────────────────────────────────────────

const int LED_PIN = 2;       // ESP32 开发板常见的板载 LED 在 GPIO2
const int VALUE_MAX = 255;   // 滑块最大值,对应最亮(analogWrite 默认 8 位:0~255)

void setup() {
  Serial.begin(115200);              // 波特率要和监视器里选的一致(115200)
  pinMode(LED_PIN, OUTPUT);
  analogWrite(LED_PIN, 0);           // 开机先把灯灭掉
  Serial.println("LED brightness demo ready");  // 不带数字,避免污染曲线
}

void loop() {
  // 串口收到一整行(滑块发来的是 "128\n"),读出来转成数字
  if (Serial.available() > 0) {
    String line = Serial.readStringUntil('\n');
    line.trim();                     // 去掉首尾空白和残留的 \r
    if (line.length() > 0) {
      int value = line.toInt();      // 文本转数字,如 "128" → 128
      if (value < 0) value = 0;
      if (value > VALUE_MAX) value = VALUE_MAX;
      analogWrite(LED_PIN, value);   // 设置 LED 亮度:0=灭,255=最亮
      Serial.print("brightness:");   // 回发确认(曲线视图能看到这条亮度线)
      Serial.println(value);
    }
  }
}
