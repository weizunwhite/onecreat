package main

// 硬件面板的 transport facade:方法照旧挂在 *App 上(Wails 绑定 / Web RPC 都按平面
// 方法名分发),实现全在 hardwareService 里(见 hardware_service.go)。

// HardwareBoardList 返回注册表里的全部板卡,供面板下拉使用。
func (a *App) HardwareBoardList() []HardwareBoardSummary { return a.hw.BoardList() }

// HardwareMCP 报告本机硬件 MCP 二进制的可用与接入状态。
func (a *App) HardwareMCP() HardwareMCPView { return a.hw.MCP() }

// HardwareDetect 探测当前项目的工具链 / 板卡 / 设备。
func (a *App) HardwareDetect() HardwareDetectView { return a.hw.Detect() }

// HardwareInstallToolchain 安装 arduino-cli 与指定的板卡 core。
func (a *App) HardwareInstallToolchain(cores []string) HardwareInstallToolchainView {
	return a.hw.InstallToolchain(cores)
}

// HardwareInstallArduinoCLI 只装 arduino-cli 本体。
func (a *App) HardwareInstallArduinoCLI() HardwareInstallStepView { return a.hw.InstallArduinoCLI() }

// HardwareInstallCore 只装一个板卡 core。
func (a *App) HardwareInstallCore(core string) HardwareInstallStepView {
	return a.hw.InstallCore(core)
}

// HardwareEvidenceStatus 汇报当前项目的证据链完成度。
func (a *App) HardwareEvidenceStatus() HardwareEvidenceStatusView { return a.hw.EvidenceStatus() }

// HardwareEvidenceExport 把证据链导出成 Markdown,返回文件路径。
func (a *App) HardwareEvidenceExport(projectDir string) (string, error) {
	return a.hw.EvidenceExport(projectDir)
}

// HardwareBoardFacts 返回某块板卡的引脚 / 电平 / 平台 API 事实卡。
func (a *App) HardwareBoardFacts(board, platform string) HardwareBoardFactsView {
	return a.hw.BoardFacts(board, platform)
}

// HardwareValidate 编译校验当前项目。
func (a *App) HardwareValidate(input HardwareRunInput) HardwareRunResult {
	return a.hw.Validate(input)
}

// HardwareUpload 烧录到板卡。
func (a *App) HardwareUpload(input HardwareRunInput) HardwareRunResult { return a.hw.Upload(input) }

// HardwareOTAUpload 走 WiFi 的 OTA 烧录。
func (a *App) HardwareOTAUpload(input HardwareRunInput) HardwareRunResult {
	return a.hw.OTAUpload(input)
}

// HardwarePublishFirmware 发布固件到远端。
func (a *App) HardwarePublishFirmware(input HardwarePublishInput) HardwareRunResult {
	return a.hw.PublishFirmware(input)
}

// HardwareMonitor 采一段串口输出作为证据。
func (a *App) HardwareMonitor(input HardwareRunInput) HardwareRunResult {
	return a.hw.Monitor(input)
}
