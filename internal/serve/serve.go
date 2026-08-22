// Package serve exposes a control.Controller over HTTP: the typed event stream
// as Server-Sent Events, and the commands as small JSON POST endpoints. It is a
// second frontend alongside the chat TUI — proof that the controller is
// transport-agnostic, and the basis for a browser/desktop client. One server
// drives one session; multiple browser tabs share it.
package serve

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/account"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/engine"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

//go:embed index.html
var indexHTML []byte

// Server wires a controller to its HTTP surface. The Broadcaster must be the
// same sink the controller was constructed with, so events reach SSE clients.
type Server struct {
	ctrl      *control.Controller
	bc        *Broadcaster
	titleProv provider.Provider // lightweight flash provider for session titles
	titles    *titleCache
}

// New builds a Server. bc must be the controller's event sink.
func New(ctrl *control.Controller, bc *Broadcaster) *Server {
	s := &Server{ctrl: ctrl, bc: bc, titles: newTitleCache(ctrl.SessionDir())}
	s.initTitleProvider()
	return s
}

// initTitleProvider builds a lightweight flash-model provider used solely to
// generate short session titles. Errors are silently swallowed — title
// generation is best-effort, and the server works fine without it.
func (s *Server) initTitleProvider() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	entry, ok := cfg.ResolveModel("deepseek-flash")
	if !ok {
		return
	}
	// 标题模型也走平台网关:否则这是全仓唯一绕过档位计量的 AI 调用点(纯网关部署里没有直连
	// 厂商 key,还会 401)。非网关模式下 ApplyOnecreatGateway 是 no-op,行为不变。
	// 账号取自会话自己的 controller,而不是进程环境变量 —— 两者必须是同一个账号。
	gw := s.ctrl.Gateway()
	boot.ApplyOnecreatGateway(entry, gw)
	var creds account.CredentialSource = account.EnvCredential{Var: entry.APIKeyEnv}
	if gw.Active() {
		creds = gw
	}
	prov, err := provider.New(entry.Kind, provider.Config{
		Name:        entry.Name,
		BaseURL:     entry.BaseURL,
		Model:       entry.Model,
		Credentials: creds,
		Gateway:     gw.Active(),
		Extra:       map[string]any{"effort": "off"},
	})
	if err != nil {
		return
	}
	s.titleProv = prov
}

// Handler returns the HTTP routes: GET / (a minimal browser client), GET /events
// (SSE), GET /history, GET /context, and POST command endpoints.
// CORS is NOT applied by default — same-origin policy protects the unauthenticated
// agent endpoints. Call HandlerWithCORS to opt in for local development.
func (s *Server) Handler() http.Handler {
	return s.handler()
}

// HandlerWithCORS returns the same routes as Handler but adds permissive CORS
// headers so a dev frontend on a different origin (e.g. Vite on :5173) can
// reach the server. Do NOT use in production — the server has no auth.
func (s *Server) HandlerWithCORS(origin string) http.Handler {
	return corsMiddleware(s.handler(), origin)
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /snapshot", s.snapshot)
	mux.HandleFunc("GET /context", s.context)
	mux.HandleFunc("POST /submit", s.submit)
	mux.HandleFunc("POST /cancel", s.cancel)
	mux.HandleFunc("POST /approve", s.approve)
	mux.HandleFunc("POST /plan", s.plan)
	mux.HandleFunc("POST /compact", s.compact)
	mux.HandleFunc("POST /new", s.newSession)
	mux.HandleFunc("POST /rewind", s.rewind)
	mux.HandleFunc("POST /fork", s.fork)
	mux.HandleFunc("POST /summarize", s.summarize)
	mux.HandleFunc("POST /bypass", s.bypass)
	mux.HandleFunc("POST /answer", s.answer)
	mux.HandleFunc("POST /resume", s.resume)
	mux.HandleFunc("POST /forget", s.forget)
	mux.HandleFunc("GET /checkpoints", s.checkpoints)
	mux.HandleFunc("GET /branches", s.branches)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("GET /skills", s.skills)
	return logMiddleware(csrfGuard(mux))
}

