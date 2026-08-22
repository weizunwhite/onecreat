package dsh

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// defaultStartupTimeout 是等 initialize 握手完成的默认超时。
const defaultStartupTimeout = 60 * time.Second

// gatewayTierEnv 是"当前选中档位"的进程环境变量,值形如 tier-1 / tier-2 / tier-3。
// 与 native 路径同一个字符串(desktop/accounts_app.go 的 gatewayEnvTier 写它,
// internal/boot/boot.go 的 applyOnecreatGateway 读它),别新造名字。
const gatewayTierEnv = "ONECREAT_TIER"

// dsh 子进程里承载连接事实的两个环境变量名。必须与 dsh/plugins/gateway 的
// DEFAULT_BASE_URL_ENV / DEFAULT_API_KEY_ENV 以及 profile 里的配置一致 ——
// 凭证轮换(onecreat/credentials.set)写的也是这两个。
const (
	envDSHBaseURL = "ONECREAT_DSH_BASE_URL"
	envDSHAPIKey  = "ONECREAT_DSH_API_KEY"
)

// Approver 是"问用户要不要放行这次工具调用"的回调。返回 (allow, 本会话记住, err),
// 与 control.Controller.RequestApproval 同签名 —— dsh 引擎因此复用 native 的整套
// 审批 UI、会话内授权记忆、YOLO/bypass 语义,前端零改动。
type Approver func(ctx context.Context, toolName, subject string) (allow bool, session bool, err error)

// ToolInvoker 在 Go 侧执行一个内置工具并返回它的输出(工具桥用:complete_step
// 由 Go 执行,因为证据引擎是它的裁判)。
//
// 它是**注入函数**而不是 `func(name string) (tool.Tool, bool)`:引擎层不得 import
// `internal/tool`(A14 守卫 `engine/boundary_test.go`)。查注册表、把证据账本塞进
// ctx、给工具桥自己的调用记账,全部由装配根(internal/boot)在闭包里做完。
// 找不到该工具时返回一个错误,引擎把它原样回帧给 sidecar。
type ToolInvoker func(ctx context.Context, name string, args json.RawMessage) (string, error)

// Recorder 是证据记账的注入点。同样出于 A14 守卫:引擎层不得 import
// `internal/evidence`,所以这里只收三个闭包,由装配根接到真账本上。
// 三个字段都允许为 nil(等价于"不记账",headless / 测试用)。
type Recorder struct {
	// Reset 在每一轮开始时清空本轮账本。
	Reset func()
	// ToolCall 记一条真实发生过的工具调用(成功与否照实记)。
	ToolCall func(name string, args json.RawMessage, success, readOnly bool)
	// Todos 记一次 todo_write —— 原始 JSON 数组,由装配根解成 evidence.TodoItem。
	Todos func(raw json.RawMessage)
}

// reset / toolCall / todos 是 Recorder 的 nil 安全调用包装。
func (r Recorder) reset() {
	if r.Reset != nil {
		r.Reset()
	}
}

func (r Recorder) toolCall(name string, args json.RawMessage, success, readOnly bool) {
	if r.ToolCall != nil {
		r.ToolCall(name, args, success, readOnly)
	}
}

func (r Recorder) todos(raw json.RawMessage) {
	if r.Todos != nil {
		r.Todos(raw)
	}
}

// 预执行决定的三种取值(与 dsh 的 PreToolDecision 一一对应)。
const (
	DecisionAllow = "allow"
	DecisionAsk   = "ask"
	DecisionDeny  = "deny"
)

// Decider 是"这次工具调用该放行/问用户/直接拒"的策略判定,由 Go 侧的
// permission.Policy 实现 —— 于是 deny 名单、ask 规则在 dsh 引擎下同样生效
// (dsh 自己的工具不经过 Go 的 tool registry,不接这条就等于绕过整套权限策略)。
type Decider func(toolName string, args json.RawMessage) (decision string, reason string)

