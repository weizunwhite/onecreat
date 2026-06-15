package main

// 一键安装核心硬件工具链(Phase 1)。设计目标:
//   - Windows / macOS 双平台(Linux 顺带支持);
//   - 装到用户目录,免管理员权限;
//   - 不依赖 Python —— arduino-cli 是单文件二进制,直接下官方发布包解压即可;
//   - 装完 arduino-cli 再装板卡 core(默认 arduino:avr + esp32:esp32),覆盖
//     Arduino 全系 + ESP32 全系,也就是「大部分硬件」。
//
// 给学生/老师打包后,他们点一下「一键安装」就能从零把编译/烧录环境备齐。
// 真正的坑(USB 驱动、ESP-IDF、pip 系工具)留到 Phase 2,本文件只做最稳的那层。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// managedToolsBin 返回本应用托管的工具安装目录(放 arduino-cli 这类单文件二进制)。
// 装到用户目录免 admin:Windows 用 %LOCALAPPDATA%,其余用 ~/.onecreat。
// 可用环境变量 REASONIX_TOOLS_DIR 覆盖根目录(测试/迁移用)。
func managedToolsBin() string {
	if v := strings.TrimSpace(os.Getenv("REASONIX_TOOLS_DIR")); v != "" {
		return filepath.Join(v, "bin")
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, "AppData", "Local")
			}
		}
		if base == "" {
			return ""
		}
		return filepath.Join(base, "onecreat", "tools", "bin")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".onecreat", "tools", "bin")
}

// ensureManagedToolsOnPath 把托管目录加到本进程 PATH 头部,让 exec.LookPath 能找到
// 我们自己装的 arduino-cli(它不在系统 PATH 上)。幂等,可重复调用。
func ensureManagedToolsOnPath() {
	dir := managedToolsBin()
	if dir == "" {
		return
	}
	path := os.Getenv("PATH")
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return // 已在 PATH 里
		}
	}
	if path == "" {
		_ = os.Setenv("PATH", dir)
		return
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+path)
}

// arduinoCLIDownloadURL 返回当前 OS/ARCH 对应的 arduino-cli 官方发布包 URL。
// isZip=true 表示 Windows 的 .zip,否则是 .tar.gz。
func arduinoCLIDownloadURL() (url string, isZip bool, err error) {
	const base = "https://downloads.arduino.cc/arduino-cli/arduino-cli_latest_"
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return base + "macOS_ARM64.tar.gz", false, nil
		}
		return base + "macOS_64bit.tar.gz", false, nil
	case "windows":
		return base + "Windows_64bit.zip", true, nil
	case "linux":
		switch runtime.GOARCH {
		case "arm64":
			return base + "Linux_ARM64.tar.gz", false, nil
		case "arm":
			return base + "Linux_ARMv7.tar.gz", false, nil
		default:
			return base + "Linux_64bit.tar.gz", false, nil
		}
	}
	return "", false, fmt.Errorf("暂不支持的系统 %s/%s,请按提示手动安装 arduino-cli", runtime.GOOS, runtime.GOARCH)
}

// installArduinoCLI 下载官方 arduino-cli 发布包,解压出二进制写进托管目录,返回二进制路径。
func installArduinoCLI(timeout time.Duration) (string, error) {
	url, isZip, err := arduinoCLIDownloadURL()
	if err != nil {
		return "", err
	}
	binDir := managedToolsBin()
	if binDir == "" {
		return "", errors.New("无法确定安装目录(取不到用户目录)")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("创建安装目录失败: %w", err)
	}
	data, err := httpGetBytes(url, timeout)
	if err != nil {
		return "", fmt.Errorf("下载 arduino-cli 失败(%s): %w", url, err)
	}
	binName := "arduino-cli"
	entryBase := "arduino-cli"
	if runtime.GOOS == "windows" {
		binName = "arduino-cli.exe"
		entryBase = "arduino-cli.exe"
	}
	target := filepath.Join(binDir, binName)
	if isZip {
		err = extractZipFile(data, entryBase, target)
	} else {
		err = extractTarGzFile(data, entryBase, target)
	}
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(target, 0o755)
	}
	ensureManagedToolsOnPath() // 让本进程后续的 core 安装能找到刚装的 arduino-cli
	return target, nil
}

func httpGetBytes(url string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// extractTarGzFile 从 tar.gz 字节里找出 base 名为 wantBase 的文件,原子写到 target。
func extractTarGzFile(archive []byte, wantBase, target string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("解压 gzip 失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) == wantBase {
			return writeFileFromReader(target, tr)
		}
	}
	return fmt.Errorf("压缩包里没找到 %s", wantBase)
}