// csrfGuard rejects state-changing requests that don't carry a JSON content type.
// The command endpoints have no auth and bind to localhost, so a page the user
// visits could otherwise drive them with a simple cross-origin POST (text/plain,
// no preflight) — submitting prompts or auto-approving tool calls. Requiring
// application/json forces a CORS preflight the unauthenticated server never
// answers, blocking cross-site requests; the same-origin frontend (which always
// sends JSON) is unaffected.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = ct[:i]
			}
			if strings.TrimSpace(ct) != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Run serves until the process is killed. Interactive approval is enabled so
// "ask" decisions surface as approval_request events answered via POST /approve.
func (s *Server) Run(addr string) error {
	s.ctrl.EnableInteractiveApproval()
	return http.ListenAndServe(addr, s.Handler())
}

// RunGraceful serves with graceful shutdown. It listens for SIGINT/SIGTERM on
// the provided context and drains active connections for up to 10 seconds
// before returning.
func (s *Server) RunGraceful(ctx context.Context, addr string) error {
	s.ctrl.EnableInteractiveApproval()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("serve: shutting down gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("serve: graceful shutdown failed", "err", err)
		}
		return <-errCh
	}
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// events streams the controller's event flow as SSE until the client
// disconnects. Each event is one `data:` frame of the JSON wire form.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := s.bc.Subscribe()
	defer s.bc.Unsubscribe(sub)

	// 开流时先告诉客户端"你是从第几号接上的"以及这是哪条流(AR-R07)。没有这一条,
	// 客户端收到的第一个 sequence 是 7 还是 700 都无从判断 —— 它不知道自己错过了
	// 前面 699 条,还是本来就只有 6 条。
	fmt.Fprint(w, streamReadyFrame(s.bc.StreamID(), s.bc.Sequence()))
	flusher.Flush()

	for {
		// Drain everything queued before waiting again, so a burst is written in
		// one pass rather than one wake-up per frame.
		for {
			f, ok := sub.TryNext()
			if !ok {
				break
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", f.Data); err != nil {
				return
			}
			flusher.Flush()
		}
		// A client that cannot keep up with state-bearing frames is disconnected
		// rather than served a stream with invisible holes: it reconnects and
		// re-syncs from /history. Silently continuing is the one option Plan 10
		// rules out.
		if sub.Overflowed() {
			fmt.Fprint(w, streamResetFrame(s.bc.Sequence()))
			flusher.Flush()
			return
		}
		select {
		case <-sub.Wake():
		case <-r.Context().Done():
			return
		}
	}
}

