package dsh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// 端到端门禁用例(需要真 node + 已装好的 dsh 组合包,故默认跳过):
//
//	ONECREAT_DSH_E2E=1 go test ./internal/engine/dsh/ -run Gate -v -count=1 -timeout 300s
//
// 为什么必须是**真** sidecar:门禁的另一半在 JS 里 —— dsh-tools 的调度器在派发之前
// 跑 `tools/pre-execute` waterfall,deny 时把 `Error: <reason>` 当工具结果回给模型、
// 工具**不执行**(node_modules/@deepseek-ai/dsh-tools/lib/index.js 的 prepareExecution)。
// bridge_test.go 那套假 sidecar 只能证明 Go 半边回了什么帧,证明不了"文件真的没被写"。
// dsh 是 developer preview、明说 rc 之间可能破坏兼容,所以每次升级都要由这几条自动
// 复证门禁语义没变(见 dsh/README.md 的升级必过项)。
//
// 网关红线:这里的"网关"是 httptest 假服务器,token 是字面量 t1,不需要也不使用任何
// 真实凭证;断言与日志都不打印请求头。

// gateHarness 是一条门禁用例的全部现场:假网关 + 真 sidecar 引擎 + 各回调的观测点。
type gateHarness struct {
	t   *testing.T
	eng *Engine
	cwd string
	// target 是模型将要写的绝对路径(<cwd>/gate.txt)。
	target string

	mu     sync.Mutex
	bodies []string
	// preEdits 记录每次 PreEdit 回调:工具名、原始参数、以及**调用当时**目标文件存不存在。
	preEdits []preEditRecord
	// approvals 记录每次审批询问的 (toolName, subject)。
	approvals [][2]string
}

type preEditRecord struct {
	name        string
	args        string
	targetExist bool
}

// newGateHarness 起假网关 + 引擎。opts 里只需要填门禁三件套(Decide/Approver/PreEdit),
// 其余连接事实由本函数补齐。
//
// 假网关逻辑:第 1 个请求回一条 write 工具调用(目标 = <cwd>/gate.txt 的绝对路径),
// 之后的请求一律回纯文本 + finish_reason:"stop" —— 否则 agent 不收敛。
func newGateHarness(t *testing.T, fill func(h *gateHarness, o *Options)) *gateHarness {
	t.Helper()
	if os.Getenv("ONECREAT_DSH_E2E") == "" {
		t.Skip("需要 ONECREAT_DSH_E2E=1(要真 node + `pnpm -C dsh install`)")
	}
	// macOS 上 /tmp 是 /private/tmp 的符号链接:先解开,免得断言里两边坐标系不一致。
	cwd := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	h := &gateHarness{t: t, cwd: cwd, target: filepath.Join(cwd, "gate.txt")}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		first := len(h.bodies) == 0
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if first {
			args, _ := json.Marshal(map[string]string{"file_path": h.target, "content": "hi"})
			call, _ := json.Marshal(map[string]any{
				"id": "1", "object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role": "assistant",
						"tool_calls": []any{map[string]any{
							"index": 0, "id": "call_1", "type": "function",
							"function": map[string]any{"name": "write", "arguments": string(args)},
						}},
					},
					"finish_reason": nil,
				}},
			})
			fmt.Fprintf(w, "data: %s\n\n", call)
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"好的\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")
	t.Setenv(gatewayTierEnv, "tier-2")
	opts := Options{
		Gateway:     true,
		Cfg:         config.DSHConfig{ModelPlaceholder: "onecreat"},
		CWD:         cwd,
		Sink:        event.Discard,
		Ledger:      testRecorder(evidence.NewLedger()),
		SessionRoot: t.TempDir(),
		BaseURL:     srv.URL + "/v1",
		APIKeyFunc:  func() string { return os.Getenv("ONECREAT_GATEWAY_TOKEN") },
		// 默认记录 PreEdit;用例可以在 fill 里覆盖。
		PreEdit: h.recordPreEdit,
	}
	if fill != nil {
		fill(h, &opts)
	}
	eng, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	h.eng = eng
	return h
}

// recordPreEdit 是 PreEdit 的观测实现。它顺手记下"此刻目标文件存不存在" —— 这是
// "快照发生在写之前"的无 sleep 证明(靠时间等待会是不稳定测试)。
func (h *gateHarness) recordPreEdit(name string, args json.RawMessage) {
	_, err := os.Stat(h.target)
	h.mu.Lock()
	h.preEdits = append(h.preEdits, preEditRecord{name: name, args: string(args), targetExist: err == nil})
	h.mu.Unlock()
}

