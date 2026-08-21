package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"reasonix/internal/account"
	"reasonix/internal/config"
	"reasonix/internal/event"
)

// defaultStartupTimeout 是等 initialize 握手完成的默认超时。
const defaultStartupTimeout = 30 * time.Second

// errKilled 是"这个 sidecar 是被我们主动杀掉的"的终结原因。
var errKilled = errors.New("dsh 引擎:sidecar 已被终止(取消一轮只能杀进程,该会话无法继续)")

// Options 配置一个 dsh sidecar 引擎实例。
type Options struct {
	// Cfg 是 [dsh] 配置段(bin_path/args/gateway 等)。
	Cfg config.DSHConfig
	// CWD 是 agent 工作区(dsh 的 bash/fs 工具在此工作)。
	CWD string
	// Sink 接收映射+脱敏后的事件。
	Sink event.Sink
	// GatewayToken 是网关鉴权 token(从环境取,调用方读 Cfg.GatewayTokenEnv 传入)。
	// 绝不落盘、不进日志。
	GatewayToken string
	// Credentials 是**活的**凭据源(可空)。它存在的理由是 token 会被刷新,而这个
	// sidecar 拿不到新的:token 在 Boot 时烤进子进程环境,进程一起来那份环境就冻住了,
	// dsh 协议里也没有「换个 token」的方法(见 §22)。
	//
	// 复核 AR-R06 给的是两条路 —— 「必须能到达运行中的 dsh,**或**触发受控滚动重启」。
	// 第一条在这个协议上不存在通道,所以走第二条:每轮开始前比一次,变了就把引擎标成
	// 终结,由调用方重建。不修的话失败形态是最糟的那种 —— sidecar 拿着过期 token 去打
	// 网关,用户看到一串没有解释的鉴权错误,而真正的原因一个字都没有。
	//
	// 为空时行为与改动前完全一致,不新增任何失败模式。
	Credentials account.CredentialSource
	// SecretsToScrub 是要兜底擦除的真实 provider/model/URL 串(从运行时注入)。
	SecretsToScrub []string
	// OnTurnEnd 在 dsh 推来 turn/end 时调用(可空)。这是这个协议里唯一的「一轮
	// 跑完了」信号 —— Adapter 用它兑现 engine.TurnHandle.Wait。它只是通知,不改变
	// 事件映射:turn/end 仍然照常经 Map 走向 sink,该由谁吞掉由 sink 那侧决定。
	OnTurnEnd func()
}

// Engine 驱动一个 dsh sidecar 子进程。它实现"拉起→握手→驱动→关闭"的生命周期,
// 并把 dsh 事件流映射+脱敏后喂给 sink。adapter.go 把它接到 engine.TurnEngine 边界上;
// 该引擎当前在装配根 fail-closed(见 protocol.go 的包注释)。
type Engine struct {
	opts   Options
	scrub  *Scrubber
	cmd    *exec.Cmd
	rpc    *LineClient
	stderr *tailBuffer

	mu      sync.Mutex
	running bool
	started bool
	// dead 一旦置起就不再放下:sidecar 已经死了(被取消杀掉,或自己退出)。
	// 这个进程再也不会响应 —— 继续往旧的 LineClient 写请求,只会让下一轮静默
	// 挂住,而 UI 还显示一切正常。见 AR-R04。
	dead   bool
	deadBy error
	// bootToken 是**烤进子进程环境的那一份** token。它是比较的基准,而不是"当前
	// 应该用的 token" —— 后者由 Credentials 给出。两者不同就说明这个进程落后了。
	bootToken string
}

// New 构造引擎(尚未拉起进程)。BinPath 为空则报错。
func New(opts Options) (*Engine, error) {
	if strings.TrimSpace(opts.Cfg.BinPath) == "" {
		return nil, errors.New("dsh 引擎:未配置 [dsh].bin_path")
	}
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	mask := opts.Cfg.ModelPlaceholder
	if mask == "" {
		mask = "onecreat"
	}
	return &Engine{
		opts:  opts,
		scrub: NewScrubber(mask, opts.SecretsToScrub...),
	}, nil
}

