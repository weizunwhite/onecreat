> 归档自 2026-08-23 调研会话,只读事实快照,以当日代码为准

# 双引擎可行性调研:dsh + pi 并存、用户可切换

调研日期:2026-08-23
调研范围(**全程只读**):
- `/Users/localwork/06_System/onecreat`(分支 main-v2,HEAD `71e6f2a9`)
- `/Users/localwork/04_project/NASApp/brain`(pi 的一手集成参考)
- 本机 pi 安装:`NASApp/brain/node_modules/@earendil-works/pi-coding-agent@0.84.2`(含 `docs/` 全套官方文档)

**一句话结论**:pi 能接。它的 `tool_call` 扩展钩子是 **async + 可 block + 错误即拦截(fail-safe)**,加上 RPC 模式自带的 **extension UI 请求/响应子协议**(扩展发 `extension_ui_request` → 阻塞等客户端回 `extension_ui_response`),构成了一条**在带内、可阻塞、可 fail-closed** 的"工具执行前 → Go 侧裁定 → 拒绝则不执行"通道 —— 与 dsh 的 `tools/pre-execute` 在语义上等价,因此 pi 能合法声明 `CapGatedTools`,`boot.requireToolGating` 会放行。代价是:pi 迭代极快(近三个月 20 个版本)、安装体积 139MB、Windows 需要 bash、模型/厂商名有多处泄漏面(比 dsh 更多),以及 rpc 模式下**一个进程只有一条会话**。

---

## 1. OneCreat 引擎接缝的精确契约

### 1.1 接口(`internal/engine/engine.go`,176 行)

```go
type TurnRequest struct {
    Input string   // 已由应用策略组装完毕的文本(@ 引用已展开、Compose 已注入)
}

type TurnEngine interface {
    Start(ctx context.Context, req TurnRequest) (TurnHandle, error)
}

type TurnHandle interface {
    Cancel() error                  // 尽力而为
    Wait(ctx context.Context) error // 阻塞到这一轮真正结束,返回该轮错误
}
```

三个可选接口(不实现 = 默认最保守):

| 接口 | 方法 | 不实现的后果 |
|---|---|---|
| `engine.Named` | `EngineName() string` | 名字记成 `"unknown"` |
| `engine.Capable` | `Supports(Capability) bool` | **什么能力都没有**(未声明即不支持) |
| —(便利实现) | `engine.Set` = `map[Capability]bool`,自带 `Supports` / `Names()` | — |

`engine.Set` 是给适配器现成用的 `Capable` 实现,pi 适配器照抄即可。

### 1.2 Capability 枚举(共 6 条,`All()` 给出固定顺序)

| 常量 | 字符串 | 语义(按源码注释的精确口径) |
|---|---|---|
| `CapStreaming` | `streaming` | 文本/推理/工具进度增量流向 sink,而不是结束时给一坨 |
| `CapApproval` | `approval` | 引擎会在**执行工具前停下来等审批** |
| `CapResume` | `resume` | 引擎的会话状态可以从一个会话标识接着跑 |
| `CapFork` | `fork` | **OneCreat 侧的消息日志就是模型可见历史的真源**(rewind/branch/compact 依赖它) |
| `CapHostedTools` | `hosted-tools` | 工具由 **OneCreat 这个进程**执行,因此必然过 `internal/toolpolicy` 流水线 |
| `CapGatedTools` | `gated-tools` | 工具在**引擎自己的进程**执行,但每次调用**派发之前**阻塞等 OneCreat 的 `toolpolicy.Pipeline.Before` 裁决 |

`CapHostedTools` 与 `CapGatedTools` 的差别被源码明确定义为:**执行位置不同,门禁时点相同**;安全上等价,拆成两条是为了将来"必须工具在本进程"的场景(共享文件句柄之类)不踩空。

### 1.3 fail-closed 判据(`internal/boot/engine.go:127-142`)

```go
func requireToolGating(e engine.TurnEngine) error {
    if engine.Supports(e, engine.CapHostedTools) || engine.Supports(e, engine.CapGatedTools) {
        return nil
    }
    return fmt.Errorf("engine = %q 暂不可用:%w …请改回 engine = \"native\"。", …)
}
```

关键:**没有任何配置项可以绕过它**(源码注释:"一个能被一行配置关掉的安全门等于没有门")。因此**任何第三个引擎要能被装配,只有一条路:证明自己是 hosted 或 gated**。

### 1.4 boundary_test 的硬约束(`internal/engine/boundary_test.go`,222 行)

用 AST 钉死四件事,**pi 适配器必须同样满足**:

1. `TurnEngine` 只能有 `Start` 一个方法;
2. `TurnHandle` 只能有 `Cancel` + `Wait`;
3. `TurnEngine`/`TurnHandle`/`Capable` 上不得出现这 14 个应用策略方法名:
   `Approve PendingApprovals History Resume Fork Rewind Plan SetPlanMode NewSession SessionPath Compact Branch Submit Send`;
4. 包依赖禁令:
   - `internal/engine` 本身不得 import `control / toolpolicy / permission / checkpoint / evidence / memory / billing / hook / skill / tool / plugin`,**连 `internal/agent` 都不行**;
   - `internal/engine/dsh` 不得 import 上面那 11 个策略包(`assertNoImports(t, "dsh", applicationPolicyPkgs)`)。

⚠️ **注意**:第 4 条目前是 `assertNoImports(t, "dsh", …)` **写死了目录名**。新增 `internal/engine/pi` 后**必须在 boundary_test.go 里加一条 `TestPiAdapterIsNotASecondCore`**,否则新引擎不受守卫 —— 这是最容易漏的一步。

另外 `boundary_test.go` 里已经有一组"说实话"用例(`TestNativeDeclaresFullCapabilities` / `TestDSHDoesNotClaimWhatItCannotDo` / `TestUndeclaredCapabilitiesFailClosed`),pi 也应加一条对称用例。

### 1.5 事件如何回流到 `event.Sink`

引擎**不经过** `TurnHandle` 回传事件。sink 是**构造引擎时绑定**的(`dsh.Options.Sink`),引擎自己在通知处理 goroutine 上 `sink.Emit(...)`。

- native:`agent.Agent` 直接 emit,sink 由 `boot` 传;
- dsh:`Engine.emit(ev)` = `opts.Sink.Emit(e.scrub.Event(ev))`,**脱敏是 emit 的最后一步**;
- 映射由 `mapper.Map(RawSessionEvent) []event.Event` 负责(纯函数,可单测)。

事件种类(`internal/event`):只有 `reasoning / text / tool_progress` 是 ephemeral,**其余全部 durable**;新增 kind 必须在 `internal/eventwire.KindNames` 注册,`TestKindNamesCoversEveryDeclaredKind` 会因此变红。**pi 适配器最好不新增 kind**,把 pi 事件映射到既有的 `TurnStarted / Text / Reasoning / Message / Usage / ToolDispatch / ToolResult / Notice` 上(dsh 就是这么做的)。

### 1.6 session / transcript 的归属

三层清晰分工:

| 层 | 归属 | 说明 |
|---|---|---|
| **身份与元数据** | `internal/session.Record` | id / **engine** / `Store`(引擎的转录引用,registry 从不解析)/ workspace / title / kind / 时间戳。索引落在 `.sessions.json` |
| **转录本身** | 引擎 | native = `agent.Session` 的 JSONL;dsh = sidecar 自己的 store(`DSH_SESSION_ROOT`) |
| **Go 侧镜像(投影)** | `control.sessionStore` + `agent.Session` | dsh 每轮把 user/assistant 文本写进 `Options.Session`,只为 History / 标题 / 前端恢复。**不是真源** |

判据函数(`internal/control/controller.go:371`):

```go
func enginePersistsTranscript(e engine.TurnEngine) bool {
    if e == nil { return true }
    return engine.Supports(e, engine.CapResume)   // 有 resume = OneCreat 该写一份转录文本
}
```

`session.EngineNative = "native"` 是目前**唯一**的常量(`internal/session/session.go:48`);dsh 的名字是 `dsh.Name = "dsh"` 走 `engine.NameOf` 直接写入,没有对应常量。加 pi 时建议统一补 `EngineDSH` / `EnginePi` 常量,或者维持现状(靠 `engine.NameOf`)—— 现状能跑,不是阻塞项。

### 1.7 能力的**执行**(不只是声明)——`internal/control/capability.go`

```go
func (c *Controller) requireCap(op string, cap engine.Capability) error {
    if c.engine == nil { return nil }        // 未装配引擎的测试路径
    return engine.Require(c.engine, op, cap) // 返回 *engine.UnsupportedError
}
```

调用点(**全部在碰任何状态之前**):

| 操作 | 要求的能力 | 位置 |
|---|---|---|
| `Fork` | `CapFork` | `branches.go:195` |
| `Branch`(新建分支) | `CapFork` | `branches.go:251` |
| `SwitchBranch` | `CapFork` | `branches.go:309` |
| `Compact`(压缩上下文) | `CapFork` | `controller.go:414` |
| `Rewind`(会话回退) | `CapFork` | `controller.go:514` |
| `Summarize` | `CapFork` | `controller.go:570` |
| `NewSession` | `CapResume` | `controller.go:460` |
| `Resume`(恢复历史会话) | `CapResume` | `controller.go:828` |

`Controller.Supports(cap)` 是只读查询;`internal/serve/serve.go:398` 把整张能力表编码给前端(`caps[string(c)] = s.ctrl.Supports(c)`)。前端据此禁用入口,但**后端仍独立校验**(UI 不是安全边界)。

### 1.8 一轮的调用形状(`runEngineTurn`,controller.go:380)