// Options 配置一个 dsh sidecar 引擎实例。
type Options struct {
	// Cfg 是 [dsh] 配置段。
	Cfg config.DSHConfig
	// CWD 是 agent 工作区(dsh 的 bash/fs 工具在此工作)。
	CWD string
	// Sink 接收映射+脱敏后的事件。
	Sink event.Sink
	// SystemPrompt 是下发给 dsh 的系统提示(它进 dsh 的缓存前缀,不是每轮注入)。
	SystemPrompt string
	// Gateway 为 true 表示走平台 AI 网关:wire model 下发档位占位符。
	// false = 直连模式,下发 Cfg.DirectModel(真实模型 id)。
	Gateway bool
	// BaseURL / APIKey 是 provider 连接事实,只经环境注入子进程,绝不落盘。
	BaseURL string
	APIKey  string
	// APIKeyFunc 非 nil 时,取凭证一律走它而不是 APIKey 快照。网关模式下平台
	// token 约 50 分钟就会被后台刷新一次(desktop 只更新父进程 env、不重建标签),
	// 而子进程的环境是 spawn 时拷贝的死快照 —— 只认快照,dsh 模式一小时后必然 401。
	// 传函数进来,引擎才能每轮重新取"此刻"的凭证并按需下发(见 syncCredentials)。
	APIKeyFunc func() string
	// SecretsToScrub 是要兜底擦除的品牌/模型/URL 串。
	SecretsToScrub []string
	// HardwareMCP 是 OneCreat 硬件 MCP 二进制路径(空 = 不挂)。
	HardwareMCP string
	// SessionRoot 是 dsh 自己的会话 store 目录。
	SessionRoot string
	// Session 是 Go 侧的消息镜像:引擎在每轮结束把 user/assistant 文本写进去,
	// 于是 History / 会话落盘 / 会话标题 / 前端恢复全都照旧工作。
	// dsh 自己的 store 仍是模型可见历史的真源,这里只是投影(只读用途)。
	Session *agent.Session
	// Tools 是工具桥的 Go 侧执行函数。
	Tools ToolInvoker
	// Approver 是审批桥的 Go 侧回调。nil = 非交互(headless),与 native 的
	// permission.Gate 一样"保持自主":Ask 一律放行。
	Approver Approver
	// Decide 是权限策略判定(deny/ask/allow)。nil = 一律 allow。
	Decide Decider
	// PreEdit 在 dsh 执行任一工具前被调用(工具名 + 原始参数),Go 侧据此做
	// 文件快照(checkpoint/rewind 保留 Go 实现)。不得阻塞太久(5s 超时后放行)。
	PreEdit func(name string, args json.RawMessage)
	// Ledger 是证据记账的注入点;引擎消费 dsh 的 tool/call、tool/result、todo/write
	// 事件喂它。零值 = 不记账。
	Ledger Recorder
	// Stderr 是 sidecar 诊断输出的去处(nil = 只留尾缓冲,不外泄)。
	Stderr io.Writer
}

// Engine 驱动一个 dsh sidecar 子进程,并实现 agent.Runner —— 于是
// control.Controller 只要把 runner 换成它,整条 Compose/hooks/checkpoint/审批/
// 证据链路都不用动(见 docs/dsh调研/05)。
type Engine struct {
	opts   Options
	scrub  *Scrubber
	cmd    *exec.Cmd
	rpc    *LineClient
	stderr *tailBuffer

	mu        sync.Mutex
	started   bool
	closed    bool
	running   bool
	sessionID string
	// created 记录本进程内已经建过/载过的 dsh 会话,避免重复 load。
	created map[string]bool
	// turn 是当前 turn 的收敛状态。
	turn *turnState
	// pending 缓冲本轮的 assistant 文本,turn 收敛后一次性写进 Go 侧 Session
	// (单写者纪律:只在 Run 所在的 run-loop goroutine 上写)。
	pending []provider.Message
	// calls 缓存在飞的 tool/call,等它的 tool/result 到了再一起记进证据账本。
	calls map[string]ToolCallInfo
	// ephemeralID 是"还没有会话文件"时用的一次性 dsh 会话 id(每个引擎实例一个)。
	ephemeralID string
	// lastBaseURL / lastAPIKey 是"已经下发给子进程的"连接事实(spawn 时的环境快照,
	// 之后由 syncCredentials 维护)。与当前值不同才补发,相同不重发。
	lastBaseURL string
	lastAPIKey  string
}

