// Package openai implements the OpenAI-compatible /chat/completions provider.
// It self-registers under the "openai" kind, so DeepSeek, MiMo, and any other
// OpenAI-compatible endpoint are just config instances rather than code.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/netclient"
	"reasonix/internal/provider"
)

// toolCallStreamSeq makes synthesized tool-call ids unique across model responses.
// A gateway/backend that streams tool calls by index without ids (DeepSeek's own
// API sends ids) would otherwise get call_<index> restarting at 0 every response,
// and a UI that merges tool cards by id across the whole transcript would fold a
// later turn's call_0 into an earlier turn's card. Each readStream bumps this and
// prefixes its synthesized ids, so ids never collide across turns.
var toolCallStreamSeq atomic.Uint64

func init() {
	provider.Register("openai", New)
}

// New builds an OpenAI-compatible provider from a resolved config.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai: base_url is required for provider %q", cfg.Name)
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai: model is required for provider %q", cfg.Name)
	}
	name := cfg.Name
	if name == "" {
		name = "openai"
	}
	keyEnv, _ := cfg.Extra["api_key_env"].(string) // for actionable auth errors
	effort, _ := cfg.Extra["effort"].(string)
	deepseek := isDeepSeekBaseURL(cfg.BaseURL) || isDeepSeekModel(cfg.Model) || isOnecreatTierModel(cfg.Model)
	if deepseek {
		effort = strings.ToLower(strings.TrimSpace(effort))
		switch effort {
		case "", "off": // "off" is a retired level (disabled thinking); fall back to the default depth
			effort = "high"
		case "high", "max":
		default:
			return nil, fmt.Errorf("openai: provider %q uses DeepSeek thinking; effort must be high or max", name)
		}
	}
	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("openai: network: %w", err)
	}
	return &client{
		name:     name,
		apiKey:   cfg.APIKey,
		keyEnv:   keyEnv,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		model:    cfg.Model,
		deepseek: deepseek,
		effort:   effort,
		http:     httpClient,
	}, nil
}

func newHTTPClient(cfg provider.Config) (*http.Client, error) {
	spec, _ := cfg.Extra["proxy_spec"].(netclient.ProxySpec)
	return netclient.NewHTTPClient(spec, 0, netclient.TransportOptions{
		DialTimeout:           30 * time.Second,
		KeepAlive:             30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // models can think for a while before the first token
	})
}

type client struct {
	name     string
	apiKey   string
	keyEnv   string // api_key_env name, surfaced in auth errors
	baseURL  string
	model    string
	http     *http.Client
	deepseek bool
	effort   string // reasoning_effort forwarded to thinking-capable models; "" = omit
}

func (c *client) Name() string { return c.name }

// authKey 返回当前请求要用的 API key。keyEnv 非空时**每次请求都从环境变量读**(而不是用 build
// 时烤死的值):这样登录 token 续期 / 重新登录后(更新了 env)对所有已建好的 controller 立即
// 生效,无需重建——对 onecreat 网关 token 的自动续期是关键。静态 key(如 DEEPSEEK_API_KEY)
// 读 env 也是同一个值,无副作用。keyEnv 为空(直接给的 key)时用 build 时的值。
func (c *client) authKey() string {
	if c.keyEnv != "" {
		return os.Getenv(c.keyEnv)
	}
	return c.apiKey
}

func isDeepSeekBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "api.deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

// isDeepSeekModel reports whether the model is one of DeepSeek's own thinking-capable
// models by its native name (deepseek-v4-flash/pro, deepseek-reasoner, …). It matches
// the hyphen form only, so OpenRouter-namespaced ids like "deepseek/deepseek-chat" (a
// reseller's hosting, no DeepSeek thinking param) are deliberately excluded. This keeps
// DeepSeek thinking + effort enabled even when routed through a non-DeepSeek base URL
// (e.g. the onecreat platform AI gateway), so behavior matches a direct connection.
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek-")
}

// isOnecreatTierModel reports whether the model is a onecreat subscription-tier
// placeholder ("tier-1", "tier-2", …) that applyOnecreatGateway substitutes for the
// real model name in gateway mode. Every tier is backed by a DeepSeek thinking model
// upstream, so we must keep sending thinking + effort exactly as a direct DeepSeek
// connection would — otherwise paying users silently drop to non-thinking mode. The
// thinking param reveals nothing about which model backs the tier, so this does not
// violate the model-privacy policy.
func isOnecreatTierModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "tier-")
}