```go
h, err := c.engine.Start(ctx, engine.TurnRequest{Input: input})
if err != nil { return err }
defer func() { _ = h.Cancel() }()   // 正常跑完是空操作
return h.Wait(ctx)
```

取消的**唯一触发源是 ctx**;`Cancel()` 只是"把 ctx 取消这件事传达给不会随 ctx 一起死的引擎"。`Wait` **绝不能**在 ctx 取消时提前返回(会撞上"Session 单写者 + 同 goroutine 免锁读"的内核红线)。

### 1.9 dsh 逐项如何满足

| 契约点 | dsh 的做法 | 源码 |
|---|---|---|
| `Start` 立即返回 | 派生 ctx + goroutine 跑 `inner.Run(cctx, input)`,与 native 逐字同形 | `turn_engine.go:82` |
| `Cancel` | `sync.Once` 里做**两件事**:`inner.Cancel()`(经 wire 发 `onecreat/session.cancel`)+ 派生 ctx 的 cancel | `turn_engine.go:110` |
| `Wait` | `<-h.done` 后返回 err,不理 ctx | `turn_engine.go:122` |
| `CapStreaming` | `assistant/chunk` 的 `text-delta`/`reasoning-delta` → `event.Text` / `event.Reasoning` | `mapper.go` |
| `CapApproval` | dsh-tools 调度器在派发前跑 `tools/pre-execute` waterfall;控制面插件 `await` 到 Go;`handlePreExecute` 跑策略 + 阻塞等审批 | `sidecar.go:840`、`dsh/plugins/control/index.js:~260` |
| `CapResume` | 会话 id 由 Go 会话文件路径 SHA1 派生;`onecreat/session.load` 从 dsh store 恢复 | `sidecar.go:203/240`、control 插件 `session.load` |
| `CapGatedTools` | 同 `CapApproval`,拒绝时工具**不执行**;通道断/取消一律 deny | control 插件 `tools/pre-execute` 的 `catch { return {kind:'deny'} }` |
| **不**声明 `CapFork` | 会话真源在 dsh 侧,照本地镜像 rewind/branch/compact 会让两边悄悄对不上 | `turn_engine.go:32-45` |
| **不**声明 `CapHostedTools` | 工具不在 OneCreat 进程里跑 | 同上 |
| 事件回流 | `Options.Sink` 构造时绑定,`emit()` 先 `scrub.Event` 再 Emit | `sidecar.go:876` |
| session 归属 | dsh store 是真源,`Options.Session`(`agent.Session`)是投影 | `sidecar.go` `flushPending` |

**⚠️ 已发现的一处现存缺口(与 pi 无关但影响判断)**:`dsh.Engine.BindSession(path)` / `NewSession(path)` / `SetPlanMode(bool)` / `TurnEngine.Inner()` 这几个方法**在装配根和 Controller 里没有任何调用点**(全树 grep `dsh\.` 与 `BindSession|Inner()` 只命中包内与注释)。后果:

- dsh 会话 id 实际走的是 `SessionID()` 的懒惰兜底 → `BindSession("")` → **进程内随机 `oc-<hex>`**,并没有绑到 Go 的会话文件路径上;
- 也就是说 `CapResume` 声明成立(dsh 侧机制是通的、`06 §2.4d` 实测过跨进程 resume),但**装配层没把它接上**,重开 OneCreat 打开老会话时 dsh 侧不会 resume 到同一条日志;
- `onecreat/planMode.set` 这条 wire 方法目前也是死的(计划模式实际经 `dshDecider` → `Pipeline.Before` 的 plan-mode 只读门生效,这是**正确且有意**的设计,见 `dshengine.go:148` 的注释),所以不是 bug,只是 wire 上有冗余。

**不确定度**:我只做了 grep + 阅读,没跑 e2e;有可能存在我没找到的间接调用(例如通过反射/接口断言)。建议实机验证一次"关掉 OneCreat → 重开 → 打开同一条会话 → 问它上一轮说了什么"。

---

## 2. dsh 适配器实现清单(pi 适配器的模板)

### 2.1 文件与职责

| 文件 | 行 | 职责 | 对 pi 的可复用度 |
|---|---:|---|---|
| `internal/engine/dsh/protocol.go` | 204 | wire 常量(方法名/通知名)+ 所有 DTO 结构体 | **0%**(协议完全不同) |
| `internal/engine/dsh/linerpc.go` | 211 | newline-delimited JSON-RPC 2.0 客户端:读循环、`Call` 等响应、`Notify`、pending map、16MB 单帧上限、入站请求回 -32601 | **~60%**(pi 不是 JSON-RPC 2.0,是扁平 `{"type":...}` 命令;但**JSONL 分帧 + id 关联 + pending map + 读循环**这套骨架照抄) |
| `internal/engine/dsh/tailbuffer.go` | 34 | 只留最后 N 字节的 stderr 尾缓冲(并发安全) | **100%** |
| `internal/engine/dsh/scrub.go` | 62 | `Scrubber`:把真实 provider/model/网关 URL 从事件文本里替换成占位符 | **100%** |
| `internal/engine/dsh/mapper.go` | 284 | dsh 会话事件 → `[]event.Event`;`request/header`+`request/context` **直接丢弃**;usage 口径换算 | **0%**(要重写,但**结构可照抄**:纯函数 + 单测) |
| `internal/engine/dsh/runtime.go` | 142 | 解析"用什么 node、在哪个 runtime 目录、带什么参数"。三级查找:配置 > 主程序旁 `runtime/dsh` > 从 cwd 向上找 `dsh/` | **~80%**(同一套查找逻辑改个目录名) |
| `internal/engine/dsh/sidecar.go` | 896 | 引擎本体:进程生命周期、`initialize` 握手、turn 状态机、通知分派、审批桥/工具桥/预执行桥、会话镜像、凭证轮换、子进程 env | **~40%**(进程管理/turn 状态/桥接骨架可复用;协议交互全换) |
| `internal/engine/dsh/turn_engine.go` | 123 | `TurnEngine`/`TurnHandle` 实现 + `caps` 声明 | **~95%**(几乎逐字模板) |
| **小计(非测试)** | **1956** | | |

测试侧:

| 文件 | 行 | 性质 |
|---|---:|---|
| `linerpc_test.go` | 179 | 纯 Go 单测 |
| `mapper_test.go` | 88 | 纯函数单测 |
| `bridge_test.go` | 209 | **假 sidecar** 的桥接单测 |
| `recorder_test.go` | 53 | 证据记账 |
| `scrub_test.go` | 54 | 脱敏 |
| `gateway_test.go` | 217 | 网关红线 |
| `e2e_gate_test.go` | **372** | **真 sidecar + httptest 假网关**:deny / ask 批 / ask 拒 / 计划模式 / 取消 fail-closed 五条,断言"文件真的没被写" |
| `e2e_gateway_test.go` | 87 | 真 sidecar 的档位/凭证轮换 |
| `e2e_reasoning_test.go` | 104 | 真 sidecar 的 CoT 每轮回传 |

装配侧:

| 文件 | 行 | 职责 |
|---|---:|---|
| `internal/boot/engine.go` | 142 | `engineSpec` / `dshProbe` / `selectEngine`(switch on name)/ `requireToolGating` |
| `internal/boot/dshengine.go` | 276 | `engineName`(优先级解析)、`dshBrandSecrets`、`buildDSHEngine`、**`dshDecider`(接 `toolpolicy.Pipeline.Before`)**、`dshRecorder`、`dshToolInvoker`、`dshTierFunc`、硬件 MCP 路径解析 |

TS sidecar 侧:

| 文件 | 行 | 职责 |
|---|---:|---|
| `dsh/plugins/control/index.js` | 422 | 自己拥有 stdio transport,内部复用官方 `HarnessSdkJsonRpcServer`,补 cancel/审批桥/计划模式/resume/inject/凭证轮换/工具桥/**预执行钩子** |
| `dsh/plugins/gateway/index.js` | 77 | 自命名 provider 路由 `onecreat-gateway`,复用官方 OpenAI 兼容传输,`models: []` 关 catalog 广播,base URL/key 只从环境读 |
| `dsh/profiles/onecreat.cordis.yml` | ~110 | 组合清单;**故意不含** jsonrpc-server / UI / 凭证面 / model selector 包;`includeHarnessIdentity: false` 关掉"我是 DeepSeek Harness"开场白 |
| `dsh/package.json` + `pnpm-lock.yaml` | — | 版本**锁死到 `0.1.0-rc.8`**,lock 入仓 |

### 2.2 建议抽出的"sidecar 引擎基类"(`internal/engine/sidecar/`)

**先做 pi、后做重构**——不要为了抽象先重构 dsh(会同时改两处、两边都不稳)。pi 跑通并绿掉门禁 e2e 之后,再把以下五块提上去:

1. **进程管理**(`proc.go`,~180 行):runtime 目录三级查找、node 解释器解析、`exec.Command` + stdio 管道、stderr tailBuffer、`Kill()` 的 SIGINT→2s→SIGKILL 梯、`Shutdown` 的优雅关闭。**dsh 与 pi 完全同形**。
2. **JSONL 行传输**(`lines.go`,~120 行):`bufio.Scanner` + 16MB buffer + 畸形行忽略 + 写侧串行化 + `Wait() <-chan error`。上面各自贴自己的协议编解码(dsh 贴 JSON-RPC 2.0,pi 贴扁平 `{"type"}`)。
3. **脱敏**(`scrub.go` 直接上提,62 行,零改动)。
4. **`TurnHandle` 模板**(`handle.go`,~50 行):`cancel + once + done + err`,`Wait` 不理 ctx。native/dsh/pi 三家一模一样。
5. **门禁回调契约**(`gate.go`,~40 行):`type Decider func(name string, args json.RawMessage) (decision, reason string)` + `DecisionAllow/Ask/Deny` 三常量 + `Recorder`(证据记账三闭包)+ `ToolInvoker`。**这是 A14 的关键**:引擎层只收闭包,`internal/boot` 在闭包里做 `toolpolicy` / `evidence` / `tool.Registry` 的活。dsh 已经是这个形状,pi 照抄即可,`boot.dshDecider` 可以直接泛化成 `boot.gateDecider(pipeline, reg, readOnlyOf)`。

**必须保持 dsh 专属**(不要硬抽):

- `protocol.go` / `mapper.go`(协议 DTO 与事件映射);
- `session.load` / `credentials.set` / `inject` / `planMode.set` 这几条 OneCreat 自补的 wire 方法(pi 有各自不同的原生对应物);
- cordis profile 与两个 JS 插件;
- `dshBrandSecrets` 的品牌串清单(pi 的泄漏面不同,见 §4.4)。

**抽象的边界纪律**:基类包 `internal/engine/sidecar` **也必须**进 `boundary_test.go` 的 `assertNoImports` 名单,否则它会变成绕过守卫的后门。

---

## 3. pi 的事实清单

版本:**`@earendil-works/pi-coding-agent@0.84.2`**,发布 2026-08-14。
安装位置(本机唯一一份):`/Users/localwork/04_project/NASApp/brain/node_modules/@earendil-works/pi-coding-agent`。
**全局没装**(`npm ls -g` 无 pi,`which pi` 未找到;`~/.pi/agent/` 只有空的 `auth.json`/`models-store.json` 和一个测试用 session 目录)。
仓库:`github.com/earendil-works/pi`(monorepo `pi-mono`),作者 Mario Zechner,MIT,`engines.node >= 22.19.0`。
子包:`pi-agent-core` / `pi-ai` / `pi-client` / `pi-protocol` / `pi-tui`(同版本号)。
`bin.pi = dist/cli.js`;另有 `./rpc-entry` 与 `./client` 两个 export 入口。

### 3.1 版本节奏与体积(风险量化)

| 事实 | 值 | 来源 |
|---|---|---|
| CHANGELOG 里的版本数 | **270 个** | `CHANGELOG.md`(527KB) |
| 近三个月发布 | 0.79.5(6/16)→ 0.84.2(8/14),**约 20 个版本 / 9 周** | `grep "^## \["` |
| 安装体积 | **139MB**(其中 `node_modules` 124MB,含 aws-sdk / google / anthropic 三家 SDK) | `du -sh` |
| 对比:dsh 组合包 | **90MB** | `du -sh dsh` |
| 语义化版本 | 0.x,**minor 号带破坏性变更**(0.84.0 有 "Changed" 段) | CHANGELOG |

结论:pi 的迭代速度**比 dsh 的 rc 还快**,而 dsh 已经因为 developer preview 被要求"锁死精确版本 + 升级必过 e2e 门禁"。pi 必须走同样甚至更严的锁版本策略。

### 3.2 协议:`pi --mode rpc`(来源:`docs/rpc.md`,1589 行)

- **传输**:stdin 收命令、stdout 出事件,**严格 JSONL,只按 `\n` 切**。文档明确写:"Node `readline` is not protocol-compliant … it also splits on `U+2028` and `U+2029`, which are valid inside JSON strings"(`rpc.md:33-40`)。NASApp 因此手写了分帧器(`pi-daemon/frame.ts:createJsonlFramer`)。**Go 侧用 `bufio.Scanner` 天然只按 `\n` 切,没有这个坑**,但要把 buffer 调大(dsh 的 16MB 上限可照抄)。
- **不是 JSON-RPC 2.0**:命令是扁平对象 `{"id":"req-1","type":"prompt","message":"..."}`;响应是 `{"id":"req-1","type":"response","command":"prompt","success":true,"data":{...}}`;事件是 `{"type":"agent_start"}` 之类,**一般不带 id**(例外:`bash_execution_update` 带原命令的 id)。
- **一个进程 = 一条会话**。换会话靠 `switch_session`(带 sessionPath)或重启进程。NASApp 的注释:"换会话 → 没别的办法:abort + kill,再以新 `--session-id` spawn(Pi 是 resume-or-create)"(`daemon.ts:596`)。

**命令清单(与 OneCreat 契约相关的)**:

| 命令 | 用途 | 对应 OneCreat 概念 |
|---|---|---|
| `prompt {message, images?, streamingBehavior?}` | 发起一轮 | `TurnEngine.Start` |
| `steer {message}` / `follow_up {message}` | 运行中插话 / 排队 | dsh 的 `onecreat/inject` |
| `abort` | 中止当前操作 | `TurnHandle.Cancel` |
| `new_session` / `switch_session {sessionPath}` | 新建/切换会话 | `Controller.NewSession` / `Resume` |
| `fork {entryId}` / `clone` / `get_fork_messages` | **原生 fork** | `Controller.Fork`(但真源在 pi 侧) |
| `compact {customInstructions?}` / `set_auto_compaction` | **原生压缩** | `Controller.Compact`(同上) |
| `get_state` / `get_messages` / `get_entries {since}` / `get_tree` | 状态与转录 | History / 会话投影 |
| `get_session_stats` | token/成本/上下文占用 | 计费旁证 |
| `set_model` / `cycle_model` / `get_available_models` | 换模型 | **红线相关,见 §3.6** |
| `set_thinking_level` | 思考档 | 档位 |
| `bash` / `abort_bash` | 客户端直接跑 shell | OneCreat 用不到 |
| `set_session_name` | 会话标题 | `session.Record.Title` |
| `set_auto_retry` / `abort_retry` | 自动重试 | — |

**`get_entries {since}` 是个亮点**:"an entry id works as a durable cursor … pass the last entry id you have seen as `since` to get only entries strictly after it, even across client restarts",响应还带 `leafId`。这比 dsh 的"全量事件通知流"更容易做**断线重连补齐**,与 OneCreat 的 `eventstream` 序列号 + `/snapshot` 重同步模型天然契合。

**事件清单**(`rpc.md` 的 Event Types 表,20 种):
`agent_start` / `agent_end` / `agent_settled` / `turn_start` / `turn_end` / `message_start` / `message_update` / `message_end` / `bash_execution_update` / `tool_execution_start` / `tool_execution_update` / `tool_execution_end` / `queue_update` / `compaction_start` / `compaction_end` / `auto_retry_start` / `auto_retry_end` / `summarization_retry_*` ×3 / `extension_error`。

`message_update.assistantMessageEvent` 的 delta 类型:`text_start/text_delta/text_end`、**`thinking_start/thinking_delta/thinking_end`**(CoT 流式,正是 dsh rc.8 升级的动机)、`toolcall_start/toolcall_delta/toolcall_end`。顶层还带累计 `usage`(input/output/cacheRead/cacheWrite/totalTokens/cost)。

→ **映射到 OneCreat 的 event.Kind 是直接的**,不需要新增 kind:
`turn_start`→`TurnStarted`;`text_delta`→`Text`;`thinking_delta`→`Reasoning`;`message_end`→`Message`+`Usage`;`tool_execution_start`→`ToolDispatch`;`tool_execution_end`→`ToolResult`;`agent_settled`→ 一轮收敛(相当于 dsh 的 `session.status: idle`);`extension_error`/`auto_retry_*` → `Notice`。

### 3.3 **工具执行前的拦截钩子**(核心问题)——`docs/extensions.md`

生命周期图(`extensions.md:280-310`)明确写:

```
│   │   LLM responds, may call tools:            │
│   │     ├─► tool_execution_start               │
│   │     ├─► tool_call (can block)              │
│   │     ├─► tool_execution_update              │
│   │     ├─► tool_result (can modify)           │
│   │     └─► tool_execution_end                 │
```

`tool_call` 的规格(`extensions.md:751-790`):

> Fired after `tool_execution_start`, **before the tool executes**. **Can block.**
> - `event.input` is mutable … Mutations affect the actual tool execution
> - Return values from `tool_call` control blocking via `{ block: true, reason?: string, terminate?: boolean }`
> - Before `tool_call` runs, pi waits for previously emitted Agent events to finish draining

处理器签名是 **`async (event, ctx) => …`**,即**允许 await**。示例代码里直接就有 `const ok = await ctx.ui.confirm("Dangerous!", "Allow rm -rf?"); if (!ok) return { block: true, reason: "Blocked by user" };`(`extensions.md:70-76`)。

**Fail-safe 语义有官方保证**(`extensions.md:2894`):

> - `tool_call` errors **block the tool (fail-safe)**

这一条至关重要:handler 抛异常 = 工具被拦。所以"Go 通道断了 → 抛错 → deny"是**语言级保证**,不用靠我们自己 catch(当然仍然应该显式 catch 并返回 block,与 dsh 控制面插件同形)。

并行工具:"sibling tool calls from the same assistant message are **preflighted sequentially**, then executed concurrently" —— 即 `tool_call` 钩子是**串行**跑的,与 dsh 的 waterfall 一样,Go 侧不必处理并发裁定交错。

### 3.4 **裁定通道**:extension UI 子协议(pi 的"官方桥")

`docs/rpc.md` 的 "Extension UI Protocol" 一节(1140-1330)是**决定性发现**:

> Extensions can request user interaction via `ctx.ui.select()`, `ctx.ui.confirm()`, etc. In RPC mode, these are translated into a **request/response sub-protocol on top of the base command/event flow**.
> - **Dialog methods** (`select`, `confirm`, `input`, `editor`): emit an `extension_ui_request` on stdout and **block until the client sends back an `extension_ui_response`** on stdin with the matching `id`.
> - **Fire-and-forget methods** (`notify`, `setStatus`, …): emit but do not expect a response.
> - If a dialog method includes a `timeout` field, the agent-side will **auto-resolve with a default value** when the timeout expires.

请求形状:
```json
{"type":"extension_ui_request","id":"uuid-1","method":"input","title":"…","placeholder":"…"}
```
响应形状(客户端写 stdin):
```json
{"type":"extension_ui_response","id":"uuid-1","value":"…"}          // select/input/editor
{"type":"extension_ui_response","id":"uuid-2","confirmed":true}      // confirm
{"type":"extension_ui_response","id":"uuid-3","cancelled":true}      // 任意 dialog → undefined/false
```

且 `ctx.mode === "rpc"` 时 **`ctx.hasUI === true`**,因为 dialog 方法在 RPC 下**是可用的**。

→ 这就是 **pi 版的 `onecreat/tool.preExecute` ↔ `onecreat/tool.preExecute.done`**,而且**是官方协议的一部分,不需要我们自己抢 stdio**(dsh 那边为了补方法不得不让控制面插件自己拥有 transport 并内嵌官方 server —— pi 这里省掉了这一整块复杂度)。

### 3.5 cancel / resume / fork / compact

| 能力 | pi 的原生支持 | 证据 |
|---|---|---|
| **cancel** | ✅ `abort` 命令(rpc);扩展侧 `ctx.abort()`;`ctx.signal` 在 `tool_call`/`tool_result` 等 handler 里可用,可让 handler 内的 `fetch` 一起被取消 | `rpc.md#abort`、`extensions.md:994-1001` |
| **resume** | ✅ `--session <path\|id>`(resume-or-create)、`--session-dir`、`switch_session` 命令、`PI_CODING_AGENT_SESSION_DIR` 环境变量 | `sessions.md`、`rpc.md#switch_session`、`environment-variables.md` |
| **fork** | ✅ **原生**:`fork {entryId}`、`clone`、`get_fork_messages`,会话本身就是**带 `id`/`parentId` 的树**,支持原地分支不建新文件 | `rpc.md#fork`、`session-format.md` |
| **compact** | ✅ **原生**:`compact {customInstructions?}`、`set_auto_compaction`,返回 summary + `firstKeptEntryId` + token 前后 | `rpc.md#compact` |
| **steer / follow_up** | ✅ 两种排队模式(`all` / `one-at-a-time`) | `rpc.md#set_steering_mode` |

⚠️ 但按 OneCreat 现行 `CapFork` 的**定义**("OneCreat 自己的消息日志**就是**模型可见历史的真源"),pi 与 dsh 一样**不该声明 `CapFork`** —— 它的转录真源在 pi 的 JSONL 里。pi 的 fork/compact 是"**委托式**"的,要么不声明,要么给 `CapFork` 拆一个新口径(见 §6.4)。

### 3.6 provider 自定义与"藏住真实模型名"

三条路,**推荐第二条**:

1. **`models.json`(纯配置,无需扩展)** —— `docs/models.md`:
   ```json
   { "providers": { "<任意名>": {
       "baseUrl": "https://gateway.example/v1",
       "api": "openai-completions",
       "apiKey": "$MY_API_KEY",
       "authHeader": true,
       "models": [ { "id": "tier-2", "name": "高级", "api": "openai-completions",
                     "reasoning": true, "input": ["text"], "contextWindow": …, "maxTokens": …,
                     "cost": {...} } ] } } }
   ```
   `apiKey` 支持三种取值:字面量、`$ENV` 插值、**`!<shell 命令>`(models.json 里的 shell 命令"resolved at request time")**。
   **NASApp 已经在用这一条**:`pi-daemon/models.deepseek.json` + `ensurePiModelsJson(piConfigDir)`(每次 spawn 前 upsert 进 `<PI_CODING_AGENT_DIR>/models.json`,幂等、只覆盖同 id、其它 provider 原样保留)。

2. **扩展 `pi.registerProvider()`(`docs/custom-provider.md`)** —— 两种形式:
   - 完整 pi-ai `Provider`:`createProvider({ id, name, baseUrl, auth:{apiKey:{login,resolve}}, models: [], api: openAICompletionsApi() })`;`resolve()` 是 **async 回调**,每次取凭证都会调 —— 这正好解决"平台 token 约 50 分钟过期、子进程 env 是 spawn 快照"的问题(dsh 是靠自补 `onecreat/credentials.set` 解决的);
   - 遗留 config 形式:`pi.registerProvider("my-provider", { name, baseUrl, apiKey:"$X", api:"openai-completions", models:[…] })`,还能只改 `baseUrl`/`headers` 覆盖已有 provider。
   工厂函数**可以是 async**,"pi waits for the factory before startup continues"。

3. `--api-key <key>` 命令行(会进 `ps` 输出,**不要用**)。

**关闭模型目录广播**:`models: []`(与 dsh gateway 插件同招)。加上 `--no-extensions`(只关自动发现,`-e` 显式加载仍生效)、隔离 `PI_CODING_AGENT_DIR`(NASApp 用 `runtime/pi-agent`),用户就看不到别的 provider。

**pi 特有的泄漏面(比 dsh 多,必须逐条堵)**:

| 泄漏点 | 说明 | 缓解 |
|---|---|---|
| `AssistantMessage` 带 `api` / `provider` / `model` 三个字段 | 在 `turn_end` / `message_end` / `get_messages` / session JSONL 里都有 | provider 名固定为 `onecreat-gateway`、model id 固定为 `tier-N`(与 dsh 同招);mapper 里不透传这三个字段 |
| **bash 工具注入 `PI_PROVIDER` / `PI_MODEL` / `PI_REASONING_LEVEL` / `PI_SESSION_FILE` 到每条 LLM 跑的命令** | `environment-variables.md` 甚至**明文教模型**:"When asked which model or provider is running, inspect these variables" | 值本身是我们的假名(可控);但 `PI_SESSION_FILE` 让模型能 `cat` 自己的 JSONL。JSONL 里也只有假名,可接受。若要更严,可用扩展重注册 `createBashTool(cwd, { exposeSessionEnvironment: false })` 覆盖内建 bash |
| `get_available_models` / `set_model` / `cycle_model` 会回真实目录 | RPC 命令由**我们**发,不发就没事;但若前端将来接了"模型列表",会露 | Go 适配器**不实现**这三条命令的转发 |
| 错误体 | provider 错误会带上游 URL/品牌 | 复用 `scrub.Scrubber`,`SecretsToScrub` 换成 pi 的品牌串集合 |
| `--tools`/系统提示里的工具描述 | 无厂商信息 | — |
| `PI_TELEMETRY` / 启动自更新检查 | 会外联 pi.dev | `PI_OFFLINE=1` + `PI_TELEMETRY=0`(NASApp 已经这么设) |

### 3.7 session 格式与存储

- 路径:`~/.pi/agent/sessions/--<cwd 把 / 换成 ->--/<timestamp>_<uuid>.jsonl`;可用 `PI_CODING_AGENT_SESSION_DIR` 或 `--session-dir` 改。
- 格式:**JSONL 树**,每条 entry 有 `id`/`parentId`,当前位置是"active leaf";version 3(v1 线性 / v2 树 / v3 `hookMessage`→`custom`),**加载时自动迁移**。
- 消息类型:`UserMessage` / `AssistantMessage`(带 api/provider/model/usage/stopReason)/ `ToolResultMessage`(带 details/usage/isError)/ `BashExecutionMessage` / `CustomMessage`。
- 删除:直接删 `.jsonl`(pi 有 `trash` CLI 支持)。

### 3.8 CLI 关键参数(`docs/usage.md`)

| 参数 | 说明 | 对 OneCreat 的意义 |
|---|---|---|
| `--mode rpc` / `--mode json` / `-p` | RPC 常驻 / 一次性 JSON 流 / 一次性 print | 主路径用 rpc;`--mode json` 可做 headless `reasonix run` |
| `--tools <list>` / `-t` | **总允许表**(built-in + extension + custom) | ⚠️ 见下 |
| `--no-builtin-tools` / `--no-tools` | 关内建/全关 | 计划模式的一种粗粒度实现 |
| `-e <path>` / `--extension` | 显式加载扩展 | **门禁扩展的加载方式** |
| `--no-extensions` | 只关自动发现,`-e` 仍生效 | 隔离用户本地扩展(安全必需) |
| `--no-skills` / `--skill <dir>` | 技能发现 | OneCreat 有自己的 `internal/skill` |
| `--no-context-files` / `-nc` | 关 `AGENTS.md`/`CLAUDE.md` 发现 | OneCreat 有自己的 memory 层,**建议关掉避免双重注入** |
| `--system-prompt <text>` | 替换默认提示(context files 与 skills 仍追加) | 下发 `Controller.SystemPrompt()` |
| `--append-system-prompt` | 追加 | — |
| `--session <path\|id>` / `--session-dir` / `--no-session` / `--name` | 会话 | resume / 标题 |
| `--provider` / `--model <pattern>` / `--thinking <level>` | 路由与档位 | 下发假 provider + tier 名 |
| `-a` / `--approve` / `-na` | 项目信任(非交互模式默认按 `defaultProjectTrust`) | ⚠️ `-a` 等于信任项目本地 `.pi/` 资源 —— **OneCreat 应该用 `-na`**,配 `--no-extensions --no-skills --no-context-files`,不让工作区里的文件改变 agent 行为 |

⚠️ **`--tools` 是启动期的总允许表,rpc 模式下不能逐轮换**。NASApp 为此付出的代价是**开两条独立进程**(人工对话泳道 + 只读诊断泳道),README 里写得很直白:"`rpc` 模式下**不能逐轮换工具表**,所以只读需求单独一条进程"。对 OneCreat 的影响:**计划模式(只读门)不能靠 `--tools` 实现**,必须靠 `tool_call` 钩子 block —— 而这恰恰就是我们的门禁通道,所以**不是新增成本**(`Pipeline.Before` 的第一段就是 plan mode 只读门,自动生效)。

### 3.9 Windows

`docs/windows.md`:**pi 在 Windows 上需要一个 bash**,按序查找 `settings.json` 的 `shellPath` → Git Bash `C:\Program Files\Git\bin\bash.exe` → PATH 上的 `bash.exe`(Cygwin/MSYS2/WSL)。"For most users, Git for Windows is sufficient."

→ Windows 用户必须装 Git for Windows,否则 pi 的 bash 工具不可用。OneCreat 现在有 `scripts/windows-package-verify.sh` 与 `windows-native-smoke.ps1`,需要加一条 pi 的 Windows 冒烟。(dsh 侧的 Windows 状况我没验证 —— `dsh-bundle.sh` 有 `runtime/node/node.exe` 分支,说明打算支持;**标为不确定**。)

### 3.10 NASApp/brain 的 pi 集成方式(可直接借鉴的代码落点)

架构(`pi-daemon/README.md`):

```
iOS ──► NAS main.py ──► 大脑 8799 (src/full-mode.ts = 客户端)
                            │  HTTP/SSE(127.0.0.1:8798,Bearer PI_DAEMON_TOKEN)
                            ▼
                     pi-daemon/daemon.ts ──spawn──► pi --mode rpc(常驻,cwd=~/agent-cluster/ops)
                            ▲                                  │
                            └──────── 业务工具桥 ◄──────────────┘
                       (Pi 扩展 oneup-tools.ts → 大脑 /api/full-mode/tool,带一次性 nonce)
```

| 文件 | 行 | 直接可借鉴的东西 |
|---|---:|---|
| `pi-daemon/daemon.ts` | 880 | **`piArgs()`(307-322)是最有价值的一段**:完整的 spawn 参数组合;`restartLane`(换会话只能 abort+kill+重 spawn);`waitForExit` 的 6s 超时;忙/闲状态由 `agent_start`/`agent_settled` 维护 |
| `pi-daemon/frame.ts` | 127 | `createJsonlFramer`(手写分帧,**Go 侧不需要**)、`EventRing`(seq 环形缓冲 + `since(seq)` 回放,**OneCreat 的 eventstream 已有等价物**)、`planPrompt`/`planSteer` 纯函数状态机(**值得抄**:忙时直接 prompt 会被 pi 判错丢消息;闲时 steer 会被 pi 拒) |
| `pi-daemon/pi-env.ts` | 148 | **`buildPiEnv()` 的白名单纪律**:只透传 `PATH/HOME/USER/SHELL/LANG/TMPDIR`,其余显式列举;`leakedCredentialKeys()` 自检函数;`ensurePiModelsJson()` 的 models.json upsert(幂等,失败返回 `'skipped'` 绝不抛 —— "拉不起 Pi 比少一条模型更糟") |
| `pi-daemon/client.ts` | 255 | HTTP/SSE 客户端(OneCreat 用不到,它直接持有子进程) |
| `pi-daemon/sessions.ts` | 192 | `piSessionIdFor(owner, short)` —— 由业务键**确定性派生** session id,**与 dsh 的 `sessionIDFor(path)` 是同一个招式** |
| `ops-template/guard.ts` | 18KB | **本地 block 规则表**:`GUARD_RULES` 数组 + `evaluateGuard` 纯函数,命中就 `{block:true, reason}`,reason 写清"为什么 + 该走哪条路"让模型换路子而不是重试。OneCreat 不需要这个(策略在 Go 侧),但**reason 的写法值得抄**——`toolpolicy` 的 `Block.Output` 已经是这个风格 |
| `ops-template/oneup-tools.ts` | 7KB | **工具桥模式**:扩展启动时向宿主拉 manifest → `pi.registerTool()` 逐个注册 → `execute` 里 `fetch` 回宿主。**这是 OneCreat 的 `complete_step` 该走的路**;它用 HTTP + 一次性 nonce,OneCreat 可以改用 extension UI 子协议免开端口 |
| `src/subagent.ts` | — | 一次性 job 用 `pi -p --mode json`,与常驻 rpc 泳道分开 |

**NASApp 踩过、OneCreat 会重踩的坑(全部有代码注释为证)**:

1. `--tools` 是总允许表,**只写内建那七个,扩展注册的 50 个业务工具会被一起关掉**(`daemon.ts:222-241`,"2026-08-22 验收第一次就踩到")。→ OneCreat 若用 `--tools`,必须把桥接注册的工具名一并写进去。
2. 允许表要写**注册后的名字**(带前缀),不是裸名(同上,"第二次踩到")。
3. 工具名要加前缀防撞:pi 内建叫 `read/bash/edit/write/grep/find/ls`,业务里有 `read_note`/`list_tasks` 之类(`oneup-tools.ts:11`)。→ OneCreat 的 `complete_step` 不撞,但硬件 MCP 的 `mcp__hardware__*` 需要确认。
4. **孤儿 nonce**:宿主重启丢注册表,而常驻 pi 不跟着重启 → 之后每次桥接调用吃 401("2026-08-22 17:53Z 就这么烂了一整轮")。→ **用 extension UI 子协议就完全没有这个问题**(通道随进程生灭)。
5. pi 内建 deepseek 目录里没有目标模型,得靠 `<piConfigDir>/models.json` 补(`pi-env.ts:118-148`)。
6. `TERM=dumb` 防止 pi 以为自己在 TTY 里。

---

## 4. 门禁可行性判定(核心结论)

### 4.1 判定:**成立**。pi 可以合法声明 `CapGatedTools`。

链路:

```
pi 的 agent loop
  └─ tool_execution_start (事件,已发给 Go)
  └─ tool_call 钩子(串行、async、可 block)     ← 我们的 onecreat-gate.ts 扩展
        └─ await ctx.ui.input({ title:"__onecreat_gate__", ... })   ← 官方 dialog 方法,阻塞
              ↓ stdout: {"type":"extension_ui_request","id":"…","method":"input", …}
        [Go] internal/engine/pi 读到 extension_ui_request
              ├─ 解出 toolName + arguments
              ├─ 调 boot 注入的 Decider ──► toolpolicy.Pipeline.Before(ctx, Call{Name,Args,ReadOnly,Preview})
              │      = plan mode 只读门 → 权限门(交互式审批就装在这个 Gate 里,ask 在此阻塞等真人)
              │        → PreToolUse hook → 写前检查点快照
              └─ 回帧 stdin: {"type":"extension_ui_response","id":"…","value":"{\"decision\":\"deny\",\"reason\":\"…\"}"}
        ↑ 扩展拿到 value,decision=="deny" → return { block: true, reason }
  └─ (被 block:工具**不执行**,reason 作为工具结果回给模型)
```

### 4.2 fail-closed 的四个断点,逐个成立

| 断点 | pi 的行为 | 是否 fail-closed |
|---|---|---|
| Go 进程死/管道断 | 扩展的 `ctx.ui.input()` 永远等不到响应 → **挂住**;若 stdin 关闭,pi 侧 dialog 会因传输失效抛错 | **抛错 → 官方保证 "tool_call errors block the tool (fail-safe)"** ✅ |
| Go 明确 deny | `value` 里带 `decision:"deny"` | 扩展 `return {block:true}` ✅ |
| 用户在 dialog 上取消 | 客户端回 `{"cancelled":true}` → `input` 返回 `undefined` | 扩展把 `undefined` 当 deny ✅(必须显式写,**这是唯一要靠我们纪律的一处**) |
| turn 被取消 | `ctx.signal` abort;handler 里 `await` 被打断抛 `AbortError` | 抛错 → 拦截 ✅ |
| dialog 超时 | 若我们**传** `timeout`,pi 自动 resolve `undefined`/`false` | 按 `undefined`=deny 处理 ✅。**建议:ask 场景不传 timeout(等真人),纯策略判定传一个大 timeout(如 60s)兜底** |

**唯一的"挂住不是拒绝"的窗口**:Go 侧还活着但卡死(死锁),扩展会无限等。dsh 有完全相同的性质(它的 `ask()` 传 `timeoutMs=0` 表示不超时)。这在两边都是"挂住 ≠ 放行",安全上可接受,可用性上要靠 turn 超时兜底。

### 4.3 两条实现路线的取舍

| 方案 | 通道 | 优点 | 缺点 |
|---|---|---|---|
| **A. extension UI 子协议(推荐)** | pi 官方的 `extension_ui_request`/`_response`,走同一条 stdio | 零额外端口、零鉴权、随进程生灭(**无 NASApp 的孤儿 nonce 问题**)、协议是官方文档的一部分 | 语义"借用"了 UI dialog(`input` 传 JSON 字符串),略 hack;若 pi 未来改 dialog 语义要跟着改 |
| **B. loopback HTTP + 一次性 nonce** | Go 起 `127.0.0.1:<随机端口>`,扩展 `fetch` | 语义干净、可传大 payload、NASApp 已验证 | 要开监听端口(桌面多标签 = 多端口)、要做鉴权、宿主重启会产生孤儿 nonce、防火墙/杀软可能拦 |

**建议 A 为主,B 作为 fallback 不做**。理由:OneCreat 是要装到老师电脑上的桌面/Web 应用,少开一个监听端口就是少一类支持工单;而且 dsh 那边已经证明"通知对 + id 配对"这个形状 Go 侧写起来很顺。

选 dialog 方法时:用 **`input`**(返回任意字符串)最灵活,可以把 `{decision, reason}` 序列化进 `value`;`confirm` 只能回 bool,表达不了 reason。`title` 用一个不会与真实 UI 冲突的哨兵串(如 `"__onecreat_gate__"`),Go 侧据此识别"这是门禁请求,不是真的要弹窗给用户看"。

### 4.4 还需要在扩展里做的两件事

1. **审批弹窗的去向**:OneCreat 的审批 UI 在 Go/前端侧(`approvalBroker`),而 `Pipeline.Before` 的 Gate 已经内含交互式审批 —— **所以 pi 扩展什么都不用做**,Go 侧在 `Before` 里阻塞等用户就行,与 `boot.dshDecider` 完全同构。dsh 的 `Approver` / `PreEdit` 两个注入点在装配根**根本没被用上**(见 §1.9),pi 照抄这个"只接 Decide 一根线"的形状即可。
2. **`complete_step` 工具桥**:pi 扩展 `pi.registerTool({name:"complete_step", …, async execute(...) { … }})`,execute 里同样用 `ctx.ui.input` 把参数送回 Go 执行(证据引擎是它的裁判)。参数 schema 用 typebox 或直接给标准 JSON Schema(`oneup-tools.ts` 证明标准 JSON Schema 可原样透传)。

### 4.5 e2e 验证是**必须的**,不是可选的

dsh 立下的规矩(`dsh/README.md` "升级必过项"):纯 Go 的假 sidecar 单测只能证明"Go 半边回了什么帧",**证明不了"文件真的没被写"**。pi 同理,而且 pi 的迭代更快。必须建一套对称的:

```sh
ONECREAT_PI_E2E=1 go test ./internal/engine/pi/ -run 'Gate|Tier|Reasoning' -v -count=1 -timeout 300s
```

五条门禁用例照抄 `e2e_gate_test.go` 的结构(假网关 httptest + 真 pi 子进程 + 断言目标文件不存在):deny / ask 批 / ask 拒 / 计划模式写工具被拦 / 取消时 fail-closed。

---

## 5. Capability 矩阵:native / dsh / pi

| 维度 | native | dsh | pi(评估) | 依据 |
|---|---|---|---|---|
| `hosted-tools` | ✅ **声明** | ❌ | ❌ | native.go `caps`;工具在别的进程 |
| `gated-tools` | —(不需要) | ✅ **声明** | ✅ **可声明** | dsh:`tools/pre-execute` waterfall;pi:`tool_call` 钩子 can block + async + 错误即拦截(`extensions.md:751/2894`)+ extension UI 阻塞子协议(`rpc.md#extension-ui-protocol`) |
| `streaming` | ✅ | ✅ | ✅ | pi `message_update` 的 `text_delta`/`thinking_delta`/`toolcall_delta` |
| `approval` | ✅ | ✅ | ✅ | 同 gated-tools 通道,ask 在 `Pipeline.Before` 里阻塞等真人 |
| `CapResume` | ✅ | ✅(声明成立,**装配未接线**,§1.9) | ✅ | pi:`--session <path\|id>` resume-or-create + `switch_session` + `--session-dir` |
| `CapFork`(=OneCreat 日志是真源) | ✅ | ❌ | ❌ | pi 的转录真源在自己的 JSONL 树里。**pi 有原生 fork/clone/get_fork_messages,但那是委托式**,不满足现行口径 |
| **compact**(受 `CapFork` 门控) | ✅ | ❌(拒) | ❌(现行口径下拒)/ ⚠️ pi **原生支持** `compact` 命令 | `controller.go:414` 判 `CapFork`;pi `rpc.md#compact` |
| **rewind — 会话回退**(判 `CapFork`) | ✅ | ❌ | ❌(现行口径)/ pi 原生有树导航 | `controller.go:514` |
| **rewind — 代码回退**(文件检查点) | ✅ | ✅ | ✅ | 检查点是 Go 侧 `internal/checkpoint`,在 `Pipeline.Before` 的最后一步做写前快照,与引擎无关 |
| **模型脱敏** | ✅(`config.ModelPrivacyPolicy` + `provider.Config.Gateway`) | ✅(gateway 插件自命名路由 + `models:[]` + `includeHarnessIdentity:false` + mapper 丢弃 `request/header`/`request/context` + Scrubber 兜底) | ⚠️ **可做,但泄漏面更多**:`AssistantMessage.{api,provider,model}` + bash 工具注入 `PI_PROVIDER`/`PI_MODEL`(文档还明文教模型去读)+ `get_available_models`。全部可控,但需逐条堵 | §3.6 |
| **流式 CoT 回传** | ✅ | ✅(rc.8 的升级动机) | ✅ `thinking_start/delta/end` | `rpc.md#message_update` |
| **多会话并行** | ✅(每标签一个 Controller) | ✅(一个 sidecar 进程可持多个 `sessionId`,`ctx.agents.get(SessionId)`) | ⚠️ **一进程一会话**;换会话要 `switch_session` 或重启进程。桌面多标签 = **每标签一个 pi 进程**(每个 139MB 的 node 进程,内存代价明显) | `daemon.ts:596` 注释;`rpc.md#switch_session` |
| **Windows** | ✅(纯 Go 静态二进制) | ⚠️ 需内置 node;`dsh-bundle.sh` 有 `node.exe` 分支(**未实测,不确定**) | ⚠️ **需要 bash**(Git Bash / Cygwin / MSYS2 / WSL),否则 bash 工具不可用 | `docs/windows.md` |
| 体积 | 0(编进主二进制) | 90MB | **139MB** | `du -sh` |
| 上游稳定性 | 自家代码 | developer preview,rc 间明说可能破坏兼容 | 0.x,**9 周 20 个版本**,minor 带破坏性变更 | dsh README;pi CHANGELOG |
| 官方嵌入支持 | — | JSON-RPC SDK server(功能少,要自补 6 个方法) | **RPC 模式文档 1589 行 + SDK(`AgentSession`)+ `pi-client` 包**,协议完备度明显更高 | `docs/rpc.md`、`docs/sdk.md` |

**一句话对比**:pi 的**协议完备度和文档质量明显优于 dsh**(cancel/resume/fork/compact/tree/cursor 全是原生命令,dsh 那六个方法全是我们自己补的 422 行 JS);dsh 的优势是**模型厂自家 harness + 已经跑通了 + 一进程多会话 + 体积小 50MB**。

---

## 6. 用户切换引擎的设计约束

### 6.1 现状

引擎名解析优先级(`internal/boot/dshengine.go:engineName`):

```
显式 boot.Options.Engine  >  环境变量 ONECREAT_ENGINE  >  配置 cfg.Engine  >  "native"
```

- **未知名字原样返回**,由 `selectEngine` 报错 —— `engine = "dsj"` 这种拼写错**不会**静默回退成 native(源码注释:"用户以为自己在用另一个引擎,实际不是,这比启动失败糟得多")。
- CLI 已有 `--engine` flag(`internal/cli/cli.go:150`),传进 `boot.Options.Engine`;
- **桌面端从不传 `Engine`**:9 个 `boot.Build` 调用点(`tab_runtime.go` ×4、`settings_app.go` ×1、`cli` ×3、`acp` ×1)里,**只有 CLI 传**。桌面因此只能靠进程级 env / 配置文件切换,**没有 UI 开关**。
- 会话记录里带 engine:`control.New` → `newSessionStore(…, engineNameOf(eng), enginePersistsTranscript(eng), …)`,`session.Record.Engine` 落进 `.sessions.json`;
- 前端已经能拿到能力表(`serve.go:398`),桌面状态栏也已显示引擎名(`desktop/app.go:602`,注释:"状态栏显示,测试时分得清")。

### 6.2 一条会话能否中途换引擎?—— **不能,必须硬拒**

理由(全部有代码支撑):

1. **转录真源不同**。native 的真源是 `agent.Session`(OneCreat 的日志);dsh/pi 的真源在 sidecar 侧,Go 侧只是投影。从 native 换到 pi:pi 那边没有这条会话的历史 → 模型失忆;从 pi 换到 native:Go 侧只有"user/assistant 纯文本投影",**丢了全部工具调用、工具结果、thinking 块**,喂回模型等于换了个人。
2. **`session.Record.Workspace` 与 `Kind` 是 write-once**(CLAUDE.md 明载),`Engine` 语义上属于同一类:它决定 `Store` 字段怎么解读,改了它历史就读不回来。
3. `cpBound`(turn → 消息索引)是按 native 日志建立的,换引擎后全部失效;
4. `enginePersistsTranscript` 决定了要不要写转录文本,中途改会产生半真半假的文件。

**结论:`Record.Engine` 应当明确升格为 write-once 字段**,和 workspace/kind 一样,并在 registry 层拒绝更新。这是 pi 落地时应该顺手补的一条约束(现在没人拦)。

### 6.3 切换粒度:推荐 **"按新会话选择 + 全局默认"**,不推荐按工作区

| 方案 | 评价 |
|---|---|
| **全局切换(改配置/env,重启生效)** | 现状。太粗:老师想为一个项目试 dsh 就得改配置重启 |
| **按工作区切换**(`<workspace>/onecreat.toml` 的 `engine`) | ⚠️ **已经能用**(配置解析顺序是 flag > workspace TOML > 全局 TOML > 默认),但**语义别扭**:引擎是"用哪个 agent 内核",不是项目属性;而且同一工作区的不同标签会被强绑同一引擎,失去对照价值 |
| **✅ 按新会话选择(推荐)** | 新建标签/新建会话时选引擎 → 存进 `Record.Engine` → **该会话终身不变**;打开历史会话时按记录里的 engine 装配。这与"引擎决定转录真源"的事实完全对齐,也是唯一不会产生半真半假状态的模型 |
| 全局默认 + 新会话覆盖 | 最终形态:配置里 `engine = "native"` 是**默认值**,新会话可覆盖 |

**落地改动量很小**:
- `boot.Options.Engine` 已存在,桌面的 5 个 `boot.Build` 调用点各加一个字段(从 tab 状态里取);
- `tabManager` 的 tab 状态加一个 `engine string`(与 `workspace` 同级,同样是 write-once);
- 打开历史会话时从 `Record.Engine` 取值传下去;
- `desktop/rpc_surface.go` 加一个 `SetTabEngine`(只在新标签/空会话上允许)并 `go generate ./...` 重生成 bindings;
- 前端按 `Controller.Supports` 的能力表禁用 compact / rewind-conversation / branch 入口(**已有机制**,`serve.go:398`;桌面需要对应加一个 `EngineCapabilities()` 方法,目前桌面只暴露了引擎**名字**,没暴露能力表 —— 这是一处缺口)。

### 6.4 关于 `CapFork` 口径要不要放宽

现在 `CapFork` 一条同时守着四件事:fork / branch / switch-branch / **compact** / **conversation rewind** / summarize。dsh 全拒是对的(它没有原生 fork)。但 **pi 原生有 `fork`/`clone`/`compact`/`get_tree`**,一刀切拒掉会白白丢功能。

两个选择:

- **(推荐,先做)** 维持现状,pi 也不声明 `CapFork`,把这些操作在 pi 下禁用。理由:安全且诚实,`08 §3.2` 刚刚才把 `CapResume`/`CapFork` 拆开、口径收敛,不宜马上再动;
- **(后续可选)** 引入第三条能力 `CapDelegatedHistory`("引擎自己能 fork/compact,但要通过它的命令做"),让 Controller 在拿不到 `CapFork` 时改走"委托路径"(`Compact` → 发 pi 的 `compact` 命令 → 用返回的 summary 重建 Go 侧投影)。**这是一次独立的架构改动,不该塞进 pi 接入里**。

⚠️ 注意 `boundary_test.go` 的 `forbiddenMethods` 里有 `Compact`/`Fork`/`Resume` —— 委托路径**不能**加方法到 `TurnEngine`,只能像 dsh 的 `Inner()` 那样由装配根拿到具体类型再调。这也解释了为什么 `dsh.TurnEngine.Inner()` 存在。

### 6.5 sidecar 依赖的分发

现状(dsh):

- `dsh/package.json` 把 22 个 `@deepseek-ai/dsh-*` 锁到**精确版本** `0.1.0-rc.8`,`pnpm-lock.yaml` 入仓;
- `scripts/web-build.sh` 把 `runtime/node/bin/node`(内置 Node)+ `runtime/dsh/`(组合包闭包)打进发行包;`SKIP_DSH=1` 可跳过("包体小 ~4 倍");`DSH_RUNTIME_DIR` 支持预装 runtime(因为原生模块只能在目标平台装);
- 运行时三级查找(`runtime.go:resolveRuntimeDir`):配置 > 主程序旁 `runtime/dsh` > 从 cwd 向上找 `dsh/`;
- 升级是一件**独立任务**,必过 e2e 三组。

**pi 的建议(照抄 + 两点调整)**:

1. 目录 `pi/`,`package.json` 只有一条依赖 `"@earendil-works/pi-coding-agent": "0.84.2"`(**精确版本,无 `^`**),用 npm(pi 自带 `npm-shrinkwrap.json`,61KB,**上游已经帮我们锁了整棵树** —— 这比 dsh 的情况更好)。`package-lock.json` 入仓。
2. **复用同一个 `runtime/node`**:pi 要 node ≥22.19,dsh 也要 node 20+;发行包里放一份 node、两个 runtime 目录(`runtime/dsh/` + `runtime/pi/`)。
3. 体积:全都打进去 = 90 + 139 + node ≈ **250MB+ 每平台**。建议:
   - 主发行包默认只带 **native**(0 额外体积)+ **一个** sidecar;
   - 或者做**按需下载**:首次切到 pi 时后台下载解包到用户目录(OneCreat 已有 arduino-cli 一键安装的先例);
   - `SKIP_DSH=1` 泛化成 `SKIP_SIDECARS=dsh,pi`。
4. **升级门禁**照抄 dsh README 的"升级必过项"表格,写进 `pi/README.md`,并在 CI 上跑。
5. **不要用 `npx`**(dsh README 已经列了三条理由:首启下载以分钟计、并发撞 npm cache 权限、preview 会破坏兼容)。pi 同理。

### 6.6 其它必须一起改的地方(清单)

| 位置 | 改什么 | 为什么 |
|---|---|---|
| `internal/engine/boundary_test.go` | 加 `TestPiAdapterIsNotASecondCore` + `TestPiDoesNotClaimWhatItCannotDo` | 现在的 `assertNoImports(t, "dsh", …)` 写死目录名 |
| `internal/boot/engine.go` `selectEngine` | 加 `case "pi"` + `piProbe`;错误文案 `(可选:"native"、"dsh"、"pi")` | 未知名字必须报错 |
| `internal/config/config.go` | `Engine` 注释加 pi;新增 `PiConfig`(runtime_dir / bin_path / model_placeholder / direct_model / startup_timeout_sec) | — |
| `internal/config/render.go` + `TestRenderTOMLRoundTrips` | 渲染新字段并扩测试 | CLAUDE.md 硬规则 |
| `internal/cli/cli.go:150` | `--engine` 的帮助文案加 pi | — |
| `internal/session/session.go` | 补 `EngineDSH` / `EnginePi` 常量;`Record.Engine` 加 write-once 校验 | §6.2 |
| `desktop/` | tab 状态加 engine;5 个 `boot.Build` 传 `Engine`;新增 `EngineCapabilities()` 并进 `rpc_surface.go` + `go generate` | §6.3 |
| `scripts/web-build.sh` / 新增 `scripts/pi-bundle.sh` | 打包 | §6.5 |
| `docs/` | `docs/pi调研/` 或直接扩 `docs/开发工作流.md` | — |

---

## 7. 工作量与风险估计

### 7.1 分期(粗粒度,人日)

| 阶段 | 内容 | 人日 | 验收 |
|---|---|---:|---|
| **P0 · Spike** | `internal/engine/pi/` 骨架:进程拉起(复用 `runtime.go` 的查找逻辑)、JSONL 行传输、`prompt` → `agent_settled` 一轮收敛、`mapper.go` 把 8 类事件映射成 `event.Event`、`TurnEngine`/`TurnHandle` | **2–3** | `reasonix run --engine pi "列出当前目录"` 出流式文本(此时门禁未做,**用临时分支绕过 requireToolGating,绝不合主线**) |
| **P1 · 门禁(最关键)** | `pi/extensions/onecreat-gate.ts`:`tool_call` 钩子 + `ctx.ui.input` 桥;Go 侧 `extension_ui_request` 分派 + `piDecider` 接 `toolpolicy.Pipeline.Before`;声明 `CapGatedTools`;`e2e_gate_test.go` 五条对称用例 | **3–4** | `ONECREAT_PI_E2E=1 go test -run Gate` 全绿,**断言被拒的写操作文件真的不落地** |
| **P2 · 网关红线** | `models.json` 模板 + upsert(照 `ensurePiModelsJson`);provider 名 `onecreat-gateway`、model 名 `tier-N`;`--no-extensions --no-skills --no-context-files -na`;`PI_OFFLINE/PI_TELEMETRY`;env 白名单;`Scrubber` 接上;**token 轮换方案**(见 §7.2 风险 R3) | **2–3** | `e2e_gateway_test` 对称版:上游收到的是档位名、事件流里 grep 不到任何真实品牌/模型串 |
| **P3 · 证据与工具桥** | `complete_step` 注册成 pi 自定义工具 + 回桥到 Go;`tool_execution_end`/`todo_write` 喂 `evidence.Ledger`(照 `dshRecorder`) | **1.5–2** | 证据链在 pi 下与 native 行为一致 |
| **P4 · 能力与切换** | `CapResume` 接线(`--session <path>`,顺手**把 dsh 漏掉的 BindSession 也接上**);`session.Record.Engine` write-once;桌面 tab 级引擎选择 + 能力表下发 + 前端禁用入口;`doctor` 加 pi 检查 | **2–3** | 新建标签能选引擎;compact/rewind 在 pi 下按能力禁用并给出人话原因 |
| **P5 · 分发与 CI** | `pi/` 目录 + 精确锁版本 + `pi-bundle.sh` + `web-build.sh` 集成 + `SKIP_SIDECARS`;升级必过项文档;Windows bash 前置检查 | **1.5–2.5** | 三平台发行包能起 pi;Windows 上缺 bash 时给出可读错误而不是崩 |
| **P6 · (可选)抽 sidecar 基类** | §2.2 的五块上提到 `internal/engine/sidecar/`,dsh 与 pi 各自瘦身 | **2–3** | dsh 全套测试仍绿(**不许回归**) |
| **合计** | | **12–17.5**(不含 P6)/ **14–20.5**(含 P6) | |

### 7.2 风险清单

| # | 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|---|
| **R1** | pi 上游改 `tool_call` 返回契约或 extension UI 子协议 → 门禁静默失效 | **中高**(9 周 20 版) | **致命**(安全门失效) | ① 精确锁版本;② 升级必过 e2e 门禁五条;③ 扩展里**双保险**:`try/catch` 显式 return block + 依赖官方 fail-safe;④ 启动时用 `get_state` 校验协议版本 |
| **R2** | 一进程一会话 → 桌面多标签 = N 个 139MB node 进程 | **高**(已确定的事实) | 中(内存/启动时间) | 懒启动(dsh 已经是"第一轮才拉起");闲置回收(NASApp 用 `PI_DAEMON_IDLE_MIN=180`);限制并发 pi 标签数并给出提示 |
| **R3** | **token 轮换**:平台 token 约 50 分钟过期,pi 子进程 env 是 spawn 快照,而 pi **没有 dsh 的 `credentials.set` 等价物** | **高** | 高(一小时后必然 401) | 三选一:① `models.json` 的 `apiKey: "!cat <tokenfile>"`("shell commands are **resolved at request time**"),Go 定期重写该文件(注意文件权限 0600,且**不能**是明文长期落盘 → 用 `TMPDIR` 下的临时文件 + 进程退出即删);② 扩展里 `pi.registerProvider(createProvider({auth:{apiKey:{resolve}}}))`,`resolve` 是 async,每次请求调,再由扩展经 extension UI 子协议向 Go 要当前 token(**最干净,推荐**);③ token 快过期时重启 pi 进程(粗暴,丢上下文) |
| **R4** | 忙/闲状态机写错:忙时直接 `prompt` 会被 pi 判错并**丢消息**;闲时 `steer` 会被 pi 拒 | 中 | 中(消息丢失) | 照抄 `frame.ts:planPrompt/planSteer` 的四分支纯函数,并写 Go 单测 |
| **R5** | 模型/厂商名泄漏(比 dsh 多三处) | 中 | **高**(SaaS 红线) | §3.6 逐条;`gateway_test` 对称版做全事件流 grep;考虑扩展里 `createBashTool({exposeSessionEnvironment:false})` 覆盖内建 bash |
| **R6** | Windows 缺 bash | 中 | 中 | 装配期检测 + 可读错误 + 文档;或 Windows 上不提供 pi 引擎 |
| **R7** | 发行包膨胀到 250MB+ | 高 | 中 | 按需下载 / `SKIP_SIDECARS` / 只带一个 sidecar |
| **R8** | 抽"sidecar 基类"时把 dsh 改坏 | 中 | 高 | **P6 放最后**;dsh 全套(含三组 e2e)必须绿 |
| **R9** | 项目本地 `.pi/` 被工作区里的文件污染(用户/学生的项目里放了 `.pi/extensions/`) | 低 | **高**(任意代码执行) | `--no-extensions --no-skills --no-prompt-templates --no-context-files -na`(明确**不信任**项目本地资源);隔离 `PI_CODING_AGENT_DIR` 到 OneCreat 自己的目录 |
| **R10** | 两套 sidecar 同时维护的长期成本 | **确定** | 中 | 这是战略选择题,不是技术问题 —— 见下 |

### 7.3 一句战略提醒

`docs/dsh调研/00_G0结论.md` §5 的原话是:pi 有 RPC 模式与多 provider,但"**产品 DeepSeek-first + dsh 是模型厂自家 harness**"这两点让 dsh 优先级高于 pi;dsh 唯一的战略劣势是 developer preview 的不稳定性。当时**没有做 pi 的对照验证**(未触发 B 计划,`05_pi对照验证.md` 从未产出)。

本次调研的新信息是:**pi 的协议完备度、文档质量与嵌入友好度明显优于 dsh**(1589 行 RPC 文档 vs 我们自补的 422 行 JS 插件;fork/compact/tree/cursor 全原生),而 dsh 的优势变成了"已经跑通 + 一进程多会话 + 体积小 + 模型厂血统"。

如果目标是"**双引擎并存供用户切换**",成本是 12–17 人日 + 长期双份升级维护;如果目标其实是"**验证 pi 是不是更好的那个 sidecar**",那更划算的做法是先花 P0+P1(5–7 人日)做一个**能过门禁 e2e 的 pi spike**,拿它和 dsh 做一次真实对照(生成质量、稳定性、CoT、工具正确率),**再决定要不要长期养两套**。建议按后者提给老板拍板。

---

## 8. 附录

### 8.1 读过的文件

**OneCreat(`/Users/localwork/06_System/onecreat`,只读)**

- `CLAUDE.md`
- `internal/engine/engine.go`(176)
- `internal/engine/boundary_test.go`(222)
- `internal/engine/native/native.go`(90)
- `internal/engine/dsh/turn_engine.go`(123)、`sidecar.go`(896)、`protocol.go`(204)、`linerpc.go`(211)、`mapper.go`(284)、`runtime.go`(142)、`scrub.go`(62)、`tailbuffer.go`(34)
- `internal/engine/dsh/e2e_gate_test.go`(前 60 行)
- `internal/boot/engine.go`(142)、`internal/boot/dshengine.go`(276)、`internal/boot/boot.go`(629-720 段)
- `internal/control/controller.go`(100-220、340-420 段)、`internal/control/capability.go`
- `internal/config/config.go`(Engine / DSHConfig 段)
- `internal/session/session.go`(40-60、240-260 段)
- `internal/toolpolicy/policy.go`(`Before` 段)
- `internal/serve/serve.go:398`、`desktop/app.go:602/625`、`desktop/tab_runtime.go`(boot.Build 调用点)、`desktop/settings_app.go:217`、`internal/cli/cli.go:150/187`
- `dsh/README.md`、`dsh/package.json`、`dsh/profiles/onecreat.cordis.yml`、`dsh/plugins/control/index.js`(422)、`dsh/plugins/gateway/index.js`(77)
- `scripts/web-build.sh`(dsh 段)、`scripts/` 目录清单
- `docs/dsh调研/00_G0结论.md`(pi 相关段落)、`docs/dsh调研/` 目录清单(01–08 未逐篇精读,见 §8.2)

**NASApp(`/Users/localwork/04_project/NASApp/brain`,只读)**

- `package.json`、`package-lock.json`(pi 依赖段)
- `pi-daemon/README.md`(71)、`daemon.ts`(880,重点读 1-60 / 200-340)、`pi-env.ts`(148)、`frame.ts`(127)、`client.ts`(255)、`models.deepseek.json`
- `ops-template/guard.ts`(前 80 行)、`ops-template/oneup-tools.ts`(前 90 行)
- `src/subagent.ts` / `src/full-mode.ts`(spawn 相关行)
- `runtime/pi-agent/`(隔离配置目录,内容为空的 `auth.json`/`models-store.json`)

**pi 官方文档(`node_modules/@earendil-works/pi-coding-agent@0.84.2/docs/`,只读)**

- `rpc.md`(1589,精读 1-1400)
- `extensions.md`(2992,精读 40-120 / 280-330 / 520-560 / 740-870 / 2880-2900)
- `custom-provider.md`(774,精读 1-120)
- `models.md`(565,grep + 关键段)
- `session-format.md`(438,精读 1-120)
- `sessions.md`(145)、`usage.md`(306,选项表)、`environment-variables.md`(94,全文)、`windows.md`(17,全文)
- `package.json`、`CHANGELOG.md`(版本节奏统计)

### 8.2 不确定 / 未验证事项

1. **dsh 的 `BindSession` 未被装配根调用**(§1.9)—— 基于全树 grep 得出,未跑实机验证。可能存在我没找到的间接路径。**建议实测**:重开应用打开老会话,看 dsh 是否记得上文。
2. **dsh 的 Windows 支持**:`dsh-bundle.sh` 有 `node.exe` 分支,但我没读该脚本全文、也没有实测记录。
3. **pi 的 `ctx.ui.input` 在 `tool_call` handler 里是否真的能 await 到 RPC 客户端的响应** —— 文档明确写 dialog 方法在 RPC 模式下"block until the client sends back an `extension_ui_response`",且 `ctx.hasUI===true`,但我**没有实跑验证**(遵守"不装 npm 包"的硬规则,也没启动 NASApp 的 pi 二进制)。这是 P1 阶段第一件要证的事,建议用 `NASApp/brain/node_modules/.bin/pi` 现成的二进制做一次 15 分钟的手工验证(起 `pi --mode rpc -e /tmp/gate.ts`,手喂 stdin),再决定要不要投 P1 的 3–4 人日。
4. **pi 的 `tool_call` 钩子对"内建 bash 工具"是否同样触发** —— 文档示例就是拦 bash(`isToolCallEventType("bash", event)`),应该成立;但"是否有任何工具路径绕过该钩子"(例如客户端直发的 `bash` 命令、`user_bash`)需要实测。**注意:RPC 的 `bash` 命令是客户端直接执行,不走 agent loop,因此不经 `tool_call`** —— OneCreat 不发这条命令即可,但要写进纪律。
5. **pi 的 `models.json` 里 `$ENV` 插值的解析时机** —— 文档只明确说 `!command` 形式是 "resolved at request time",`$VAR` 形式未明说。R3 的方案①依赖这一点,方案②不依赖,故推荐方案②。
6. `docs/dsh调研/01–08` 我只精读了 00 与各文档标题;07(门禁缺口解决报告)与 08(路线 A 执行方案)的细节可能包含与本报告冲突或补充的判断,建议交叉复核 07 §3。
7. pi 与 dsh **并发跑在同一台机器上**时的资源/端口/临时目录冲突未评估。