// turnState 跟踪一轮的生命周期:dsh 先报 running,收敛后报 idle。
type turnState struct {
	started bool
	done    chan struct{}
	closed  bool
}

// New 构造引擎(尚未拉起进程)。
func New(opts Options) (*Engine, error) {
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	// 脱敏用的替换文案:用产品名而不是档位占位符,免得错误体读成
	// "tier-1 API request to tier-1 failed" 这种莫名其妙的话。
	const mask = "OneCreat"
	return &Engine{
		opts:    opts,
		scrub:   NewScrubber(mask, opts.SecretsToScrub...),
		created: map[string]bool{},
	}, nil
}

// BindSession 把引擎的 dsh 会话绑到一个 Go 会话文件路径上。dsh 会话 id 由路径
// 确定性派生,于是"打开历史会话"能对上同一条 dsh 会话日志(见 loadIfNeeded)。
//
// 路径为空(还没落盘的新会话、headless 的 `reasonix run`)时用一个进程内随机 id:
// 否则所有这类运行会共用同一条 dsh 会话、把上一次的上下文带进来。
func (e *Engine) BindSession(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		if e.ephemeralID == "" {
			var b [12]byte
			if _, err := rand.Read(b[:]); err != nil {
				e.ephemeralID = "oc-" + strconv.FormatInt(time.Now().UnixNano(), 16)
			} else {
				e.ephemeralID = "oc-" + hex.EncodeToString(b[:])
			}
		}
		e.sessionID = e.ephemeralID
		return
	}
	e.sessionID = sessionIDFor(path)
}

// sessionIDFor 由 Go 会话文件路径派生稳定的 dsh 会话 id。
func sessionIDFor(path string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(path)))
	return "oc-" + hex.EncodeToString(sum[:])[:24]
}

// SessionID 返回当前 dsh 会话 id。
func (e *Engine) SessionID() string {
	e.mu.Lock()
	id := e.sessionID
	e.mu.Unlock()
	if id == "" {
		e.BindSession("")
		e.mu.Lock()
		id = e.sessionID
		e.mu.Unlock()
	}
	return id
}

// ensureStarted 懒启动 sidecar:第一轮对话时才拉起 node 子进程,于是构造
// Controller(桌面新建标签)本身不付启动代价。
func (e *Engine) ensureStarted(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("dsh 引擎:已关闭")
	}
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	return e.Start(ctx)
}