func (c *client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request: %w", c.name, err)
	}

	resp, err := c.sendWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}

	out := make(chan provider.Chunk)
	go c.readStream(ctx, resp, out)
	return out, nil
}

// sendWithRetry POSTs the request body and returns the streaming response,
// retrying on transient network errors and retryable HTTP statuses (408, 429,
// 5xx) with exponential backoff + jitter. Retries only cover the connection +
// header phase; once we hand the response to readStream, mid-stream failures
// surface as ChunkError without retry, since the model has already started
// emitting tokens we'd otherwise duplicate.
func (c *client) sendWithRetry(ctx context.Context, body []byte) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1))*500*time.Millisecond + time.Duration(rand.Intn(250))*time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", c.name, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.authKey())
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			if !isTransientErr(err) {
				return nil, fmt.Errorf("%s: request failed: %w", c.name, err)
			}
			lastErr = fmt.Errorf("%s: request failed: %w", c.name, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		msg, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			msg = []byte(fmt.Sprintf("(could not read error body: %v)", readErr))
		}
		// Drain any remaining body so the HTTP connection can be reused by the
		// transport pool, then close.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// A rejected key is a configuration problem, not a transient one — give
		// an actionable error instead of dumping the raw status body.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if c.keyEnv == "ONECREAT_GATEWAY_TOKEN" {
				if message := openAIErrorMessage(msg); message != "" {
					return nil, fmt.Errorf("%s: %s", c.name, message)
				}
				return nil, fmt.Errorf("%s: 登录已失效,请在 onecreat 重新登录", c.name)
			}
			return nil, &provider.AuthError{Provider: c.name, KeyEnv: c.keyEnv, Status: resp.StatusCode}
		}
		var statusErr error
		if c.isGateway() {
			// 网关模式:绝不把上游 body 原文拼进 error(400 的 "for model deepseek-…" 会泄露
			// 真实模型名),只映射成平台话术+状态码。
			statusErr = fmt.Errorf("%s: %s", c.name, gatewayStatusMessage(resp.StatusCode))
		} else {
			statusErr = fmt.Errorf("%s: status %d: %s", c.name, resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		if !isRetryableStatus(resp.StatusCode) {
			return nil, statusErr
		}
		lastErr = statusErr
	}
	return nil, lastErr
}

// isGateway reports whether this client talks to the OneCreat platform gateway
// (authed via ONECREAT_GATEWAY_TOKEN). In that mode the real provider/model is a
// billing secret, so error text must never carry upstream body/message verbatim.
func (c *client) isGateway() bool { return c.keyEnv == "ONECREAT_GATEWAY_TOKEN" }

// gatewayStatusMessage maps an upstream HTTP status to platform-safe wording that
// contains no vendor/model text. A DeepSeek 400 body typically reads "…for model
// deepseek-…", which would pierce the 标准/高级/旗舰 tier abstraction if surfaced —
// so the gateway path returns only this mapping plus the status code.
func gatewayStatusMessage(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "请求内容过长或不合法,请精简后重试(400)"
	case status == http.StatusTooManyRequests:
		return "当前档位请求繁忙,请稍后重试(429)"
	case status >= 500 && status <= 599:
		return "服务暂时不可用,请稍后重试(5xx)"
	default:
		return fmt.Sprintf("请求失败,请稍后重试(%d)", status)
	}
}

func openAIErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Error.Message)
}

// isRetryableStatus returns true for HTTP status codes a transient backoff can
// reasonably recover from: 408 (request timeout), 429 (rate limit), and 5xx.
// 4xx other than 408/429 (auth, validation, not-found) are caller bugs and
// won't fix themselves on retry.
func isRetryableStatus(s int) bool {
	return s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || (s >= 500 && s <= 599)
}

