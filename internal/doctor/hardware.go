package doctor

// 硬件工具链自检。老师排查「为什么烧不进去」最自然的动作是跑一个体检命令,
// 但 doctor 此前对硬件一无所知——arduino-cli/core/串口的自检只藏在桌面端
// 设备实验台面板里,要点开才知道。这里把最关键的几项纳入 doctor:
// arduino-cli(含桌面端一键安装的托管目录)、已装 core、pio/mpremote、串口设备。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type HardwareReport struct {
	ArduinoCLI  string   `json:"arduino_cli"` // "x.y.z (PATH)" / "托管目录" / "missing"
	Cores       []string `json:"cores,omitempty"`
	PlatformIO  bool     `json:"platformio"`
	MPRemote    bool     `json:"mpremote"`
	SerialPorts []string `json:"serial_ports,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// managedArduinoCLI 返回桌面端一键安装的 arduino-cli 托管路径(存在才返回)。
// 与 cmd/reasonix-hardware-mcp/installer.go 的 managedToolsBin 同一约定:
// Windows 在 %LOCALAPPDATA%\onecreat\tools\bin,其余在 ~/.onecreat/tools/bin,
// 可用 REASONIX_TOOLS_DIR 覆盖根目录。
func managedArduinoCLI() string {
	root := strings.TrimSpace(os.Getenv("REASONIX_TOOLS_DIR"))
	if root == "" {
		if runtime.GOOS == "windows" {
			root = os.Getenv("LOCALAPPDATA")
			if root != "" {
				root = filepath.Join(root, "onecreat", "tools")
			}
		} else if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".onecreat", "tools")
		}
	}
	if root == "" {
		return ""
	}
	bin := filepath.Join(root, "bin", "arduino-cli")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if info, err := os.Stat(bin); err == nil && !info.IsDir() {
		return bin
	}
	return ""
}

func collectHardware() HardwareReport {
	r := HardwareReport{ArduinoCLI: "missing"}

	cli := ""
	if p, err := exec.LookPath("arduino-cli"); err == nil {
		cli = p
		r.ArduinoCLI = "present (PATH)"
	} else if p := managedArduinoCLI(); p != "" {
		cli = p
		r.ArduinoCLI = "present (托管目录 " + redactHome(filepath.Dir(p)) + ")"
	}
	if cli != "" {
		if out := runQuick(cli, "version"); out != "" {
			// 输出形如 "arduino-cli  Version: 1.1.1 Commit: ..." — 只留版本号。
			first := strings.SplitN(out, "\n", 2)[0]
			if i := strings.Index(first, "Version:"); i >= 0 {
				ver := strings.Fields(first[i+len("Version:"):])
				if len(ver) > 0 {
					r.ArduinoCLI = ver[0] + " (" + r.ArduinoCLI[strings.Index(r.ArduinoCLI, "(")+1:]
				}
			}
		}
		for _, line := range strings.Split(runQuick(cli, "core", "list"), "\n") {
			fields := strings.Fields(line)
			// core list 表格行:ID Installed Latest Name;跳过表头。
			if len(fields) >= 2 && strings.Contains(fields[0], ":") {
				r.Cores = append(r.Cores, fields[0]+"@"+fields[1])
			}
		}
	}

	_, pioErr := exec.LookPath("pio")
	r.PlatformIO = pioErr == nil
	_, mpErr := exec.LookPath("mpremote")
	r.MPRemote = mpErr == nil

	r.SerialPorts = listLikelySerialPorts()
	if runtime.GOOS == "windows" && len(r.SerialPorts) == 0 {
		r.Note = "Windows 的 COM 口枚举请在桌面端设备实验台查看;零串口最常见原因是缺 CH340/CP210x 驱动。"
	}
	return r
}

// runQuick 跑一个 3 秒限时的工具子命令,失败返回空串(doctor 绝不能被卡住)。
func runQuick(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// listLikelySerialPorts 用设备文件通配枚举常见 USB 串口(macOS/Linux)。
// Windows 无设备文件,COM 口枚举需要 PowerShell/注册表,留给硬件 MCP 做。
func listLikelySerialPorts() []string {
	patterns := []string{
		"/dev/cu.usbserial*", "/dev/cu.usbmodem*", "/dev/cu.SLAB*", "/dev/cu.wchusb*",
		"/dev/ttyUSB*", "/dev/ttyACM*",
	}
	var ports []string
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		ports = append(ports, matches...)
	}
	return ports
}