// approve 造一个记录询问内容并返回固定答复的 Approver。
func (h *gateHarness) approve(allow bool) Approver {
	return func(_ context.Context, toolName, subject string) (bool, bool, error) {
		h.mu.Lock()
		h.approvals = append(h.approvals, [2]string{toolName, subject})
		h.mu.Unlock()
		return allow, false, nil
	}
}

// run 跑一轮,并把假网关看到的请求条数记进日志(不打印请求头)。
func (h *gateHarness) run(prompt string) {
	h.t.Helper()
	if err := h.eng.Run(context.Background(), prompt); err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.t.Logf("上游共收到 %d 个请求", len(h.bodies))
}

// lastToolMessages 取最后一个请求体里所有 role=="tool" 的消息内容(拼成一串)。
// 门禁 deny 的证据就在这里:模型收到的是 `Error: <reason>`,而不是工具输出。
func (h *gateHarness) lastToolMessages() string {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) < 2 {
		h.t.Fatalf("上游只收到 %d 个请求(模型没拿到工具结果就收敛了)", len(h.bodies))
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	last := h.bodies[len(h.bodies)-1]
	if err := json.Unmarshal([]byte(last), &req); err != nil {
		h.t.Fatalf("解请求体失败: %v", err)
	}
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			sb.WriteString(s)
		} else {
			sb.Write(m.Content)
		}
		sb.WriteByte('\n')
	}
	if sb.Len() == 0 {
		h.t.Fatalf("最后一个请求体里没有 role=\"tool\" 消息")
	}
	return sb.String()
}

// requireNoTarget 断言目标文件没被创建 —— 门禁的最终事实。
func (h *gateHarness) requireNoTarget() {
	h.t.Helper()
	if _, err := os.Stat(h.target); err == nil {
		h.t.Fatalf("门禁失效:%s 竟然被写出来了", filepath.Base(h.target))
	}
}

// snapshot 取观测点的副本。
func (h *gateHarness) snapshot() ([]preEditRecord, [][2]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]preEditRecord(nil), h.preEdits...), append([][2]string(nil), h.approvals...)
}

// TestGateDenyBlocksWriteE2E:Decide 恒回 deny → 真 sidecar 不派发 write,
// 模型收到 Error + 原因,PreEdit 不被调用(被拒的调用不该留下 rewind 点)。
func TestGateDenyBlocksWriteE2E(t *testing.T) {
	const reason = "被权限策略拒绝(deny 名单)—— 不要重试"
	h := newGateHarness(t, func(_ *gateHarness, o *Options) {
		o.Decide = func(string, json.RawMessage) (string, string) { return DecisionDeny, reason }
	})
	h.run("请把 gate.txt 写出来")

	h.requireNoTarget()
	msgs := h.lastToolMessages()
	t.Logf("模型收到的工具消息: %s", strings.TrimSpace(msgs))
	if !strings.Contains(msgs, "Error:") || !strings.Contains(msgs, reason) {
		t.Fatalf("工具结果里没有 deny 原因: %s", msgs)
	}
	pre, _ := h.snapshot()
	if len(pre) != 0 {
		t.Fatalf("被拒的调用不该做检查点快照,却调了 PreEdit %d 次: %+v", len(pre), pre)
	}
}

// TestGateAskApproveWritesE2E:Decide 回 ask、用户批准 → 审批卡片拿到正确的
// 工具名与对象;PreEdit 在文件出现**之前**被调用;文件最终落地。
func TestGateAskApproveWritesE2E(t *testing.T) {
	h := newGateHarness(t, func(h *gateHarness, o *Options) {
		o.Decide = func(string, json.RawMessage) (string, string) { return DecisionAsk, "" }
		o.Approver = h.approve(true)
	})
	h.run("请把 gate.txt 写出来")

	pre, asks := h.snapshot()
	if len(asks) == 0 {
		t.Fatal("Decide 回了 ask,却没弹审批")
	}
	t.Logf("审批询问: tool=%s subject=%s", asks[0][0], asks[0][1])
	if asks[0][0] != "write" {
		t.Fatalf("审批拿到的工具名不对: %q", asks[0][0])
	}
	if asks[0][1] != h.target {
		t.Fatalf("审批拿到的对象不对: %q(期望 %q)", asks[0][1], h.target)
	}
	if len(pre) == 0 {
		t.Fatal("放行前没有调 PreEdit(文件级 rewind 会拿不到快照)")
	}
	t.Logf("PreEdit: name=%s 目标当时存在=%v args=%s", pre[0].name, pre[0].targetExist, pre[0].args)
	if pre[0].name != "write" {
		t.Fatalf("PreEdit 拿到的工具名不对: %q", pre[0].name)
	}
	if !strings.Contains(pre[0].args, "gate.txt") {
		t.Fatalf("PreEdit 拿到的参数里没有目标路径: %s", pre[0].args)
	}
	if pre[0].targetExist {
		t.Fatal("PreEdit 是在文件写出来之后才调的 —— 快照到的是新内容,rewind 会还原不回去")
	}
	data, err := os.ReadFile(h.target)
	if err != nil {
		t.Fatalf("批准之后文件仍没落地: %v", err)
	}
	if string(data) != "hi" {
		t.Fatalf("文件内容不对: %q", string(data))
	}
}

