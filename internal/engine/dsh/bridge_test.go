package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/tool"
	_ "reasonix/internal/tool/builtin" // 注册 complete_step 等内置工具
)

// bridgeHarness 把一个 Engine 接到 net.Pipe 的一端,另一端由测试当"dsh sidecar"用:
// 测试可以往里推通知(模拟 dsh 发来的审批/工具/预执行请求),并读回 Go 侧的应答帧。
type bridgeHarness struct {
	eng     *Engine
	enc     *json.Encoder
	frames  chan rpcFrame
	cleanup func()
}

func newBridgeHarness(t *testing.T, opts Options) *bridgeHarness {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	eng, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.rpc = NewLineClient(clientConn, clientConn, eng.onNotify)
	eng.started = true

	h := &bridgeHarness{
		eng:    eng,
		enc:    json.NewEncoder(serverConn),
		frames: make(chan rpcFrame, 16),
	}
	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			var f rpcFrame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				continue
			}
			h.frames <- f
		}
	}()
	h.cleanup = func() { clientConn.Close(); serverConn.Close() }
	t.Cleanup(h.cleanup)
	return h
}

// push 模拟 sidecar 发来一条通知。
func (h *bridgeHarness) push(t *testing.T, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.enc.Encode(rpcRequest{JSONRPC: jsonrpcVersion, Method: method, Params: json.RawMessage(raw)}); err != nil {
		t.Fatalf("push: %v", err)
	}
}

// await 等 Go 侧回一帧指定方法的通知。
func (h *bridgeHarness) await(t *testing.T, method string) json.RawMessage {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-h.frames:
			if f.Method == method {
				return f.Params
			}
		case <-deadline:
			t.Fatalf("等 %s 应答超时", method)
		}
	}
}

