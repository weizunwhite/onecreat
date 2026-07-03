package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reasonix/internal/provider"
)

// TestIsRetryableStatus covers the boundary: 408/429/5xx retry, other 4xx don't.
func TestIsRetryableStatus(t *testing.T) {
	retry := []int{408, 429, 500, 502, 503, 504, 599}
	noRetry := []int{200, 400, 401, 403, 404, 422}
	for _, s := range retry {
		if !isRetryableStatus(s) {
			t.Errorf("status %d should be retryable", s)
		}
	}
	for _, s := range noRetry {
		if isRetryableStatus(s) {
			t.Errorf("status %d should not be retryable", s)
		}
	}
}

// TestIsTransientErr keeps user-intent errors (ctx cancel / deadline) out of
// the retry path while letting network-level failures through.
func TestIsTransientErr(t *testing.T) {
	if isTransientErr(nil) {
		t.Error("nil error should not be transient")
	}
	if isTransientErr(context.Canceled) {
		t.Error("ctx canceled should not be transient")
	}
	if isTransientErr(context.DeadlineExceeded) {
		t.Error("ctx deadline should not be transient")
	}
	if !isTransientErr(errors.New("connection reset")) {
		t.Error("generic network-ish error should be transient")
	}
}

// TestStreamAuthError verifies a 401 surfaces as an actionable *provider.AuthError
// (naming the provider and its key env var) rather than a raw status body.
func TestStreamAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails, Your api key: ****ae54 is invalid"}}`))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name:    "deepseek",
		BaseURL: srv.URL,
		Model:   "deepseek-v4",
		APIKey:  "bad",
		Extra:   map[string]any{"api_key_env": "DEEPSEEK_API_KEY"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var authErr *provider.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("want *provider.AuthError, got %T: %v", err, err)
	}
	if authErr.Provider != "deepseek" || authErr.KeyEnv != "DEEPSEEK_API_KEY" || authErr.Status != 401 {
		t.Errorf("AuthError fields wrong: %+v", authErr)
	}
	if msg := authErr.Error(); !strings.Contains(msg, "DEEPSEEK_API_KEY") || strings.Contains(msg, "ae54") {
		t.Errorf("message should name the env var and not dump the raw body: %q", msg)
	}
}

func TestStreamGatewayAuthErrorUsesPlatformMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"登录已失效,请在 onecreat 重新登录","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name:    "onecreat",
		BaseURL: srv.URL,
		Model:   "tier-1",
		APIKey:  "expired",
		Extra:   map[string]any{"api_key_env": "ONECREAT_GATEWAY_TOKEN"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want gateway auth error")
	}
	if got := err.Error(); !strings.Contains(got, "登录已失效") || strings.Contains(got, "ONECREAT_GATEWAY_TOKEN") {
		t.Fatalf("gateway auth error should use platform message without env leak: %q", got)
	}
}

// B1 回归:网关模式上游 400 body 含 "for model deepseek-…",返回的 error 必须只剩平台话术+
// 状态码,绝不含 deepseek/model 原文(否则击穿档位抽象、违反 ModelPrivacyPolicy)。
func TestStreamGatewayHTTPErrorRedactsUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded for model deepseek-v4-pro"}}`))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name: "onecreat", BaseURL: srv.URL, Model: "tier-1", APIKey: "tok",
		Extra: map[string]any{"api_key_env": "ONECREAT_GATEWAY_TOKEN"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want gateway HTTP error")
	}
	got := err.Error()
	if strings.Contains(strings.ToLower(got), "deepseek") || strings.Contains(got, "for model") {
		t.Fatalf("网关错误泄露了上游 body/模型名: %q", got)
	}
	if !strings.Contains(got, "400") {
		t.Fatalf("网关错误应保留状态码 400: %q", got)
	}
}

// B1:非网关(用户自带 key 直连)保持现状——透传上游 body 原文,方便自行排障。
func TestStreamNonGatewayHTTPErrorPassesThroughBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request for model deepseek-v4-pro"}}`))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name: "deepseek", BaseURL: srv.URL, Model: "deepseek-v4-pro", APIKey: "tok",
		Extra: map[string]any{"api_key_env": "DEEPSEEK_API_KEY"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want HTTP error")
	}
	if !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("非网关应透传上游 body(含模型名),got %q", err.Error())
	}
}

// B1 回归:网关模式流中 error 帧 message 含 deepseek → ChunkError 必须脱敏成通用话术。
func TestStreamGatewayMidStreamErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"rate limited for model deepseek-v4-pro\"}}\n\n"))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name: "onecreat", BaseURL: srv.URL, Model: "tier-1", APIKey: "tok",
		Extra: map[string]any{"api_key_env": "ONECREAT_GATEWAY_TOKEN"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var gotErr error
	for chunk := range ch {
		if chunk.Type == provider.ChunkError {
			gotErr = chunk.Err
		}
	}
	if gotErr == nil {
		t.Fatal("want a mid-stream ChunkError")
	}
	if strings.Contains(strings.ToLower(gotErr.Error()), "deepseek") {
		t.Fatalf("流中错误泄露了模型名: %q", gotErr.Error())
	}
}