// writeOpError 把控制层的错误映射成**语义正确**的状态码。
//
// 之前这些端点一律回 500。500 的含义是「服务器坏了,可以重试」—— 而对一个引擎干不了
// 的操作,事实恰恰相反:重试多少次都一样。照状态码行事的客户端(重试队列、自动化脚本)
// 会一直撞下去,真正的原因只在响应体的中文里。语义错的状态码,和不给原因差不太多。
//
// 两条必须分开(复核 AR-R02 的验收原话就是「明确 409/422」):
//   - 引擎不支持 → 422:请求本身没问题,这个引擎处理不了,**别重试**;
//   - 有回合在跑 → 409:是状态冲突,**待会儿可以再来**。
func writeOpError(w http.ResponseWriter, err error) {
	var unsupported *engine.UnsupportedError
	var busy *control.BusyError
	switch {
	case errors.As(err, &unsupported):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.As(err, &busy):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// submit runs raw user input as a turn (slash commands and @-references
// resolved by the controller). Returns 202 — output arrives on the event stream.
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Input == "" {
		http.Error(w, "missing input", http.StatusBadRequest)
		return
	}
	s.ctrl.Submit(body.Input)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) cancel(w http.ResponseWriter, _ *http.Request) {
	s.ctrl.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Allow   bool   `json:"allow"`
		Session bool   `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.ctrl.Approve(body.ID, body.Allow, body.Session)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.ctrl.SetPlanMode(body.On)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.Compact(r.Context(), ""); err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) newSession(w http.ResponseWriter, _ *http.Request) {
	if err := s.ctrl.NewSession(); err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// history returns the session's message log as {role, content} pairs so a
// reconnecting client can repopulate its transcript.
func (s *Server) history(w http.ResponseWriter, _ *http.Request) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	out := []msg{} // 非 nil:空会话也编码成 [] 而非 null,否则 JS 客户端 for...of 抛 TypeError(E9)
	for _, m := range s.ctrl.History() {
		out = append(out, msg{Role: string(m.Role), Content: m.Content})
	}
	writeJSON(w, out)
}

// streamReadyFrame 是开流时的第一帧:告诉客户端"你是从第几号接上的"以及这是哪条流。
// 没有它,客户端收到的第一个 sequence 是 7 还是 700 都无从判断 —— 它不知道自己错过了
// 前面 699 条,还是本来就只有 6 条(AR-R07)。
func streamReadyFrame(streamID string, seq uint64) string {
	return fmt.Sprintf("event: stream_ready\ndata: {%q:%q,%q:%d}\n\n",
		"streamId", streamID, "sequence", seq)
}

// streamResetFrame 是把慢客户端断开时的最后一帧。
//
// 它指向 /snapshot 而**不是** /history:被丢掉的可能是 ApprovalRequest 或 TurnDone,
// 而 /history 只有 transcript,对不齐这些"只出现过一次"的状态。带上 sequence,客户端
// 才知道自己要对齐到哪儿。
func streamResetFrame(seq uint64) string {
	return fmt.Sprintf("event: stream_reset\ndata: {%q:%q,%q:%q,%q:%d}\n\n",
		"reason", "client too slow", "resyncFrom", "/snapshot", "sequence", seq)
}

// snapshot 返回一份**权威状态**:客户端在任何时候都能用它把自己对齐回真相
// (AR-R07)。
//
// 为什么必须有它:慢客户端积压过多状态帧时会被断开(`stream_reset`),重连之后
// 只是接上了一条新的事件流 —— 它并不知道自己错过了什么。而错过的可能是
// ApprovalRequest(于是审批永远不出现,agent 卡在没人看得见的提示上)或
// TurnDone(于是 UI 永远转圈)。V2 的 sequence 让"有洞"这件事可被发现,但发现之后
// 得有地方对齐,否则这个能力等于没用。
//
// 快照里必须**同时**有 transcript 和那些"只在事件里出现过一次"的状态:pending 的
// 审批与提问、running、plan mode。只给 transcript 是不够的 —— 那正是原来 /history
// 的缺口。
//
// sequence 是这份快照对应的截止序号:之后收到的第一条事件若不是 sequence+1,
// 中间就有洞,客户端应当再取一次快照。
func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	// 每个切片都显式初始化为非 nil:空集合要编码成 [] 而不是 null,否则 JS 客户端
	// 的 for...of 会抛 TypeError(E9 的教训)。
	history := []msg{}
	for _, m := range s.ctrl.History() {
		history = append(history, msg{Role: string(m.Role), Content: m.Content})
	}
	approvals := s.ctrl.PendingApprovals()
	if approvals == nil {
		approvals = []event.Approval{}
	}
	asks := s.ctrl.PendingAsks()
	if asks == nil {
		asks = []event.Ask{}
	}
	// 能力表:前端据此禁用做不到的入口**并说明原因**,而不是让用户点下去撞一个
	// 422。后端仍然独立校验(requireCap 在改任何状态之前)—— 这里给的是"显示原因"
	// 所需的数据,不是那道门本身。UI 从来不是安全边界(复核 AR-R02)。
	caps := map[string]bool{}
	for _, c := range engine.All() {
		caps[string(c)] = s.ctrl.Supports(c)
	}
	writeJSON(w, map[string]any{
		"schemaVersion":    eventwire.SchemaVersion,
		"streamId":         s.bc.StreamID(),
		"sequence":         s.bc.Sequence(),
		"engine":           s.ctrl.EngineName(),
		"capabilities":     caps,
		"history":          history,
		"running":          s.ctrl.Running(),
		"plan":             s.ctrl.PlanMode(),
		"pendingApprovals": approvals,
		"pendingAsks":      asks,
	})
}

// context returns the prompt-vs-window gauge numbers.
func (s *Server) context(w http.ResponseWriter, _ *http.Request) {
	used, window := s.ctrl.ContextSnapshot()
	writeJSON(w, map[string]int{"used": used, "window": window})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("serve: writeJSON encode failed", "err", err)
	}
}

// corsMiddleware adds CORS headers for a specific allowed origin. Only use for
// local development — the server has no auth, so broad CORS would let any site
// drive the agent. origin is the exact origin to allow (e.g.
// "http://localhost:5173"); empty origin skips CORS entirely.
func corsMiddleware(next http.Handler, origin string) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logMiddleware logs each request's method, path, and status.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("serve: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).String(),
		)
	})
}

// responseWriter captures the status code for logging.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter if it supports flushing
// (required for SSE /events). Without this the type assertion in the events
// handler fails and the stream endpoint returns 500.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rewind rewinds the session to a checkpoint.
func (s *Server) rewind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn  int    `json:"turn"`
		Scope string `json:"scope"` // "code", "conversation", "both"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}
	scope := control.RewindBoth
	switch body.Scope {
	case "code":
		scope = control.RewindCode
	case "conversation":
		scope = control.RewindConversation
	}
	if err := s.ctrl.Rewind(body.Turn, scope); err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fork creates a new branch at a checkpoint.
func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn int    `json:"turn"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}
	path, err := s.ctrl.ForkNamed(body.Turn, body.Name)
	if err != nil {
		writeOpError(w, err)
		return
	}
	writeJSON(w, map[string]string{"path": path})
}