// Start 拉起 dsh 子进程,接好 stdio,完成 initialize 握手。
func (e *Engine) Start(ctx context.Context) error {
	spec, err := resolveLaunch(e.opts.Cfg)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.started = true
	e.mu.Unlock()

	cmd := exec.Command(spec.Bin, spec.Args...)
	cmd.Dir = spec.RuntimeDir
	cmd.Env = e.childEnv(spec.RuntimeDir)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("dsh 引擎:StdinPipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dsh 引擎:StdoutPipe: %w", err)
	}
	e.stderr = newTailBuffer(64 * 1024)
	if e.opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(e.stderr, e.opts.Stderr)
	} else {
		cmd.Stderr = e.stderr
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dsh 引擎:启动 %s 失败: %w", spec.Bin, err)
	}
	e.mu.Lock()
	e.cmd = cmd
	e.rpc = NewLineClient(stdout, stdin, e.onNotify)
	rpc := e.rpc
	e.mu.Unlock()

	to := defaultStartupTimeout
	if e.opts.Cfg.StartupTimeoutSec > 0 {
		to = time.Duration(e.opts.Cfg.StartupTimeoutSec) * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	var res InitializeResult
	err = rpc.Call(ictx, MethodInitialize, InitializeParams{
		CWD:      e.opts.CWD,
		Provider: GatewayProviderRoute,
		Model:    e.wireModel(),
	}, &res)
	if err != nil {
		_ = e.Kill()
		return fmt.Errorf("dsh 引擎:initialize 失败: %w (sidecar: %s)", err, e.scrub.Text(e.stderr.String()))
	}
	if res.ServerInfo.Name != WireServerName {
		e.emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: "dsh 引擎:serverInfo.name=" + res.ServerInfo.Name + "(预期 " + WireServerName + ")"})
	}
	return nil
}

// wireModel 返回下发给 dsh 的 wire model。网关模式永远是档位占位符(真实模型由
// 平台按 token/档位映射,客户端不该也不能知道);直连模式才是真实模型 id。
//
// 网关模式下优先取 ONECREAT_TIER —— 平台网关就是按 "tier-N" 映射模型与计费的
// (native 路径见 internal/boot/boot.go 的 applyOnecreatGateway,同一个来源)。
// 不读它就等于"用户选了旗舰、请求里却写着 onecreat":要么被网关拒,要么切档不生效。
// 档位由桌面端登录/切档时写进进程环境(desktop/accounts_app.go),切档会重建
// controller → 重新 spawn sidecar → 这里重新读到新档位。
func (e *Engine) wireModel() string {
	if e.opts.Gateway {
		if tier := strings.TrimSpace(os.Getenv(gatewayTierEnv)); tier != "" {
			return tier
		}
		if e.opts.Cfg.ModelPlaceholder != "" {
			return e.opts.Cfg.ModelPlaceholder
		}
		return "onecreat"
	}
	if m := strings.TrimSpace(e.opts.Cfg.DirectModel); m != "" {
		return m
	}
	return "deepseek-v4-flash"
}

// currentAPIKey 取"此刻"的凭证(网关模式下会被后台刷新覆盖)。
func (e *Engine) currentAPIKey() string {
	if e.opts.APIKeyFunc != nil {
		return strings.TrimSpace(e.opts.APIKeyFunc())
	}
	return strings.TrimSpace(e.opts.APIKey)
}

// currentBaseURL 取"此刻"的 base URL。
func (e *Engine) currentBaseURL() string { return strings.TrimSpace(e.opts.BaseURL) }

// snapshotCredentials 记下已经下发给子进程的连接事实(spawn 时由 childEnv 调用)。
func (e *Engine) snapshotCredentials(baseURL, apiKey string) {
	e.mu.Lock()
	e.lastBaseURL, e.lastAPIKey = baseURL, apiKey
	e.mu.Unlock()
}

// syncCredentials 在发 prompt 之前把"此刻"的凭证补发给 sidecar。
//
// 为什么必须每轮做:子进程环境是 spawn 时的快照,而平台 token 约 50 分钟刷新一次
// (desktop/accounts_app.go 刷新后只调 applyGatewayEnvFromSession 更新父进程 env,
// 有意不重建标签)。native provider 每次请求都 os.Getenv 所以没事;dsh 不补这一步,
// 一小时后必然 401(表现为 "! AUTH: invalid token")。
// 凭证只走内存与子进程环境,不落盘、不进 profile、不进日志。
func (e *Engine) syncCredentials(ctx context.Context) {
	apiKey := e.currentAPIKey()
	baseURL := e.currentBaseURL()
	e.mu.Lock()
	rpc := e.rpc
	var params CredentialsParams
	if apiKey != "" && apiKey != e.lastAPIKey {
		params.APIKey = apiKey
	}
	if baseURL != "" && baseURL != e.lastBaseURL {
		params.BaseURL = baseURL
	}
	e.mu.Unlock()
	if rpc == nil || (params.APIKey == "" && params.BaseURL == "") {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rpc.Call(cctx, MethodCredentialsSet, params, nil); err != nil {
		// 不阻塞这一轮:旧凭证也许还没过期;真过期了 turn/end 会照实报 AUTH 错。
		e.emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: "dsh 引擎:凭证同步失败(令牌轮换可能未生效):" + err.Error()})
		return
	}
	e.snapshotCredentials(baseURL, apiKey)
}