// isTransientErr classifies HTTP client errors. ctx cancellation and deadline
// expiry are caller intent — never retry those. Everything else (DNS failures,
// connection resets, abrupt EOF, etc.) gets one more shot.
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func (c *client) buildRequest(req provider.Request) chatRequest {
	// Repair tool-call pairing before sending: an interrupted/resumed history can
	// carry an assistant tool_calls turn whose results never landed, which DeepSeek
	// rejects with a 400 ("must be followed by tool messages …").
	src := provider.SanitizeToolPairing(req.Messages)
	msgs := make([]chatMessage, len(src))
	for i, m := range src {
		// reasoning_content is deliberately NOT sent back: it's a response-only
		// field. DeepSeek counts re-sent reasoning as billable prompt input
		// (measured ~500 extra tokens per turn on a reasoner chain); MiMo accepts
		// it but does not require it (verified empirically: multi-turn tool-call
		// sessions work fine without it, saving ~18 tokens/turn). The session
		// still keeps it (for display/archive); we just don't pay to re-upload it.
		cm := chatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			wire := chatToolCall{ID: tc.ID, Type: "function"}
			wire.Function.Name = tc.Name
			wire.Function.Arguments = tc.Arguments
			cm.ToolCalls = append(cm.ToolCalls, wire)
		}
		msgs[i] = cm
	}

	var tools []chatTool
	for _, t := range req.Tools {
		tools = append(tools, chatTool{
			Type:     "function",
			Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}

	out := chatRequest{
		Model:           c.model,
		Messages:        msgs,
		Tools:           tools,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: c.effort,
	}
	if c.deepseek {
		out.Thinking = &thinkingMode{Type: "enabled"}
	}
	return out
}

// readStream parses the SSE stream, emits text deltas live, accumulates tool-call
// fragments internally, and emits complete ToolCalls (by index) when done. Each
// call also gets a ChunkToolCallStart the moment its name is known, so a frontend
// can show the tool card while the arguments are still streaming.
func (c *client) readStream(ctx context.Context, resp *http.Response, out chan<- provider.Chunk) {
	defer resp.Body.Close()
	defer close(out)

	// Close the response body when the context is canceled so scanner.Scan()
	// unblocks instead of hanging on a stalled connection. done lets the goroutine
	// exit when readStream returns normally — otherwise it outlives the call, and
	// blocks forever on a non-cancellable context whose Done() is nil.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			resp.Body.Close()
		case <-done:
		}
	}()

	acc := map[int]*provider.ToolCall{}
	started := map[int]bool{}
	var order []int
	var lastFinishReason string
	var think thinkSplitter
	streamNonce := toolCallStreamSeq.Add(1) // 前缀,保证合成的 tool_call id 跨回合唯一

	scanner := bufio.NewScanner(resp.Body)
	// 单个 data: 帧上限 32MB。大 reasoning 块 / 大 tool_call arguments 作为单帧下发时可能
	// 超 1MB,旧上限会让 Scan 报 bufio.ErrTooLong、整轮失败且 mid-stream 不重试。32MB 远超
	// 任何合理单帧,又仍对真正异常流兜底(不做无界读)。
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var sr streamResponse
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			chunkErr := fmt.Errorf("%s: decode stream: %w", c.name, err)
			if c.isGateway() {
				// 网关模式:json 解析错误可能回显上游帧原文,同样脱敏成通用话术。
				chunkErr = fmt.Errorf("%s: 连接中断,请重试", c.name)
			}
			out <- provider.Chunk{Type: provider.ChunkError, Err: chunkErr}
			return
		}
		if sr.Error != nil {
			chunkErr := fmt.Errorf("%s: %s", c.name, sr.Error.Message)
			if c.isGateway() {
				// 网关模式:流中错误 message 同样不得带上游原文。
				chunkErr = fmt.Errorf("%s: 连接中断,请重试", c.name)
			}
			out <- provider.Chunk{Type: provider.ChunkError, Err: chunkErr}
			return
		}
		if len(sr.Choices) > 0 && sr.Choices[0].FinishReason != nil && *sr.Choices[0].FinishReason != "" {
			lastFinishReason = *sr.Choices[0].FinishReason
		}
		if sr.Usage != nil {
			u := normaliseUsage(sr.Usage)
			u.FinishReason = lastFinishReason
			out <- provider.Chunk{Type: provider.ChunkUsage, Usage: u}
		}
		if len(sr.Choices) == 0 {
			continue
		}

		delta := sr.Choices[0].Delta
		if delta.ReasoningContent != "" {
			out <- provider.Chunk{Type: provider.ChunkReasoning, Text: delta.ReasoningContent}
		}
		if delta.Content != "" {
			r, txt := think.push(delta.Content)
			if r != "" {
				out <- provider.Chunk{Type: provider.ChunkReasoning, Text: r}
			}
			if txt != "" {
				out <- provider.Chunk{Type: provider.ChunkText, Text: txt}
			}
		}
		for _, tc := range delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
				cur = &provider.ToolCall{}
				acc[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments
			// Signal the call's start the moment its name is known, so a frontend
			// can show the tool card immediately rather than only after its
			// (possibly large) arguments finish streaming.
			if !started[tc.Index] && cur.Name != "" {
				started[tc.Index] = true
				// 兼容网关按 index 流式、不带 id:在 start 处就合成稳定 id(call_<index>),
				// 让 start 卡片与最终 ToolCall 用同一 id,前端能正确 merge、不悬挂 partial
				// 卡片;多并行 call 时空 id 也不再互不可分(E10)。DeepSeek 官方带 id,不触发。
				if cur.ID == "" {
					cur.ID = fmt.Sprintf("call_%d_%d", streamNonce, tc.Index)
				}
				out <- provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &provider.ToolCall{ID: cur.ID, Name: cur.Name}}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		out <- provider.Chunk{Type: provider.ChunkError, Err: fmt.Errorf("%s: read stream: %w", c.name, err)}
		return
	}

	if r, txt := think.flush(); r != "" || txt != "" {
		if r != "" {
			out <- provider.Chunk{Type: provider.ChunkReasoning, Text: r}
		}
		if txt != "" {
			out <- provider.Chunk{Type: provider.ChunkText, Text: txt}
		}
	}

	sort.Ints(order)
	for _, idx := range order {
		tc := acc[idx]
		if tc.ID == "" {
			// Some OpenAI-compatible gateways stream tool calls by index with no id.
			// Synthesize a stable one (streamNonce-prefixed so it stays unique across
			// turns) so the result can be paired back to its call and a UI merging by
			// id doesn't fold this turn's call into an earlier turn's card.
			tc.ID = fmt.Sprintf("call_%d_%d", streamNonce, idx)
		}
		out <- provider.Chunk{Type: provider.ChunkToolCall, ToolCall: tc}
	}
	out <- provider.Chunk{Type: provider.ChunkDone}
}