// B1:状态码映射本身不得含任何厂商/模型字样,且保留状态码线索。
func TestGatewayStatusMessageNoVendorText(t *testing.T) {
	for _, s := range []int{400, 429, 500, 503, 404} {
		msg := gatewayStatusMessage(s)
		if strings.Contains(strings.ToLower(msg), "deepseek") || strings.Contains(strings.ToLower(msg), "model") {
			t.Fatalf("gatewayStatusMessage(%d)=%q 含厂商/模型字样", s, msg)
		}
	}
	if !strings.Contains(gatewayStatusMessage(429), "429") {
		t.Fatalf("429 应保留状态码: %q", gatewayStatusMessage(429))
	}
	if !strings.Contains(gatewayStatusMessage(500), "5xx") {
		t.Fatalf("5xx 应有 5xx 提示: %q", gatewayStatusMessage(500))
	}
}

// B2 回归:上游把大 reasoning/大 tool_call arguments 作为单个 ~2MB data: 帧下发时,scanner
// 不得报 bufio.ErrTooLong 整轮失败。缓冲上限提到 32MB 后应正常解析。
func TestReadStreamLargeFrameNoErrTooLong(t *testing.T) {
	big := strings.Repeat("A", 2*1024*1024) // ~2MB 单帧文本增量,远超旧 1MB 上限
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"` + big + `"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p, err := New(provider.Config{
		Name: "deepseek", BaseURL: srv.URL, Model: "deepseek-v4", APIKey: "tok",
		Extra: map[string]any{"api_key_env": "DEEPSEEK_API_KEY"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
		case provider.ChunkError:
			t.Fatalf("大帧不应报错(ErrTooLong 回归?): %v", chunk.Err)
		}
	}
	if text.Len() != len(big) {
		t.Fatalf("解析出的文本长度 = %d, want %d(大帧被截断/丢弃)", text.Len(), len(big))
	}
}

// TestBuildRequestAlwaysSerializesContent guards the DeepSeek 400 regression:
// an assistant turn that is pure tool_calls (no preamble text) has empty
// content, and DeepSeek rejects a message missing the `content` field. Every
// message — including that one — must serialize a content field.
func TestBuildRequestAlwaysSerializesContent(t *testing.T) {
	c := &client{model: "deepseek-v4"}
	req := c.buildRequest(provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "list the files"},
			// Assistant turn with no text, only a tool call — the offending shape.
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "ls", Arguments: `{"path":"."}`},
			}},
			{Role: provider.RoleTool, Content: "main.go", ToolCallID: "call_1", Name: "ls"},
		},
	})

	b, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Decode generically so we can assert the key's presence (not just its value).
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, m := range raw {
		if _, ok := m["content"]; !ok {
			t.Errorf("messages[%d] is missing the content field: %s", i, b)
		}
	}
	// The tool-call-only assistant message must carry content:"" and its tool_calls.
	if got := string(raw[1]["content"]); got != `""` {
		t.Errorf("assistant content = %s, want \"\"", got)
	}
	if _, ok := raw[1]["tool_calls"]; !ok {
		t.Errorf("assistant message lost its tool_calls: %s", b)
	}
}

// TestStreamRepairsDanglingToolCalls reproduces and guards the DeepSeek 400
// "An assistant message with 'tool_calls' must be followed by tool messages
// responding to each 'tool_call_id'". A resumed/interrupted session can carry an
// assistant tool_calls turn whose tool results never landed; the server here
// mimics DeepSeek and rejects any unpaired tool_call with that exact 400, so the
// request must be repaired before it is sent.
func TestStreamRepairsDanglingToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		answered := map[string]bool{}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				answered[m.ToolCallID] = true
			}
		}
		for _, m := range req.Messages {
			if m.Role != "assistant" {
				continue
			}
			for _, tc := range m.ToolCalls {
				if !answered[tc.ID] {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"message":"An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'. (insufficient tool messages following tool_calls message)","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`))
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p, err := New(provider.Config{Name: "deepseek-flash", BaseURL: srv.URL, Model: "deepseek-v4", APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An assistant tool_calls turn whose tool result never landed (an interrupted
	// turn), followed by a fresh user message — the exact shape that 400s.
	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "list the files"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "ls", Arguments: `{"path":"."}`},
			}},
			{Role: provider.RoleUser, Content: "never mind, what time is it?"},
		},
	})
	if err != nil {
		t.Fatalf("Stream sent a dangling tool_calls to the API: %v", err)
	}
	var streamErr error
	var text strings.Builder
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
		case provider.ChunkError:
			streamErr = chunk.Err
		}
	}
	if streamErr != nil {
		t.Fatalf("stream errored: %v", streamErr)
	}
	if text.String() != "done" {
		t.Fatalf("completion text = %q, want \"done\"", text.String())
	}
}