// summarize runs summarize-from or summarize-up-to on a turn.
func (s *Server) summarize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn int    `json:"turn"`
		Mode string `json:"mode"` // "from" or "upto"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}
	var err error
	switch body.Mode {
	case "from":
		err = s.ctrl.SummarizeFrom(r.Context(), body.Turn)
	case "upto":
		err = s.ctrl.SummarizeUpTo(r.Context(), body.Turn)
	default:
		http.Error(w, "mode must be 'from' or 'upto'", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bypass toggles YOLO/bypass mode.
func (s *Server) bypass(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.ctrl.SetBypass(body.On)
	w.WriteHeader(http.StatusNoContent)
}

// answer responds to an ask_request.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string            `json:"id"`
		Answers []event.AskAnswer `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.ctrl.AnswerQuestion(body.ID, body.Answers)
	w.WriteHeader(http.StatusNoContent)
}

// resume loads a previous session by index.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	// Use Submit to handle /resume which the controller dispatches
	s.ctrl.Submit("/resume " + body.Path)
	w.WriteHeader(http.StatusAccepted)
}

// forget deletes a saved memory by name.
func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	if err := s.ctrl.ForgetMemory(body.Name); err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkpoints returns the session's checkpoint list for the rewind picker.
func (s *Server) checkpoints(w http.ResponseWriter, _ *http.Request) {
	type cp struct {
		Turn   int    `json:"turn"`
		Prompt string `json:"prompt"`
		Files  int    `json:"files"`
	}
	raw := s.ctrl.Checkpoints()
	out := make([]cp, len(raw))
	for i, c := range raw {
		out[i] = cp{Turn: c.Turn, Prompt: c.Prompt, Files: len(c.Paths)}
	}
	writeJSON(w, out)
}

// branches returns the branch list and tree text.
func (s *Server) branches(w http.ResponseWriter, _ *http.Request) {
	branches, err := s.ctrl.Branches()
	if err != nil {
		writeOpError(w, err)
		return
	}
	tree := s.ctrl.BranchTreeText()
	writeJSON(w, map[string]any{"branches": branches, "tree": tree})
}

// status returns a combined status snapshot.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	used, window := s.ctrl.ContextSnapshot()
	hit, miss := s.ctrl.SessionCache()
	sess := map[string]any{
		"label":     s.ctrl.Label(),
		"running":   s.ctrl.Running(),
		"plan":      s.ctrl.PlanMode(),
		"bypass":    s.ctrl.Bypass(),
		"cwd":       s.ctrl.SessionDir(),
		"used":      used,
		"window":    window,
		"cacheHit":  hit,
		"cacheMiss": miss,
	}
	if u := s.ctrl.LastUsage(); u != nil {
		sess["lastUsage"] = u
	}
	if b, err := s.ctrl.Balance(r.Context()); err == nil && b != nil {
		sess["balance"] = b
	}
	if j := s.ctrl.Jobs(); len(j) > 0 {
		sess["jobs"] = j
	}
	writeJSON(w, sess)
}

const titlePrompt = `Generate a very short title (3-5 words max) for this conversation based on the user's first message. Reply with ONLY the title, no quotes, no punctuation at the end.`

// generateTitle calls a lightweight LLM to produce a short session title.
// Returns empty string on any error — callers should fall back to a preview.
func (s *Server) generateTitle(ctx context.Context, firstMsg string) string {
	if nilutil.IsNil(s.titleProv) || strings.TrimSpace(firstMsg) == "" {
		return ""
	}
	if r := []rune(firstMsg); len(r) > 300 {
		firstMsg = string(r[:300]) + "..."
	}
	ch, err := s.titleProv.Stream(ctx, provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: titlePrompt},
			{Role: provider.RoleUser, Content: firstMsg},
		},
		Temperature: 0,
		MaxTokens:   20,
	})
	if err != nil {
		return ""
	}
	var text strings.Builder
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
		case provider.ChunkError:
			return ""
		}
	}
	title := strings.TrimSpace(text.String())
	if len(title) >= 2 && ((title[0] == '"' && title[len(title)-1] == '"') || (title[0] == '\'' && title[len(title)-1] == '\'')) {
		title = title[1 : len(title)-1]
	}
	return strings.TrimSpace(title)
}

