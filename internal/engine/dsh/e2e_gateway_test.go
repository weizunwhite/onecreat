package dsh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// 端到端(需要真 node + 已装好的 dsh 组合包,故默认跳过):
//
//	ONECREAT_DSH_E2E=1 go test ./internal/engine/dsh/ -run TestGatewayTierAndTokenRotation -v
//
// 它拉起**真的** sidecar 打一个假的 OpenAI 兼容网关,校两件事:
//  1. 上游收到的 model 是用户选中的档位(tier-2),不是占位符;
//  2. 两轮之间轮换登录 token 后,第二轮请求头用的是新 token —— 这条只有
//     dsh/plugins/control 的 onecreat/credentials.set 真的生效才成立
//     (纯 Go 的 wire 单测用假 sidecar,盖不住 JS 那一半)。
func TestGatewayTierAndTokenRotation(t *testing.T) {
	if os.Getenv("ONECREAT_DSH_E2E") == "" {
		t.Skip("需要 ONECREAT_DSH_E2E=1(要真 node + `pnpm -C dsh install`)")
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("auth=%s model=%v", r.Header.Get("Authorization"), m["model"]))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t1")
	t.Setenv(gatewayTierEnv, "tier-2")
	eng, err := New(Options{
		Gateway: true,
		// RuntimeDir 留空:resolveRuntimeDir 从 cwd 逐级向上找仓库根的 dsh/。
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
	if err := eng.Run(ctx, "第一轮:只回一个 ok"); err != nil {
		t.Fatalf("round1: %v", err)
	}
	t.Setenv("ONECREAT_GATEWAY_TOKEN", "t2")
	if err := eng.Run(ctx, "第二轮:只回一个 ok"); err != nil {
		t.Fatalf("round2: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range seen {
		t.Logf("上游请求 %d: %s", i+1, s)
	}
	if len(seen) < 2 {
		t.Fatalf("上游只收到 %d 个请求", len(seen))
	}
	if seen[0] != "auth=Bearer t1 model=tier-2" {
		t.Fatalf("第一轮请求错: %s", seen[0])
	}
	if seen[len(seen)-1] != "auth=Bearer t2 model=tier-2" {
		t.Fatalf("第二轮没用上轮换后的 token: %s", seen[len(seen)-1])
	}
}