// TestNormaliseUsageDeepSeekShape covers DeepSeek's top-level cache fields.
func TestNormaliseUsageDeepSeekShape(t *testing.T) {
	u := normaliseUsage(&wireUsage{
		PromptTokens:          1000,
		CompletionTokens:      200,
		TotalTokens:           1200,
		PromptCacheHitTokens:  900,
		PromptCacheMissTokens: 100,
	})
	if u.CacheHitTokens != 900 || u.CacheMissTokens != 100 {
		t.Errorf("DeepSeek-shape cache fields lost: hit=%d miss=%d", u.CacheHitTokens, u.CacheMissTokens)
	}
}

// TestNormaliseUsageMiMoShape covers the nested prompt_tokens_details /
// completion_tokens_details path used by OpenAI and MiMo. Miss is derived
// from prompt - hit when only hit is provided.
func TestNormaliseUsageMiMoShape(t *testing.T) {
	u := normaliseUsage(&wireUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 600},
		CompletionTokensDetails: &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: 180},
	})
	if u.CacheHitTokens != 600 || u.CacheMissTokens != 400 {
		t.Errorf("nested cache normalisation wrong: hit=%d miss=%d (want 600 / 400)", u.CacheHitTokens, u.CacheMissTokens)
	}
	if u.ReasoningTokens != 180 {
		t.Errorf("reasoning tokens lost: %d", u.ReasoningTokens)
	}
}

// TestBuildRequestDropsReasoningContent guards the cache/cost fix: an assistant
// turn's reasoning_content is a response-only signal and must never be echoed
// back in the outgoing request. DeepSeek otherwise counts it as paid prompt
// input (~500 tok/turn on a reasoner chain). The session keeps it for
// display/archive; the wire request must not carry it.
func TestBuildRequestDropsReasoningContent(t *testing.T) {
	c := &client{model: "deepseek-reasoner"}
	req := c.buildRequest(provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "explain"},
			{Role: provider.RoleAssistant, Content: "the answer", ReasoningContent: "SECRET-CHAIN-OF-THOUGHT"},
			{Role: provider.RoleUser, Content: "thanks"},
		},
	})
	b, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "reasoning_content") {
		t.Errorf("outgoing request must not carry a reasoning_content field: %s", b)
	}
	if strings.Contains(string(b), "SECRET-CHAIN-OF-THOUGHT") {
		t.Errorf("the assistant chain-of-thought leaked into the request: %s", b)
	}
	// The visible answer must survive — we only drop reasoning, not content.
	if !strings.Contains(string(b), "the answer") {
		t.Errorf("assistant content was dropped along with reasoning: %s", b)
	}
}