// normaliseUsage folds the two cache-hit shapes the OpenAI-compatible ecosystem
// uses into a single Usage: DeepSeek puts prompt_cache_{hit,miss}_tokens at the
// top of usage; OpenAI and MiMo put it nested under prompt_tokens_details.
// Whichever side reports non-zero wins; miss is derived when only hit is given.
// Reasoning tokens land in completion_tokens_details on thinking-mode models.
func normaliseUsage(u *wireUsage) *provider.Usage {
	hit := u.PromptCacheHitTokens
	miss := u.PromptCacheMissTokens
	if hit == 0 && u.PromptTokensDetails != nil {
		hit = u.PromptTokensDetails.CachedTokens
	}
	if miss == 0 && hit > 0 && u.PromptTokens > hit {
		miss = u.PromptTokens - hit
	}
	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return &provider.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheHitTokens:   hit,
		CacheMissTokens:  miss,
		ReasoningTokens:  reasoning,
	}
}

// --- OpenAI-compatible wire protocol ---

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	Tools           []chatTool     `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	Temperature     float64        `json:"temperature,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Thinking        *thinkingMode  `json:"thinking,omitempty"`
}

type thinkingMode struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role string `json:"role"`
	// content is always serialized, even when empty: an assistant turn that is
	// pure tool_calls (no preamble text) has empty content, and DeepSeek's
	// strict deserializer rejects a message missing the field ("missing field
	// `content`"). An empty string satisfies presence and is accepted by every
	// OpenAI-compatible backend for all roles (unlike null, which some reject
	// for a tool message).
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	// no reasoning_content field: it is a response-only signal and is never sent
	// back upstream — re-uploading it is paid prompt input.
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// wireUsage covers both DeepSeek's top-level cache fields and the
// OpenAI/MiMo nested details — normaliseUsage chooses whichever side
// reports values.
type wireUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