// sessions lists saved session files from the session directory, enriched with
// LLM-generated titles and turn counts.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	dir := s.ctrl.SessionDir()
	if dir == "" {
		writeJSON(w, []any{})
		return
	}
	type sessionEntry struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Title   string `json:"title,omitempty"`
		Turns   int    `json:"turns,omitempty"`
		Current bool   `json:"current,omitempty"`
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	current := filepath.Clean(s.ctrl.SessionPath())
	var out []sessionEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		entry := sessionEntry{Name: name, Path: path, Current: filepath.Clean(path) == current}
		if first, turns := previewSessionFile(path); turns > 0 {
			entry.Turns = turns
			entry.Title = s.sessionTitle(r.Context(), e.Name(), first, fileModNano(e))
		}
		out = append(out, entry)
	}
	// reverse so newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []sessionEntry{}
	}
	writeJSON(w, out)
}

// sessionTitle returns a title for a session: the cached flash-generated title
// when it matches the file's mtime, otherwise a freshly generated one (cached
// for next time), falling back to a truncated preview when generation is off.
func (s *Server) sessionTitle(ctx context.Context, name, first string, mod int64) string {
	if cached, ok := s.titles.get(name, mod); ok {
		return cached
	}
	if title := s.generateTitle(ctx, first); title != "" {
		s.titles.put(name, title, mod)
		return title
	}
	return previewTitle(first)
}

func previewTitle(first string) string {
	if r := []rune(first); len(r) > 50 {
		return string(r[:47]) + "..."
	}
	return first
}

func fileModNano(e os.DirEntry) int64 {
	info, err := e.Info()
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// previewSessionFile reads the first user message and turn count from a JSONL session file.
func previewSessionFile(path string) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	first := ""
	turns := 0
	for {
		var m struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m.Role == "user" {
			turns++
			if first == "" {
				first = strings.TrimSpace(m.Content)
			}
		}
	}
	return first, turns
}

// skills lists discoverable skills.
func (s *Server) skills(w http.ResponseWriter, _ *http.Request) {
	type skillEntry struct {
		Name        string `json:"name"`
		Scope       string `json:"scope"`
		Subagent    bool   `json:"subagent"`
		Description string `json:"description"`
	}
	raw := s.ctrl.Skills()
	out := make([]skillEntry, len(raw))
	for i, sk := range raw {
		out[i] = skillEntry{Name: sk.Name, Scope: string(sk.Scope), Subagent: sk.RunAs == "subagent", Description: sk.Description}
	}
	writeJSON(w, out)
}