// extractZipFile 从 zip 字节里找出 base 名为 wantBase 的文件,原子写到 target。
func extractZipFile(archive []byte, wantBase, target string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("解压 zip 失败: %w", err)
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) == wantBase {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFileFromReader(target, rc)
		}
	}
	return fmt.Errorf("压缩包里没找到 %s", wantBase)
}

// writeFileFromReader 把 r 写到 target.tmp 再原子 rename,避免半截文件。
func writeFileFromReader(target string, r io.Reader) error {
	tmp := target + ".download.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

// --- 一键安装编排 ---

type toolInstallStep struct {
	Tool    string `json:"tool"`
	Action  string `json:"action"` // already_present | installed | failed | skipped
	OK      bool   `json:"ok"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

type installToolchainReport struct {
	Steps      []toolInstallStep `json:"steps"`
	AllOK      bool              `json:"allOK"`
	ManagedDir string            `json:"managedDir"`
	NextStep   string            `json:"nextStep"`
}

// defaultInstallCores 是一键安装默认装的 core:覆盖 Arduino 全系 + ESP32 全系。
var defaultInstallCores = []string{"arduino:avr", "esp32:esp32"}

// arduinoCoreInstalled 用 `arduino-cli core list` 判断某个 core 是否已装(子串匹配 core id)。
func arduinoCoreInstalled(core string) bool {
	out, err := runCommandText("arduino-cli", []string{"core", "list"}, "", 30*time.Second)
	if err != nil {
		return false
	}
	return strings.Contains(out, core)
}

// runInstallToolchain 是 hardware_install_toolchain 工具的实现:
// 缺 arduino-cli 就下载装好,再把指定的 core 补齐;每步回报状态,失败给手动指引。
func runInstallToolchain(args map[string]any) (string, error) {
	ensureManagedToolsOnPath()
	timeout := timeoutArg(args, "timeout_seconds", 600*time.Second)
	report := installToolchainReport{ManagedDir: managedToolsBin()}

	// 1) arduino-cli 本体
	if p, err := exec.LookPath("arduino-cli"); err == nil {
		report.Steps = append(report.Steps, toolInstallStep{Tool: "arduino-cli", Action: "already_present", OK: true, Path: p, Message: "已安装,跳过"})
	} else {
		target, ierr := installArduinoCLI(timeout)
		if ierr != nil {
			report.Steps = append(report.Steps, toolInstallStep{Tool: "arduino-cli", Action: "failed", OK: false, Message: ierr.Error() + "。" + toolInstallHint("arduino-cli")})
			report.AllOK = false
			report.NextStep = "arduino-cli 没装上,后续 core 无法安装。检查网络后重试一键安装,或按上面的手动指引装。"
			return prettyJSON(report), nil
		}
		report.Steps = append(report.Steps, toolInstallStep{Tool: "arduino-cli", Action: "installed", OK: true, Path: target, Message: "已下载并安装到 " + target})
	}

	// 2) 板卡 core
	cores := strSliceArg(args, "cores")
	if len(cores) == 0 {
		cores = defaultInstallCores
	}
	for _, core := range cores {
		core = strings.TrimSpace(core)
		if core == "" {
			continue
		}
		if arduinoCoreInstalled(core) {
			report.Steps = append(report.Steps, toolInstallStep{Tool: core, Action: "already_present", OK: true, Message: "core 已装,跳过"})
			continue
		}
		if _, err := runArduinoCoreInstall(map[string]any{"core": core, "timeout_seconds": timeout.Seconds()}); err != nil {
			report.Steps = append(report.Steps, toolInstallStep{Tool: core, Action: "failed", OK: false, Message: "core 安装失败: " + err.Error()})
			continue
		}
		report.Steps = append(report.Steps, toolInstallStep{Tool: core, Action: "installed", OK: true, Message: "core 安装完成"})
	}

	report.AllOK = true
	for _, s := range report.Steps {
		if !s.OK {
			report.AllOK = false
			break
		}
	}
	if report.AllOK {
		report.NextStep = "工具链就绪。现在可以直接 hardware_project_scaffold → arduino_compile → arduino_upload 烧录 Arduino / ESP32。"
	} else {
		report.NextStep = "部分项安装失败,看每步 message 的手动指引;装好后重跑一键安装会自动跳过已成功项。"
	}
	return prettyJSON(report), nil
}
