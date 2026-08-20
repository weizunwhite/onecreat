package dsh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// 端到端(需要真 node + 已装好的 dsh 组合包,故默认跳过):
//
//	ONECREAT_DSH_E2E=1 go test ./internal/engine/dsh/ -run TestReasoningPassedBackOnPlainTurn -v
//
// 这条钉的是 dsh 0.1.0-rc.8 的**升级主要动机**:llm-deepseek 从"只在带工具调用的
// 轮回传 CoT"改成"每一个产生了 reasoning 的轮都回传 reasoning_content"
// (上游 commit 583894f7ae)。rc.7 下第二轮请求体里那条 assistant 消息**没有**
// reasoning_content —— 推理模型因此每轮都在丢自己的思考,是实打实的生成质量损失。
//
// 做法:假 OpenAI 兼容网关第一轮吐一段 reasoning_content + 一句正文(不带任何
// tool_calls),第二轮把收到的请求体存下来,断言里面那条 assistant 消息带着
// reasoning_content。
func TestReasoningPassedBackOnPlainTurn(t *testing.T) {
	if os.Getenv("ONECREAT_DSH_E2E") == "" {
		t.Skip("需要 ONECREAT_DSH_E2E=1(要真 node + `pnpm -C dsh install`)")
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// 先一段思考,再一句正文,**不带 tool_calls** —— rc.7 正是在这种轮丢 CoT。
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"先想一下这道题的边界条件\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")
	t.Setenv(gatewayTierEnv, "tier-2")
	eng, err := New(Options{
		Gateway:     true,
		Cfg:         config.DSHConfig{ModelPlaceholder: "onecreat"},
		CWD:         t.TempDir(),
		Sink:        event.Discard,
		Ledger:      evidence.NewLedger(),
		SessionRoot: t.TempDir(),
		BaseURL:     srv.URL + "/v1",
		APIKeyFunc:  func() string { return os.Getenv("ONECREAT_GATEWAY_TOKEN") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()
	if err := eng.Run(ctx, "第一轮"); err != nil {
		t.Fatalf("round1: %v", err)
	}
	if err := eng.Run(ctx, "第二轮"); err != nil {
		t.Fatalf("round2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("上游只收到 %d 个请求", len(bodies))
	}
	last := bodies[len(bodies)-1]
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(last), &req); err != nil {
		t.Fatalf("解第二轮请求体失败: %v", err)
	}
	found := false
	for _, m := range req.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) != 0 {
			continue
		}
		if strings.Contains(m.ReasoningContent, "边界条件") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮没把上一轮的 reasoning_content 传回(rc.8 的 CoT 每轮回传没生效)\n请求体: %s", last)
	}
}