// childEnv 组装子进程环境:凭证与 base URL 只走环境(不落盘、不进 profile)。
func (e *Engine) childEnv(runtimeDir string) []string {
	env := os.Environ()
	set := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	baseURL := e.currentBaseURL()
	apiKey := e.currentAPIKey()
	set(envDSHBaseURL, baseURL)
	set(envDSHAPIKey, apiKey)
	e.snapshotCredentials(baseURL, apiKey)
	set("DSH_CWD", e.opts.CWD)
	set("DSH_SYSTEM_PROMPT", e.opts.SystemPrompt)
	root := e.opts.SessionRoot
	if root == "" {
		root = filepath.Join(runtimeDir, ".sessions")
	}
	set("DSH_SESSION_ROOT", root)
	set("ONECREAT_HARDWARE_MCP", e.opts.HardwareMCP)
	return env
}

// --- agent.Runner ---

// Run 跑完整一轮:把(已经过 Controller.Compose 的)输入排进 dsh,等这一轮收敛。
// 它就是 native 里 agent.Agent.Run 的位置,所以 Controller 的 Compose / hooks /
// checkpoint / 计划门 全部照旧生效。
func (e *Engine) Run(ctx context.Context, input string) error {
	if err := e.ensureStarted(ctx); err != nil {
		return err
	}
	e.opts.Ledger.reset()

	sid := e.SessionID()
	e.loadIfNeeded(ctx, sid)
	e.syncCredentials(ctx)

	st := &turnState{done: make(chan struct{})}
	e.mu.Lock()
	if e.turn != nil {
		e.mu.Unlock()
		return errors.New("dsh 引擎:上一轮还没结束")
	}
	e.turn = st
	e.pending = nil
	rpc := e.rpc
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.turn = nil
		e.mu.Unlock()
	}()

	if e.opts.Session != nil {
		e.opts.Session.Add(provider.Message{Role: provider.RoleUser, Content: input})
	}

	if err := rpc.Call(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID:     sid,
		ContentBlocks: []ContentBlock{TextBlock(input)},
	}, nil); err != nil {
		return e.wrapErr(err)
	}

	var runErr error
	select {
	case <-st.done:
	case <-ctx.Done():
		// 有 wire 级取消:让 dsh 自己收敛这一轮,不再靠杀进程。
		e.cancelSession(sid)
		select {
		case <-st.done:
		case <-time.After(10 * time.Second):
		}
	case err := <-rpc.Wait():
		runErr = fmt.Errorf("dsh 引擎:sidecar 意外退出: %w (sidecar: %s)", err, e.scrub.Text(e.stderr.String()))
	}

	e.flushPending()
	return runErr
}

// wrapErr 给出错的 RPC 附上脱敏后的 sidecar 诊断尾巴。
func (e *Engine) wrapErr(err error) error {
	tail := ""
	if e.stderr != nil {
		tail = strings.TrimSpace(e.scrub.Text(e.stderr.String()))
	}
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w (sidecar: %s)", err, tail)
}