// Start 拉起 dsh 子进程,接好 stdio,完成 initialize 握手。
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("dsh 引擎:已启动")
	}
	e.mu.Unlock()

	cmd := exec.Command(e.opts.Cfg.BinPath, e.opts.Cfg.Args...)
	cmd.Dir = e.opts.CWD
	cmd.Env = e.childEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("dsh 引擎:StdinPipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dsh 引擎:StdoutPipe: %w", err)
	}
	e.stderr = newTailBuffer(64 * 1024)
	cmd.Stderr = e.stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dsh 引擎:启动 %s 失败: %w", e.opts.Cfg.BinPath, err)
	}
	e.cmd = cmd
	e.rpc = NewLineClient(stdout, stdin, e.onNotify)

	e.mu.Lock()
	e.started = true
	e.mu.Unlock()

	// initialize 握手(带超时)。
	to := defaultStartupTimeout
	if e.opts.Cfg.StartupTimeoutSec > 0 {
		to = time.Duration(e.opts.Cfg.StartupTimeoutSec) * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	var res InitializeResult
	err = e.rpc.Call(ictx, MethodInitialize, InitializeParams{
		CWD:      e.opts.CWD,
		Provider: "deepseek-official", // dsh 内置路由名;真实 model 由网关按 token/档位映射
		Model:    e.modelPlaceholder(),
	}, &res)
	if err != nil {
		_ = e.Kill()
		return fmt.Errorf("dsh 引擎:initialize 失败: %w (stderr: %s)", err, e.stderr.String())
	}
	if res.ServerInfo.Name != WireServerName {
		// 不致命,但记一笔:握手对端不是预期的 dsh SDK 运行时。
		e.emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: "dsh 引擎:serverInfo.name=" + res.ServerInfo.Name + "(预期 " + WireServerName + ")"})
	}
	return nil
}

// modelPlaceholder 返回下发给 dsh 的 wire model —— 永远是档位占位符,绝不是真实模型名。
func (e *Engine) modelPlaceholder() string {
	if e.opts.Cfg.ModelPlaceholder != "" {
		return e.opts.Cfg.ModelPlaceholder
	}
	return "onecreat"
}

// inheritedEnvVars 是 sidecar 允许从宿主进程继承的变量白名单。
//
// 之前这里是 `os.Environ()` —— 把本进程的**全部**秘密交给一个第三方子进程:其它
// provider 的 API key、项目 .env 里的东西、CI/宿主注入的凭据。dsh 一个都用不上。
// 白名单只留它真正需要的:找得到自己的运行时、解析得了 DNS、写得进临时目录。
var inheritedEnvVars = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TZ",
	"TMPDIR", "TEMP", "TMP",
	// Windows 上没有这些就连子进程都拉不起来。
	"SystemRoot", "SystemDrive", "windir", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
	// 代理设置:没有它们,受限网络里的 sidecar 直接连不上网关。
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// childEnv 组装子进程环境:白名单继承 + 注入网关 base URL / token(从不落盘)。
func (e *Engine) childEnv() []string {
	var env []string
	for _, k := range inheritedEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if e.opts.Cfg.GatewayBaseURL != "" {
		env = append(env, "DEEPSEEK_BASE_URL="+e.opts.Cfg.GatewayBaseURL)
	}
	if tok := e.tokenForBoot(); tok != "" {
		// dsh 的 deepseek adapter 默认读 DEEPSEEK_API_KEY 作为 Bearer。
		env = append(env, "DEEPSEEK_API_KEY="+tok)
	}
	if e.opts.CWD != "" {
		env = append(env, "DSH_CWD="+e.opts.CWD)
	}
	return env
}

// Submit 把一段文本作为 user message 入队到指定会话,返回 dsh 的 messageId。
func (e *Engine) Submit(ctx context.Context, sessionID, text string) (string, error) {
	if dead, cause := e.Dead(); dead {
		// 绝不往一个死掉的 LineClient 写:那会让这一轮静默挂住,而 UI 还显示就绪。
		return "", cause
	}
	if e.rpc == nil {
		return "", errors.New("dsh 引擎:未启动")
	}
	var res SessionPromptResult
	err := e.rpc.Call(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID:     sessionID,
		ContentBlocks: []ContentBlock{TextBlock(text)},
	}, &res)
	if err != nil {
		return "", err
	}
	return res.MessageID, nil
}

// Running 报告 dsh 侧整 agent 是否在跑(由 session.status 通知维护)。
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Died 在 sidecar 的读循环终止(EOF 或读错误)时收到原因。子进程半路死掉时,
// dsh 再也不会推 turn/end —— 没有这个信号,等一轮结束就会永远挂住。未启动时返回
// nil channel(select 上永远不就绪),调用方无需特判。
func (e *Engine) Died() <-chan error {
	if e.rpc == nil {
		return nil
	}
	return e.rpc.Wait()
}

