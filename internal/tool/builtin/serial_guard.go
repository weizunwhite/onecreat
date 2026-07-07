package builtin

import "regexp"

// 串口独占防护:AI 曾用 bash 起后台 monitor(screen / cat /dev/cu / arduino-cli monitor 等)
// 长期占住 /dev/cu.*,随后再调硬件 MCP 的 arduino_monitor_sample,和自己抢串口导致采样/烧录失败;
// 这类交互式/监听类命令还会在前台阻塞把 agent 的 bash 挂死。原先只有硬件 MCP 事后返回一句提示
// (serialNoOutputGuidance),命令早已执行、端口早已被占,拦不住。这里在 bash 执行前做事前拦截。
//
// 拦截原则(全局拦,不限硬件语境:这些命令在任何语境下都会挂死或占死端口,无正当用法):
// 只拦「会长期持有端口的交互式/监听类」命令,按命令语义匹配,而不是见到 /dev/cu 就杀——
// 合法的 arduino-cli upload -p /dev/cu.xxx、arduino-cli board list、ls /dev/cu.* 都不误伤。
//
// 边界:screen / minicom / picocom 只有带 /dev/ 参数(真去开串口)才拦;不带的(screen -ls
// 这种会话管理)放行。monitor 子命令类(arduino-cli monitor / pio device monitor / idf.py …
// monitor)、mpremote repl|connect、cu -l 本身就是开串口,无条件拦。
type serialHogRule struct {
	name string         // 规则名,便于单测和拦截提示定位命中了哪条
	re   *regexp.Regexp // 命中即视为「会长期独占串口」的命令
}

var serialHogRules = []serialHogRule{
	// 读串口设备 / 交互式串口终端 —— 必须真的带 /dev/(cu|tty) 设备路径才拦,
	// 所以 `screen -ls`、裸 `cat file`、裸 `minicom` 不会命中。[^\n|;&]* 把匹配限制在
	// 同一条命令段内(遇到管道/分号/&& 就断开),避免 `head x && ls /dev/cu.*` 被误判。
	{"reader-on-serial-device", regexp.MustCompile(`(?i)\b(cat|screen|minicom|picocom)\b[^\n|;&]*/dev/(cu|tty)`)},
	// 重定向读写串口设备:`echo r > /dev/cu.xxx`、`cat < /dev/ttyUSB0`,写/读都会占住端口。
	{"redirect-to-serial-device", regexp.MustCompile(`(?i)[<>]\s*/dev/(cu|tty)`)},
	// arduino-cli 的 monitor 子命令(长期读串口)。upload / board list / compile 不含,放行。
	{"arduino-cli-monitor", regexp.MustCompile(`(?i)\barduino-cli\s+monitor\b`)},
	// PlatformIO 串口监视器。`pio run -t upload`、`pio device list` 不含 "device monitor",放行。
	{"pio-device-monitor", regexp.MustCompile(`(?i)\b(pio|platformio)\s+device\s+monitor\b`)},
	// ESP-IDF 串口监视器(含 flash monitor 组合)。`idf.py flash`、`idf.py build` 无 monitor,放行。
	{"idf-monitor", regexp.MustCompile(`(?i)\bidf\.py\b[^\n]*\bmonitor\b`)},
	// mpremote 交互式 REPL / 建立连接(长期占串口)。`mpremote run` 不命中,交给 MCP mpremote_run。
	{"mpremote-repl-connect", regexp.MustCompile(`(?i)\bmpremote\b[^\n]*\b(repl|connect)\b`)},
	// 传统 cu 呼出串口线路:`cu -l /dev/cu.xxx`。
	{"cu-line", regexp.MustCompile(`(?i)\bcu\s+-l\b`)},
}

// serialPortHogRule 返回命中的规则名;命令不属于「长期独占串口」类则返回空串。
// 拆成独立函数是为了做表驱动单测(拦截样本 / 放行样本各自覆盖)。
func serialPortHogRule(command string) string {
	for _, r := range serialHogRules {
		if r.re.MatchString(command) {
			return r.name
		}
	}
	return ""
}

// serialHogDenyMessage 是命中后返回给模型的中文指引:说清为什么拦、该走哪条正道、哪些操作不受影响。
const serialHogDenyMessage = "串口监视请用硬件面板的串口监视器或 arduino_monitor_sample 工具," +
	"不要用 bash 直接打开串口(会独占端口导致烧录/采样失败,交互式命令还会前台阻塞挂死本次会话)。" +
	"合法操作不受影响,例如:arduino-cli upload -p /dev/cu.xxx(烧录)、arduino-cli board list、ls /dev/cu.*。"