// flushPending 把本轮 dsh 侧产出的 assistant 文本写进 Go 会话镜像。只在 Run 的
// goroutine 上调用,满足 Session 单写者纪律。
func (e *Engine) flushPending() {
	e.mu.Lock()
	msgs := e.pending
	e.pending = nil
	e.mu.Unlock()
	if e.opts.Session == nil {
		return
	}
	for _, m := range msgs {
		e.opts.Session.Add(m)
	}
}

// loadIfNeeded 第一次碰到某个 dsh 会话 id 时,尝试从持久化里 resume 它。
// 失败(store 里没有)属正常:下一步的 session/prompt 会建一个新的。
func (e *Engine) loadIfNeeded(ctx context.Context, sid string) {
	e.mu.Lock()
	done := e.created[sid]
	e.created[sid] = true
	rpc := e.rpc
	e.mu.Unlock()
	if done || rpc == nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var res SessionLoadResult
	_ = rpc.Call(lctx, MethodSessionLoad, SessionRefParams{SessionID: sid}, &res)
}

// --- 引擎控制面(control.EngineBackend)---

// Running 报告 dsh 侧整 agent 是否在跑(由 session.status 通知维护)。
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Cancel 取消当前 turn。走 wire 上的 onecreat/session.cancel(我们自己补的方法),
// 不再是"杀进程"。
func (e *Engine) Cancel() error {
	e.cancelSession(e.SessionID())
	return nil
}

func (e *Engine) cancelSession(sid string) {
	e.mu.Lock()
	rpc := e.rpc
	e.mu.Unlock()
	if rpc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rpc.Call(ctx, MethodSessionCancel, SessionRefParams{SessionID: sid}, nil)
}

// SetPlanMode 驱动 dsh 的 plan-mode 插件。sidecar 还没起、或会话还没建时是 no-op
// (Controller 会在下一轮重新下发)。
func (e *Engine) SetPlanMode(active bool) error {
	e.mu.Lock()
	rpc := e.rpc
	started := e.started
	sid := e.sessionID
	e.mu.Unlock()
	if !started || rpc == nil || sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := rpc.Call(ctx, MethodPlanModeSet, PlanModeParams{SessionID: sid, Active: active}, nil)
	if err != nil {
		// 会话还没建起来(第一轮之前)属正常,不打扰用户。
		return nil
	}
	return nil
}

// NewSession 让引擎切到一条新的 dsh 会话(Controller 换 session 文件时调用)。
func (e *Engine) NewSession(path string) {
	e.BindSession(path)
}

// Inject 把一段文本作为"下一次 pre-step 的模型可见上下文"塞进 dsh(不进系统提示,
// 保住前缀缓存)。Controller 的每轮状态默认随 Compose 走用户消息(与 native 一致),
// 这个接缝留给 turn 进行中的补充注入(后台任务完成通知等)。
func (e *Engine) Inject(text string) error {
	e.mu.Lock()
	rpc := e.rpc
	sid := e.sessionID
	e.mu.Unlock()
	if rpc == nil || sid == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return rpc.Call(ctx, MethodInject, InjectParams{SessionID: sid, Text: text}, nil)
}

// Shutdown 优雅关闭:先发 shutdown RPC,再走 SIGTERM→SIGKILL 梯。
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	rpc := e.rpc
	e.closed = true
	e.mu.Unlock()
	if rpc != nil {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = rpc.Call(sctx, MethodShutdown, map[string]any{}, nil)
		cancel()
	}
	return e.Kill()
}

// Close 是 Controller.Close 的清理钩子:关掉 sidecar,不留 node 残留。
func (e *Engine) Close() error {
	return e.Shutdown(context.Background())
}