// Cancel 取消当前工作。dsh SDK JSON-RPC 无 mid-turn 取消方法(见 01 笔记),故取消
// 语义降级为"关闭子进程"。这是 spike 的已知取舍,Phase 1 收尾需评估改走 ACP 或等
// 上游补 cancel。
func (e *Engine) Cancel() error {
	return e.Kill()
}

// Shutdown 优雅关闭:先发 shutdown RPC,再走 SIGTERM→SIGKILL 梯。
func (e *Engine) Shutdown(ctx context.Context) error {
	if e.rpc != nil {
		sctx, cancel := context.WithTimeout(ctx, time.Second)
		_ = e.rpc.Call(sctx, MethodShutdown, map[string]any{}, nil)
		cancel()
	}
	return e.Kill()
}

// tokenForBoot 决定烤进子进程的那份 token,并记下它作为后续比较的基准。
// 有活的凭据源就用它当下的值,否则退回构造时传入的静态串(改动前的行为)。
func (e *Engine) tokenForBoot() string {
	tok := e.opts.GatewayToken
	if e.opts.Credentials != nil {
		if v, err := e.opts.Credentials.Token(context.Background()); err == nil && v != "" {
			tok = v
		}
	}
	e.mu.Lock()
	e.bootToken = tok
	e.mu.Unlock()
	return tok
}

// errTokenRefreshed 是"网关 token 变了,这个进程还拿着旧的"的终结原因。
var errTokenRefreshed = errors.New("dsh 引擎:网关 token 已刷新,而 sidecar 的凭据在启动时就固定了 —— 该会话需要重建(受控滚动重启)")

// tokenChanged 报告凭据源给出的 token 是否已经和启动时烤进子进程的那份不一样了。
//
// 取不到 token 时返回 false:凭据源临时不可用不该把一个正常会话打死,那会把一次网络
// 抖动变成"会话没了"。真正过期的 token 会在网关那侧以 401 收场,不会被这里放过。
func (e *Engine) tokenChanged(ctx context.Context) bool {
	if e.opts.Credentials == nil {
		return false
	}
	now, err := e.opts.Credentials.Token(ctx)
	if err != nil || now == "" {
		return false
	}
	e.mu.Lock()
	boot := e.bootToken
	e.mu.Unlock()
	return boot != "" && now != boot
}

// markDead 记下"这个 sidecar 不能再用了"(只记第一个原因)。
func (e *Engine) markDead(cause error) {
	e.mu.Lock()
	if !e.dead {
		e.dead, e.deadBy = true, cause
	}
	e.running = false
	e.mu.Unlock()
}

// Dead 报告 sidecar 是否已经终结,并给出原因。终结是**不可逆**的:dsh 没有
// mid-turn 取消,取消只能杀进程,而杀掉之后这条 stdio 连接和它承载的会话就没了。
func (e *Engine) Dead() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dead, e.deadBy
}

// Kill 强制结束子进程(幂等),并把引擎标记为终结。
func (e *Engine) Kill() error {
	e.markDead(errKilled)
	e.mu.Lock()
	cmd := e.cmd
	e.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// 先温和 SIGTERM,给 200ms,再 SIGKILL。
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		_ = cmd.Process.Kill()
	}
	return nil
}

// onNotify 分派 dsh 的 server→client 通知。
func (e *Engine) onNotify(method string, params json.RawMessage) {
	switch method {
	case NotifySessionEvent:
		var n SessionEventNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		for _, ev := range Map(n.Event) {
			e.emit(ev)
		}
		// 先把事件发完再通知「这轮结束了」:Wait 返回后 Controller 会立刻收尾
		// (Stop hook、落盘),此时这一轮的事件必须已经全部出去了。
		if n.Event.Type == EvTurnEnd && e.opts.OnTurnEnd != nil {
			e.opts.OnTurnEnd()
		}
	case NotifySessionStatus:
		var n SessionStatusNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e.mu.Lock()
		e.running = n.Status == "running"
		e.mu.Unlock()
	}
}

// emit 脱敏后喂 sink(网关红线的最后一道防线)。
func (e *Engine) emit(ev event.Event) {
	e.opts.Sink.Emit(e.scrub.Event(ev))
}
