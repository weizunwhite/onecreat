package main

// serialService 是「串口监视器」面板的后端:常驻、双向的串口连接。
//
// - Open 打开端口并起一个读 goroutine,把读到的字节通过事件 "serial:data" 推给前端;
// - Write 往串口写(发送框 / 滑块控件);
// - Close 关闭;
//
// 一次只维护一个连接(再开会先关旧的)。和 hardware 那条「采样 8 秒」的证据链路互不
// 影响:那条是一次性子进程采样,这条是常驻交互连接。
//
// 它对外只依赖一个 Shell(推事件),不认识 App —— 所以 App 上那对
// serialMu/serialSes 字段随之消失,`desktop/serial.go` 只剩转发。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

type serialService struct {
	// shell 每次调用时取,而不是构造时固定:App 的 shell 在 NewApp 里才装上,
	// 且测试里的裸 App 靠 a.sh() 兜底成 noopShell。
	shell func() Shell

	mu  sync.Mutex
	ses *serialSession
}

func newSerialService(shell func() Shell) *serialService {
	return &serialService{shell: shell}
}

func (s *serialService) sh() Shell {
	if s == nil || s.shell == nil {
		return noopShell{}
	}
	return s.shell()
}

type serialSession struct {
	port   serial.Port
	name   string
	baud   int
	closed chan struct{} // 关闭信号:用户主动 Close 时 close 掉,读循环据此干净退出
}

// SerialResult 是打开/写入的统一返回。
type SerialResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Ports 列出可用串口(给面板下拉),过滤掉蓝牙/调试口等噪音。
// 下面几个方法容忍 nil receiver:单元测试里的裸 &App{} 不装配任何服务,
// 与 a.sh() / tabManager 的既有约定一致。
func (s *serialService) Ports() []string {
	ports, err := serial.GetPortsList()
	out := []string{}
	if err != nil {
		return out
	}
	for _, p := range ports {
		low := strings.ToLower(p)
		if strings.Contains(low, "bluetooth") || strings.Contains(low, "debug-console") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Open 按指定波特率打开串口并开始持续读;已有连接会先关掉。
func (s *serialService) Open(portName string, baud int) SerialResult {
	if s == nil {
		return SerialResult{Error: "串口服务未就绪"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(portName) == "" {
		return SerialResult{Error: "未选择串口"}
	}
	if baud <= 0 {
		baud = 115200
	}
	s.closeLocked() // 关掉旧连接

	port, err := serial.Open(portName, &serial.Mode{BaudRate: baud})
	if err != nil {
		return SerialResult{Error: "打开串口失败:" + err.Error() + "(检查端口没被别的程序占用、板子已接好)"}
	}
	// ESP32+CH340:别一直拉 DTR/RTS,否则可能把板子摁进复位/下载模式;设 false 直接读
	// 正在跑的输出(代价是不会触发一次复位、看不到 boot 日志,对监视来说无所谓)。
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)
	// 读超时让 Read 周期返回,便于干净关闭(否则会一直阻塞在 Read 上)。
	_ = port.SetReadTimeout(200 * time.Millisecond)

	ses := &serialSession{port: port, name: portName, baud: baud, closed: make(chan struct{})}
	s.ses = ses
	go s.readLoop(ses)
	return SerialResult{OK: true}
}

// readLoop 持续读串口,把数据通过事件推给前端;端口出错(拔线等)时发 serial:closed。
func (s *serialService) readLoop(ses *serialSession) {
	buf := make([]byte, 1024)
	for {
		select {
		case <-ses.closed:
			return // 用户主动关了
		default:
		}
		n, err := ses.port.Read(buf)
		if n > 0 {
			s.sh().Emit("serial:data", string(buf[:n]))
		}
		if err != nil {
			// 区分「用户主动关」和「真出错(拔线/端口消失)」:前者不报错。
			select {
			case <-ses.closed:
			default:
				s.sh().Emit("serial:closed", err.Error())
			}
			s.mu.Lock()
			if s.ses == ses {
				s.ses = nil
			}
			s.mu.Unlock()
			return
		}
	}
}

// Write 往当前串口写一段数据(发送框 / 控件用)。
func (s *serialService) Write(data string) SerialResult {
	if s == nil {
		return SerialResult{Error: "串口服务未就绪"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ses == nil {
		return SerialResult{Error: "串口未连接"}
	}
	if _, err := s.ses.port.Write([]byte(data)); err != nil {
		return SerialResult{Error: "发送失败:" + err.Error()}
	}
	return SerialResult{OK: true}
}

// Close 关闭当前串口连接(幂等)。
func (s *serialService) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

// closeLocked 关闭并清空当前会话;调用方必须持有 mu。
func (s *serialService) closeLocked() {
	if s.ses == nil {
		return
	}
	close(s.ses.closed) // 通知读循环退出
	_ = s.ses.port.Close()
	s.ses = nil
}

// ReleaseForToolUse 是传给 boot.Build 的 PreToolUse 观察回调:AI 通过硬件 MCP 工具烧录 /
// 采串口前,如果常驻「串口监视器」正占着串口,先自动关掉它——MCP 是独立子进程,不知道
// 桌面进程持着串口,不先释放就会撞 busy 失败(面板「烧录」按钮已有同款逻辑,这里补的是
// AI→内核→MCP 这条路)。外部程序占用维持原失败路径(失败知识卡有指引)。
// 这是每次工具调用都会进的热路径:shouldReleaseSerial 命中前不碰任何锁,零开销。
func (s *serialService) ReleaseForToolUse(_ context.Context, name string, args json.RawMessage) {
	if s == nil || !shouldReleaseSerial(name, args) {
		return
	}
	s.mu.Lock()
	open := s.ses != nil
	if open {
		s.closeLocked()
	}
	s.mu.Unlock()
	if open {
		// 复用 serial:closed 事件:前端的 onSerialClosed 会把面板状态翻成「未连接」并显示原因,
		// 让用户知道监视器为什么断了(closeLocked 是干净关闭、读循环不会再自己发这个事件)。
		s.sh().Emit("serial:closed", "已自动断开,让本次烧录/串口采样独占串口")
	}
}

// shouldReleaseSerial 判断某个(可能带 mcp__server__ 前缀的)工具会不会占用本地 USB 串口。
// 命中的工具在执行前需要先让出串口监视器。判定按工具基名 + 参数:
//   - arduino_upload / arduino_monitor_sample / mpremote_run:一定占串口;
//   - platformio_run:targets 含 upload 或 monitor 才占;
//   - esp_idf_run:action 为 flash / monitor / flash_monitor 才占。
//
// OTA(arduino_ota_upload)走 WiFi、firmware_publish 走远程、ssh_deploy_run 走 SSH,都不碰
// 本地串口,不命中。
func shouldReleaseSerial(name string, args json.RawMessage) bool {
	base := name
	// 剥掉 MCP 适配器加的 mcp__<server>__ 前缀,拿到原始工具名。
	if i := strings.LastIndex(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	switch base {
	case "arduino_upload", "arduino_monitor_sample", "mpremote_run":
		return true
	case "platformio_run":
		var p struct {
			Targets []string `json:"targets"`
		}
		_ = json.Unmarshal(args, &p)
		for _, t := range p.Targets {
			if t == "upload" || t == "monitor" {
				return true
			}
		}
		return false
	case "esp_idf_run":
		var p struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &p)
		return p.Action == "flash" || p.Action == "monitor" || p.Action == "flash_monitor"
	default:
		return false
	}
}
