package main

// MCP 抽屉的 transport facade:实现在 mcpService 里(见 mcp_service.go)。

// AddMCPServer 把一台 MCP 服务器写进配置,返回配置里现有服务器总数。
func (a *App) AddMCPServer(in MCPServerInput) (int, error) { return a.mcp.AddServer(in) }

// AddHardwareMCPServer 把本机的硬件 MCP 二进制登记成一台服务器。
func (a *App) AddHardwareMCPServer() (int, error) { return a.mcp.AddHardwareServer() }

// RemoveMCPServer 从配置里删掉一台服务器。
func (a *App) RemoveMCPServer(name string) error { return a.mcp.RemoveServer(name) }

// RetryMCPServer 重连一台失败的服务器。
func (a *App) RetryMCPServer(name string) error { return a.mcp.RetryServer(name) }

// SetMCPServerEnabled 在本会话里开关一台服务器。
func (a *App) SetMCPServerEnabled(name string, enabled bool) error {
	return a.mcp.SetServerEnabled(name, enabled)
}
