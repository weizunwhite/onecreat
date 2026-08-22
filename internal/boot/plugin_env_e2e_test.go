package boot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"reasonix/internal/plugin"
	"reasonix/internal/workspace"
)

// TestEnvReportHelper 是一个最小 MCP stdio 服务器,唯一的本事是把它**子进程里**看到的
// C1_MCP_KEY 当成工具名报出来 —— 工具名会进注册表,父进程用 ctrl.ToolNames() 就能读到,
// 不必再调用一次工具。用测试二进制自己当这个服务器(经 -test.run 分派),与
// internal/plugin 里既有的 TestHelperProcess 同一套路。
//
// 为什么要真起进程:BaseEnv 是不是被填上了,读一下结构体字段就知道 —— 但那证明不了
// 子进程**看见**了什么。而这次的 bug 恰恰是「字段没填上,于是子进程看的是进程环境」,
// 只有真跑一个进程才分得清。
func TestEnvReportHelper(t *testing.T) {
	if os.Getenv("C1_MCP_HELPER") != "1" {
		return
	}
	defer os.Exit(0)

	in := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "envreport", "version": "0"},
				"capabilities":    map[string]any{},
			}
		case "tools/list":
			// 工具名带上它看到的值。看不到就是 "saw_" —— 一眼能分出「没拿到」和「拿错了」。
			result = map[string]any{"tools": []map[string]any{{
				"name":        "saw_" + os.Getenv("C1_MCP_KEY"),
				"description": "reports the C1_MCP_KEY this subprocess can see",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		default:
			result = map[string]any{}
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

// C1 的端到端:一个**在装配后期才被追加**的 MCP 服务器(这里用 ExtraPlugins 代表
// codegraph 与 ACP 那两类),它的子进程必须看得到本工作区 `.env` 的值。
//
// 回归前:`PluginSpecsIn` 只覆盖配置里的插件,后期追加的 spec BaseEnv 为 nil,退回
// 进程环境 —— 而 C1 之后 `.env` 已经不在进程环境里,于是这个服务器静默地什么也看不到。
func TestLateAddedMCPServerSeesTheWorkspaceDotEnv(t *testing.T) {
	os.Unsetenv("C1_MCP_KEY")
	t.Chdir(t.TempDir())

	dir := t.TempDir()
	projectConfig(t, dir, "PROMPT", "rule")
	writeFile(t, dir, ".env", "C1_MCP_KEY=fromworkspace\n")
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctrl, err := Build(ctx, Options{
		Workspace: ws,
		ExtraPlugins: []plugin.Spec{{
			Name:    "envreport",
			Command: os.Args[0],
			Args:    []string{"-test.run=TestEnvReportHelper", "--"},
			Env:     map[string]string{"C1_MCP_HELPER": "1"},
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ctrl.Close()

	// 服务器把它看到的值编进了工具名,于是注册表里就带着答案。
	names := ctrl.ToolNames()
	want := "mcp__envreport__saw_fromworkspace"
	for _, n := range names {
		if n == want {
			return
		}
	}
	var got []string
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__envreport__") {
			got = append(got, n)
		}
	}
	t.Fatalf("后期追加的 MCP 服务器报出的是 %v(want %s)—— 它没拿到本工作区的 .env,静默退回了进程环境", got, want)
}
