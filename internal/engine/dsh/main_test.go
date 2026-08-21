package dsh

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// fakeSidecarFlag 让这个测试二进制可以把自己当成一个 dsh sidecar 跑起来。
//
// 复核对 dsh 的验收写得很明确:「不允许只编译 adapter」。用手搓的结构体驱动
// `turn`/`Adapter` 只能测到我**以为**协议长什么样;真正拉起一个进程、走完
// stdio JSON-RPC 的 initialize → prompt → 通知 → turn/end,才算测到接线本身。
//
// 走命令行参数而不是环境变量,是因为 childEnv 现在是白名单(AR-R06),测试专用的
// 环境变量根本传不进子进程 —— 这一点本身也说明那个白名单是真的在起作用。
const fakeSidecarFlag = "--fake-dsh-sidecar"

// hangSentinel 让假 sidecar 收下这一轮但永不结束它,用来测取消与终结语义。
const hangSentinel = "HANG"

// promptContains 看 session/prompt 的文本里有没有某个标记。
func promptContains(params json.RawMessage, want string) bool {
	var p SessionPromptParams
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	for _, b := range p.ContentBlocks {
		if strings.Contains(b.Text, want) {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	// 必须在 m.Run() 之前拦截:testing 在 Run 里解析 flag,我们这个参数它不认识。
	if len(os.Args) > 1 && os.Args[1] == fakeSidecarFlag {
		runFakeSidecar()
		return
	}
	goleak.VerifyTestMain(m)
}

// runFakeSidecar 说最少的那部分 dsh 协议:握手、接 prompt、推一轮事件、关。
func runFakeSidecar() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	out := json.NewEncoder(os.Stdout)

	notify := func(evType string, data any) {
		raw, _ := json.Marshal(data)
		ev, _ := json.Marshal(SessionEventNotification{
			SessionID: "s1",
			Event:     RawSessionEvent{Type: evType, Data: raw},
		})
		_ = out.Encode(rpcRequest{JSONRPC: jsonrpcVersion, Method: NotifySessionEvent, Params: json.RawMessage(ev)})
	}

	for in.Scan() {
		var req rpcFrame
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case MethodInitialize:
			res, _ := json.Marshal(map[string]any{
				"serverInfo": map[string]string{"name": WireServerName, "version": "0.1.0-rc.7"},
			})
			_ = out.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID, Result: res})

		case MethodSessionPrompt:
			res, _ := json.Marshal(map[string]string{"messageId": "m1"})
			_ = out.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID, Result: res})
			notify(EvTurnStart, map[string]any{})
			notify(EvAssistantChunk, map[string]any{"chunk": map[string]string{"type": "text-delta", "text": "你好"}})
			// 输入里带 hangSentinel 时**不发 turn/end**:模拟一轮还在跑。
			// 没有这个开关就没法测取消 —— 这个假 sidecar 原本回得太快,
			// Cancel 到达时那一轮早就正常收工了(而"已收工就不该杀进程"恰好是对的)。
			if promptContains(req.Params, hangSentinel) {
				continue
			}
			notify(EvAssistantMsg, map[string]any{
				"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "你好"}}},
			})
			notify(EvTurnEnd, map[string]any{})

		case MethodShutdown:
			_ = out.Encode(rpcFrame{JSONRPC: jsonrpcVersion, ID: req.ID, Result: json.RawMessage("{}")})
			fmt.Fprintln(os.Stderr, "fake sidecar: shutdown")
			return
		}
	}
}