// 审批桥:dsh 发来审批请求 → Go 侧问 Approver → 回一帧 approval.resolve。
func TestApprovalBridge(t *testing.T) {
	asked := make(chan string, 1)
	h := newBridgeHarness(t, Options{
		Ledger: evidence.NewLedger(),
		Approver: func(_ context.Context, toolName, subject string) (bool, bool, error) {
			asked <- toolName
			return true, false, nil
		},
	})
	h.push(t, NotifyApprovalRequest, ApprovalRequestNotification{ID: "a1", SessionID: "s1", ToolName: "bash", Reason: "会改环境"})
	select {
	case name := <-asked:
		if name != "bash" {
			t.Fatalf("审批工具名错误: %s", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Approver 未被调用")
	}
	var reply ApprovalResolveNotification
	if err := json.Unmarshal(h.await(t, NotifyApprovalResolve), &reply); err != nil {
		t.Fatalf("解析应答: %v", err)
	}
	if reply.ID != "a1" || !reply.Allow {
		t.Fatalf("审批应答错误: %+v", reply)
	}
}

// 非交互(headless)下没有 Approver:与 native 的 permission.Gate 一致,Ask 放行。
func TestPreExecuteAskWithoutApproverAllows(t *testing.T) {
	h := newBridgeHarness(t, Options{
		Ledger: evidence.NewLedger(),
		Decide: func(string, json.RawMessage) (string, string) { return DecisionAsk, "" },
	})
	h.push(t, NotifyToolPreExecute, ToolPreExecuteNotification{ID: "p1", Name: "bash", Arguments: `{"command":"ls"}`})
	var dec PreExecuteDecision
	if err := json.Unmarshal(h.await(t, NotifyToolPreExecuteDone), &dec); err != nil {
		t.Fatalf("解析裁定: %v", err)
	}
	if dec.Decision != DecisionAllow {
		t.Fatalf("无 Approver 时 ask 应放行,得到 %+v", dec)
	}
}

// 权限策略的 deny 必须穿到 dsh 侧(否则 dsh 自己的工具绕过整套权限体系)。
func TestPreExecuteDenyPropagates(t *testing.T) {
	snapped := 0
	h := newBridgeHarness(t, Options{
		Ledger:  evidence.NewLedger(),
		Decide:  func(string, json.RawMessage) (string, string) { return DecisionDeny, "在 deny 名单上" },
		PreEdit: func(string, json.RawMessage) { snapped++ },
	})
	h.push(t, NotifyToolPreExecute, ToolPreExecuteNotification{ID: "p2", Name: "bash", Arguments: `{"command":"rm -rf /"}`})
	var dec PreExecuteDecision
	if err := json.Unmarshal(h.await(t, NotifyToolPreExecuteDone), &dec); err != nil {
		t.Fatalf("解析裁定: %v", err)
	}
	if dec.Decision != DecisionDeny || dec.Reason == "" {
		t.Fatalf("deny 未穿透: %+v", dec)
	}
	if snapped != 0 {
		t.Fatalf("被拒的调用不应做文件快照(白占 checkpoint),snapped=%d", snapped)
	}
}

// 工具桥 + 证据引擎:谎报"烧录成功"必须被判未完成 —— 本轮账本里没有任何设备动作。
func TestToolBridgeRejectsFabricatedFlash(t *testing.T) {
	ledger := evidence.NewLedger()
	h := newBridgeHarness(t, Options{
		Ledger: ledger,
		Tools: func(name string) (tool.Tool, bool) {
			for _, t := range tool.Builtins() {
				if t.Name() == name {
					return t, true
				}
			}
			return nil, false
		},
	})
	args := `{"step":"烧录固件到 ESP32","result":"固件已烧录到开发板并运行","evidence":[{"kind":"verification","summary":"烧录成功,串口输出 led:on","command":"arduino-cli upload -b esp32:esp32:esp32"}]}`
	h.push(t, NotifyToolInvoke, ToolInvokeNotification{ID: "t1", Name: "complete_step", Arguments: args})
	var res ToolResultNotification
	if err := json.Unmarshal(h.await(t, NotifyToolResult), &res); err != nil {
		t.Fatalf("解析工具结果: %v", err)
	}
	if res.Error == "" {
		t.Fatalf("谎报烧录竟然被签收了:%q", res.Output)
	}
	t.Logf("证据引擎拒绝:%s", res.Error)

	// 真做过一次成功的烧录之后,同一条签收就应该通过。
	ledger.Record(evidence.Receipt{ToolName: "mcp__hardware__arduino_upload", Success: true})
	ledger.Record(evidence.Receipt{ToolName: "bash", Success: true, Command: "arduino-cli upload -b esp32:esp32:esp32"})
	h.push(t, NotifyToolInvoke, ToolInvokeNotification{ID: "t2", Name: "complete_step", Arguments: args})
	var res2 ToolResultNotification
	if err := json.Unmarshal(h.await(t, NotifyToolResult), &res2); err != nil {
		t.Fatalf("解析工具结果: %v", err)
	}
	if res2.Error != "" {
		t.Fatalf("真烧录后仍被拒: %s", res2.Error)
	}
	if !strings.Contains(res2.Output, "已签收") {
		t.Fatalf("签收回执异常: %q", res2.Output)
	}
}

// 证据喂料:dsh 的 tool/call + tool/result + todo/write 必须进 Go 侧账本。
func TestConsumeFeedsLedger(t *testing.T) {
	ledger := evidence.NewLedger()
	eng, err := New(Options{Ledger: ledger, Sink: event.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	call, _ := json.Marshal(map[string]any{"callId": "c1", "name": "bash", "arguments": `{"command":"go test ./..."}`})
	eng.consume(RawSessionEvent{Type: EvToolCall, Data: call})
	result, _ := json.Marshal(map[string]any{
		"message": map[string]any{"content": []map[string]any{{
			"type": "tool-result", "toolCallId": "c1",
			"content": []map[string]string{{"type": "text", "text": "ok"}},
		}}},
	})
	eng.consume(RawSessionEvent{Type: EvToolResult, Data: result})
	if !ledger.HasSuccessfulCommand("go test ./...") {
		t.Fatal("dsh 的工具事件没有喂进证据账本")
	}
	todos, _ := json.Marshal(map[string]any{"todos": []map[string]string{
		{"content": "编译固件", "status": "completed"},
	}})
	eng.consume(RawSessionEvent{Type: EvTodoWrite, Data: todos})
	if items, ok := ledger.LatestTodos(); !ok || len(items) != 1 || items[0].Content != "编译固件" {
		t.Fatalf("todo/write 没有喂进账本: %v %v", items, ok)
	}
}