func TestBuildRequestForwardsReasoningEffort(t *testing.T) {
	c := &client{model: "mimo-v2", effort: "high"}
	if got := c.buildRequest(provider.Request{}).ReasoningEffort; got != "high" {
		t.Errorf("ReasoningEffort = %q, want high", got)
	}

	b, err := json.Marshal((&client{model: "deepseek-v4"}).buildRequest(provider.Request{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "reasoning_effort") {
		t.Errorf("empty effort must be omitted from the payload: %s", b)
	}
}

func TestBuildRequestDeepSeekThinking(t *testing.T) {
	for _, tc := range []struct {
		name          string
		effort        string
		wantThinking  string
		wantReasoning string
	}{
		{name: "high", effort: "high", wantThinking: "enabled", wantReasoning: "high"},
		{name: "max", effort: "max", wantThinking: "enabled", wantReasoning: "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := (&client{model: "deepseek-v4", deepseek: true, effort: tc.effort}).buildRequest(provider.Request{})
			if req.Thinking == nil || req.Thinking.Type != tc.wantThinking {
				t.Fatalf("Thinking = %+v, want %q", req.Thinking, tc.wantThinking)
			}
			if req.ReasoningEffort != tc.wantReasoning {
				t.Fatalf("ReasoningEffort = %q, want %q", req.ReasoningEffort, tc.wantReasoning)
			}
		})
	}
}

func TestBuildRequestNonDeepSeekOmitsThinking(t *testing.T) {
	req := (&client{model: "mimo-v2", effort: "high"}).buildRequest(provider.Request{})
	if req.Thinking != nil {
		t.Fatalf("non-DeepSeek request must not include thinking, got %+v", req.Thinking)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", req.ReasoningEffort)
	}
}

func TestNewDeepSeekThinkingDefaultsAndValidation(t *testing.T) {
	p, err := New(provider.Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := p.(*client)
	if !c.deepseek || c.effort != "high" {
		t.Fatalf("deepseek=%v effort=%q, want true/high", c.deepseek, c.effort)
	}

	p, err = New(provider.Config{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-v4", Extra: map[string]any{"effort": "max"}})
	if err != nil {
		t.Fatalf("New max: %v", err)
	}
	if got := p.(*client).effort; got != "max" {
		t.Fatalf("effort = %q, want max", got)
	}

	if _, err := New(provider.Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4", Extra: map[string]any{"effort": "medium"}}); err == nil {
		t.Fatal("New should reject invalid DeepSeek effort")
	}
	p, err = New(provider.Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4", Extra: map[string]any{"effort": "off"}})
	if err != nil {
		t.Fatalf("New should migrate retired effort=off, not reject it: %v", err)
	}
	if got := p.(*client).effort; got != "high" {
		t.Fatalf("retired effort=off should fall back to high, got %q", got)
	}
}

// TestNewGatewayTierKeepsThinking guards the onecreat gateway path: in tier mode the
// provider's BaseURL is the platform gateway (not *.deepseek.com) and the model is a
// "tier-N" placeholder, so neither DeepSeek check matches — yet the tier is backed by a
// DeepSeek thinking model upstream and must still send thinking + effort. Without this,
// every paying user silently drops to non-thinking mode.
func TestNewGatewayTierKeepsThinking(t *testing.T) {
	p, err := New(provider.Config{Name: "onecreat", BaseURL: "https://gateway.example.com/api/onecreat/v1", Model: "tier-2"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := p.(*client)
	if !c.deepseek {
		t.Fatalf("tier model over gateway must be treated as thinking-capable, deepseek=%v", c.deepseek)
	}
	if c.effort != "high" {
		t.Fatalf("tier model must default effort to high, got %q", c.effort)
	}
	req := c.buildRequest(provider.Request{})
	if req.Thinking == nil || req.Thinking.Type != "enabled" {
		t.Fatalf("tier request must enable thinking, got %+v", req.Thinking)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("tier request ReasoningEffort = %q, want high", req.ReasoningEffort)
	}
}

func TestNewReadsEffortFromConfig(t *testing.T) {
	p, err := New(provider.Config{
		Name:    "mimo",
		BaseURL: "https://api.example.com",
		Model:   "mimo-v2",
		Extra:   map[string]any{"effort": "medium"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.(*client).effort; got != "medium" {
		t.Errorf("effort = %q, want medium", got)
	}
}

// TestBuildRequestPreservesEmptyIDToolResults proves a multi-tool turn whose
// calls carry no id (some OpenAI-compatible gateways omit it, sending only the
// index) keeps every tool result through buildRequest. SanitizeToolPairing keys
// on tool_call_id, so empty ids collapse and all but the last result is dropped.
func TestBuildRequestPreservesEmptyIDToolResults(t *testing.T) {
	c := &client{model: "deepseek-v4"}
	req := c.buildRequest(provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "scan"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
				{ID: "", Name: "read_file", Arguments: `{"p":"a"}`},
				{ID: "", Name: "read_file", Arguments: `{"p":"b"}`},
			}},
			{Role: provider.RoleTool, ToolCallID: "", Name: "read_file", Content: "RESULT-A"},
			{Role: provider.RoleTool, ToolCallID: "", Name: "read_file", Content: "RESULT-B"},
		},
	})
	var toolContents []string
	for _, m := range req.Messages {
		if m.Role == string(provider.RoleTool) {
			toolContents = append(toolContents, m.Content)
		}
	}
	if len(toolContents) != 2 {
		t.Fatalf("want 2 tool results in request, got %d: %v", len(toolContents), toolContents)
	}
	if toolContents[0] == toolContents[1] {
		t.Errorf("tool results collapsed to %q — a result was dropped from the model's context", toolContents[0])
	}
}

// TestStreamSynthesizesMissingToolCallIDs covers a gateway that streams tool
// calls by index with no id (vLLM / llama.cpp do this). Each completed call must
// come back with a stable, distinct synthetic id so its result can pair back.
func TestStreamSynthesizesMissingToolCallIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{\"p\":\"a\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"read_file","arguments":"{\"p\":\"b\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p, err := New(provider.Config{Name: "local", BaseURL: srv.URL, Model: "qwen", APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var ids []string
	for chunk := range ch {
		if chunk.Type == provider.ChunkToolCall && chunk.ToolCall != nil {
			ids = append(ids, chunk.ToolCall.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %v", len(ids), ids)
	}
	if ids[0] == "" || ids[1] == "" {
		t.Errorf("a tool call came back with an empty id: %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("synthesized ids must be distinct, got %v", ids)
	}
}