// Kill 强制结束子进程(幂等)。
func (e *Engine) Kill() error {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// --- 通知处理 ---

// onNotify 分派 dsh 的 server→client 通知。读循环 goroutine 上跑,**不能阻塞**:
// 任何要等 Go 侧应答的桥接都开 goroutine。
func (e *Engine) onNotify(method string, params json.RawMessage) {
	switch method {
	case NotifySessionEvent:
		var n SessionEventNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e.consume(n.Event)
		for _, ev := range Map(n.Event) {
			e.emit(ev)
		}
	case NotifySessionStatus:
		var n SessionStatusNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e.onStatus(n.Status)
	case NotifyApprovalRequest:
		var n ApprovalRequestNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		go e.handleApproval(n)
	case NotifyToolInvoke:
		var n ToolInvokeNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		go e.handleToolInvoke(n)
	case NotifyToolPreExecute:
		var n ToolPreExecuteNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		go e.handlePreExecute(n)
	}
}

// onStatus 维护 running 标志与一轮的收敛判定:running→idle 才算这一轮结束。
func (e *Engine) onStatus(status string) {
	e.mu.Lock()
	e.running = status == "running"
	st := e.turn
	if st != nil {
		if status == "running" {
			st.started = true
		} else if st.started && !st.closed {
			st.closed = true
			close(st.done)
		}
	}
	e.mu.Unlock()
}

// consume 把 dsh 事件喂给证据引擎与消息镜像(不产出前端事件,那是 Map 的活)。
func (e *Engine) consume(raw RawSessionEvent) {
	switch raw.Type {
	case EvToolCall:
		if info, ok := ParseToolCall(raw); ok {
			e.mu.Lock()
			e.pendingCalls()[info.CallID] = info
			e.mu.Unlock()
		}
	case EvToolResult:
		info, ok := ParseToolResult(raw)
		if !ok {
			return
		}
		e.mu.Lock()
		call, known := e.pendingCalls()[info.CallID]
		delete(e.pendingCalls(), info.CallID)
		e.mu.Unlock()
		if !known {
			return
		}
		// 证据账本:一条真实发生过的工具调用(成功与否照实记)。
		e.opts.Ledger.toolCall(call.Name, json.RawMessage(call.Args), !info.IsError, isReadOnlyName(call.Name))
	case EvTodoWrite:
		todos, ok := ParseTodos(raw)
		if !ok {
			return
		}
		e.opts.Ledger.todos(todos)
	case EvAssistantMsg:
		var d struct {
			Message struct {
				Content []contentBlock `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return
		}
		text := ""
		for _, c := range d.Message.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		if strings.TrimSpace(text) == "" {
			return
		}
		e.mu.Lock()
		e.pending = append(e.pending, provider.Message{Role: provider.RoleAssistant, Content: text})
		e.mu.Unlock()
	}
}

// pendingCalls 惰性初始化在飞调用表。调用方必须已持有 e.mu。
func (e *Engine) pendingCalls() map[string]ToolCallInfo {
	if e.calls == nil {
		e.calls = map[string]ToolCallInfo{}
	}
	return e.calls
}

// isReadOnlyName 粗判一个 dsh 工具名是不是只读(证据引擎用它区分读/写受益)。
func isReadOnlyName(name string) bool {
	switch name {
	case "read", "grep", "glob", "ls", "todo_write", "complete_step":
		return true
	}
	return strings.HasPrefix(name, "mcp__") && strings.Contains(name, "detect")
}

// handleApproval 把 dsh 的审批问题转成 native 的 event.Approval,等用户回答后回帧。
func (e *Engine) handleApproval(n ApprovalRequestNotification) {
	e.mu.Lock()
	approver := e.opts.Approver
	e.mu.Unlock()
	// 非交互(headless)下没有 approver:与 native 的 permission.Gate 一致,放行。
	allow := true
	if approver != nil {
		a, _, err := approver(context.Background(), n.ToolName, n.Reason)
		allow = err == nil && a
	}
	e.notify(NotifyApprovalResolve, ApprovalResolveNotification{ID: n.ID, Allow: allow})
}

// handleToolInvoke 在 Go 侧执行一个内置工具(工具桥),把结果回帧给 dsh。
// complete_step 走这条路:它的裁判是 Go 侧的证据账本。
func (e *Engine) handleToolInvoke(n ToolInvokeNotification) {
	out, errText := "", ""
	if e.opts.Tools == nil {
		errText = "OneCreat 侧没有名为 " + n.Name + " 的工具"
	} else if res, err := e.opts.Tools(context.Background(), n.Name, json.RawMessage(n.Arguments)); err != nil {
		errText = err.Error()
	} else {
		out = res
	}
	e.notify(NotifyToolResult, ToolResultNotification{ID: n.ID, Output: out, Error: errText})
}

// handlePreExecute 是 dsh 每次工具执行前的**单一决策点**:Go 侧在这里一次做三件事
//  1. 跑权限策略(deny 名单 / ask 规则 / 只读放行)—— 否则 dsh 自己的 bash/write
//     完全绕过 OneCreat 的权限体系;
//  2. ask 时经 Controller 弹审批(复用 native 的整套审批 UI 与会话内授权记忆);
//  3. 放行前给 checkpoint 做文件快照(文件级 rewind 保留 Go 实现)。
func (e *Engine) handlePreExecute(n ToolPreExecuteNotification) {
	e.mu.Lock()
	preEdit := e.opts.PreEdit
	decide := e.opts.Decide
	approver := e.opts.Approver
	e.mu.Unlock()

	args := json.RawMessage(n.Arguments)
	decision, reason := DecisionAllow, ""
	if decide != nil {
		decision, reason = decide(n.Name, args)
	}
	if os.Getenv("ONECREAT_DSH_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[dsh-dbg] pre-exec %s decision=%s approver=%v decide=%v\n", n.Name, decision, approver != nil, decide != nil)
	}
	if decision == DecisionAsk {
		if approver == nil {
			// 非交互(headless):与 native 的 permission.Gate 一致 —— 保持自主,放行。
			decision = DecisionAllow
		} else if allow, _, err := approver(context.Background(), n.Name, subjectOf(args)); err != nil || !allow {
			decision = DecisionDeny
			reason = "用户拒绝了这次调用 —— 不要重试,换个做法或问用户下一步怎么办。"
		} else {
			decision = DecisionAllow
		}
	}
	if decision == DecisionAllow && preEdit != nil {
		preEdit(n.Name, args)
	}
	e.notify(NotifyToolPreExecuteDone, PreExecuteDecision{ID: n.ID, Decision: decision, Reason: reason})
}

// subjectOf 从工具参数里取一个人类可读的"对象"(命令行 / 文件路径),用于审批卡片。
func subjectOf(args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "cmd", "path", "file_path", "filePath", "file", "url"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// notify 发一帧通知给 sidecar。
func (e *Engine) notify(method string, params any) {
	e.mu.Lock()
	rpc := e.rpc
	e.mu.Unlock()
	if rpc == nil {
		return
	}
	_ = rpc.Notify(method, params)
}

// emit 脱敏后喂 sink(网关红线的最后一道防线)。
func (e *Engine) emit(ev event.Event) {
	e.opts.Sink.Emit(e.scrub.Event(ev))
}

// SetApprover 接上审批回调(Controller 造好之后才有,故单独设)。传 nil 恢复
// 非交互语义(Ask 放行)。
func (e *Engine) SetApprover(a func(ctx context.Context, toolName, subject string) (bool, bool, error)) {
	e.mu.Lock()
	e.opts.Approver = a
	e.mu.Unlock()
}

// SetDecider 接上权限策略判定。
func (e *Engine) SetDecider(d Decider) {
	e.mu.Lock()
	e.opts.Decide = d
	e.mu.Unlock()
}

// SetPreEdit 接上写操作前的文件快照回调(checkpoint/rewind 保留 Go 实现)。
func (e *Engine) SetPreEdit(f func(name string, args json.RawMessage)) {
	e.mu.Lock()
	e.opts.PreEdit = f
	e.mu.Unlock()
}
