package builtin

import "testing"

// TestSerialPortHogRule 表驱动:拦截样本必须命中(返回非空规则名),放行样本必须全部放行(返回空)。
// 放行样本刻意覆盖 upload / board list / ls,证明「见到 /dev/cu 就杀」的误伤没有发生。
func TestSerialPortHogRule(t *testing.T) {
	// 应拦截:会长期独占串口的交互式/监听类命令。
	deny := []string{
		"screen /dev/cu.usbserial-1420 115200",
		"cat /dev/cu.usbserial-1420",
		"echo reset > /dev/cu.usbserial-1420",
		"cat < /dev/ttyUSB0",
		"arduino-cli monitor -p /dev/cu.usbserial --config baudrate=115200",
		"pio device monitor -b 115200",
		"idf.py -p /dev/cu.usbserial monitor",
		"idf.py flash monitor",
		"mpremote connect /dev/cu.usbserial repl",
		"mpremote repl",
		"minicom -D /dev/cu.usbserial",
		"picocom /dev/ttyUSB0 -b 115200",
		"cu -l /dev/cu.usbserial -s 115200",
	}
	for _, cmd := range deny {
		if serialPortHogRule(cmd) == "" {
			t.Errorf("应拦截但放行了:%q", cmd)
		}
	}

	// 应放行:合法的编译/烧录/枚举/文件操作,即使命令里出现 /dev/cu 也不能误伤。
	allow := []string{
		"arduino-cli upload -p /dev/cu.usbserial-1420 --fqbn arduino:avr:nano sketch/",
		"arduino-cli board list",
		"arduino-cli compile --fqbn arduino:avr:nano sketch/",
		"ls /dev/cu.*",
		"ls -la /dev/tty*",
		"screen -ls",
		"pio run -t upload",
		"pio device list",
		"idf.py build",
		"idf.py flash",
		"mpremote run main.py",
		"cat sketch.ino",
		"head -100 build.log && ls /dev/cu.*",
	}
	for _, cmd := range allow {
		if rule := serialPortHogRule(cmd); rule != "" {
			t.Errorf("应放行但被拦截了:%q(命中规则 %s)", cmd, rule)
		}
	}
}