// TestGateAskRejectBlocksWriteE2E:用户拒绝 → 文件不落地,模型被明确告知是"用户拒绝"。
func TestGateAskRejectBlocksWriteE2E(t *testing.T) {
	h := newGateHarness(t, func(h *gateHarness, o *Options) {
		o.Decide = func(string, json.RawMessage) (string, string) { return DecisionAsk, "" }
		o.Approver = h.approve(false)
	})
	h.run("请把 gate.txt 写出来")

	h.requireNoTarget()
	msgs := h.lastToolMessages()
	t.Logf("模型收到的工具消息: %s", strings.TrimSpace(msgs))
	if !strings.Contains(msgs, "用户拒绝") {
		t.Fatalf("工具结果里没说是用户拒绝: %s", msgs)
	}
	pre, asks := h.snapshot()
	if len(asks) == 0 {
		t.Fatal("没弹审批")
	}
	if len(pre) != 0 {
		t.Fatalf("被拒的调用不该做检查点快照,却调了 PreEdit %d 次", len(pre))
	}
}

// TestGatePlanModeDeniesWriteE2E:计划模式是 Go 侧硬门 —— 非只读工具一律拒。
//
// Decide 这里**镜像** boot.dshDecider 的计划模式分支,而不是直接调它:internal/boot
// 依赖本包,测试反向 import 会成环。dshDecider 本身的行为由 internal/boot/dshengine_test.go
// 表驱动钉住,两边合起来才是完整证明。
func TestGatePlanModeDeniesWriteE2E(t *testing.T) {
	const reason = "计划模式:现在只调研与设计,不执行任何会修改环境的操作。"
	h := newGateHarness(t, func(_ *gateHarness, o *Options) {
		o.Decide = func(name string, _ json.RawMessage) (string, string) {
			if isReadOnlyName(name) {
				return DecisionAllow, ""
			}
			return DecisionDeny, reason
		}
	})
	h.run("请把 gate.txt 写出来")

	h.requireNoTarget()
	msgs := h.lastToolMessages()
	t.Logf("模型收到的工具消息: %s", strings.TrimSpace(msgs))
	if !strings.Contains(msgs, "计划模式") {
		t.Fatalf("工具结果里没有计划模式原因: %s", msgs)
	}
	pre, _ := h.snapshot()
	if len(pre) != 0 {
		t.Fatalf("计划模式下不该做检查点快照,却调了 PreEdit %d 次", len(pre))
	}
}

// TestGateFailClosedOnCancelE2E:门禁通道在等答复时 turn 被取消 —— 必须 fail-closed。
//
// JS 侧的 `ask` 挂在 exec.signal 上,turn 取消时它 reject,插件 catch 后返回
// {kind:'deny'}(dsh/plugins/control/index.js)。这条钉的就是那个 catch:安全闸门
// 在通道不可用时不能 fail-open。
func TestGateFailClosedOnCancelE2E(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newGateHarness(t, func(_ *gateHarness, o *Options) {
		o.Decide = func(string, json.RawMessage) (string, string) {
			once.Do(func() { close(reached) })
			// 一直阻塞到用例收尾:模拟"审批弹窗还开着,人没点"。
			<-release
			return DecisionDeny, "通道收尾"
		}
	})
	// 收尾时才放行,保证整轮期间 Go 侧始终没回过 preExecute.done。
	t.Cleanup(func() { close(release) })

	done := make(chan error, 1)
	go func() { done <- h.eng.Run(context.Background(), "请把 gate.txt 写出来") }()

	select {
	case <-reached:
	case <-time.After(120 * time.Second):
		t.Fatal("等门禁被触达超时(模型没发出 write 调用?)")
	}
	if err := h.eng.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case err := <-done:
		t.Logf("取消后 Run 返回: %v", err)
	case <-time.After(120 * time.Second):
		t.Fatal("取消后 Run 挂死了(fail-closed 没生效)")
	}
	h.requireNoTarget()
}
