# 05 · Phase 1 收尾 + Phase 2 护城河搬迁 实施报告

> 日期 2026-08-19。无人值守执行(Claude Opus 5)。分支 `feat/dsh-engine`(从 `main-v2` 切,**未 push**)。
> dsh 锁定 **`0.1.0-rc.7`**,全程未升级。`engine` 默认仍是 `native`,native 路径行为未变。
> 本报告只写**实际做到并验证过的**;没做/没验的在 §6、§7 明确列出。

---

## 0. 一句话结论

**`engine = "dsh"` 已经是一条可用的引擎路径**:CLI 与 Web 模式下能流式对话、走硬件 MCP 完成真实
ESP32 编译、审批/取消/计划模式/新建恢复会话全通、证据引擎照常判假、网关红线在落盘会话里扫不到
泄漏、多标签各自 sidecar 且退出无残留、darwin-arm64 发行包内置 sidecar 并在解压目录跑通。
**前端(React)一行未改。**

---

## 1. 架构:接缝选在哪

规划文档设想的是"在 `internal/control` 抽一个 `engineBackend` 接口、native 实现一份、dsh 实现一份"。
实际做下来**接缝更靠下、改动更小**:

```
control.Controller            ← 一行未动的编排逻辑:Compose / 计划门 / checkpoint /
      │                          审批 / 证据 / slash / 记忆 / @引用 / 会话文件
      ├── runner  agent.Runner ← ★ 真正的接缝就是这个既有接口
      │      ├── native: *agent.Agent(现内核)
      │      └── dsh:    *dsh.Engine(sidecar 驱动)
      └── engine  EngineBackend(可选)← 只给"引擎自己有状态"的命令用:
                                        取消 / 计划模式 / 会话绑定 / 审批回调 / 关闭
```

`agent.Runner` 本来就是 `interface{ Run(ctx, input) error }` —— 一轮对话的完整语义。
`dsh.Engine` 实现它(排 prompt → 等这一轮 running→idle 收敛),`boot.Build` 在 `engine="dsh"` 时
**只把 `runner` 换掉**,其余装配(工具注册表、技能、记忆、hooks、权限策略、证据账本、会话文件)
一律照旧。于是:

- `Compose` 仍在每轮拼板卡事实/教练人设/记忆 → 进用户消息、不进系统提示(前缀缓存纪律不破);
- 计划门(`exit_plan_mode` 审批)仍在 Controller 里;
- checkpoint、审批、证据仍是 Go 侧那一套。

**native 路径的判据**:`c.engine == nil`,所有 `if c.engine != nil` 分支都不进。

新增的 `control.EngineBackend` 接口只有 6 个方法:`BindSession / SetApprover / SetPreEdit /
Cancel / Running / SetPlanMode / Inject / Close`。

---

## 2. 补在 wire 上的东西(Phase 1 最大的一块)

`01_架构与扩展点.md` §6 点明的四个 wire 缺口(取消 / 审批 / 计划模式 / resume),这次全部补上了,
做法是**我们自己拥有 stdio 传输**:`dsh/plugins/control/index.js` 内部 `new HarnessSdkJsonRpcServer(...)`
复用官方实现的 initialize/prompt/shutdown 与四种通知,再在同一条 wire 上加自己的方法与通知。
(官方 `JsonRpcLineTransport.onRequest` 是"后注册覆盖前一个",所以没法在官方 server 上追加方法;
profile 里因此**不加载** `dsh-sdk-jsonrpc-server` 那一行,否则两个 transport 抢 stdin。)

| 缺口 | 补法 | 验证 |
|---|---|---|
| 取消 | `onecreat/session.cancel` → `agent.cancel({kind:'user'})` | Web:running=true → Cancel → 5s 内 idle,`sleep 45` 被打断 |
| 审批 | `tools/pre-execute` 成为**单一决策点**:Go 侧跑 `permission.Policy`,ask 时经 `Controller.RequestApproval` 弹审批 | Web:`approval_request{tool:bash,subject:…}` 弹出并阻塞,Approve 后文件才落地 |
| 计划模式 | `onecreat/planMode.set` 驱动 `dsh-plan-mode` **+ Go 侧硬门控** | Web:写操作被拒、文件未创建 → 出方案 → `exit_plan_mode` 审批 → 执行 |
| resume | `onecreat/session.load` 走 `ctx.agents.resume`;dsh 会话 id 由 Go 会话文件路径确定性派生 | Web:ResumeSession 后模型正确答出上一会话创建的文件名 |

另外补的两条桥:

- **工具桥** `onecreat/tool.invoke` ↔ `onecreat/tool.result`:`complete_step` 在 Go 侧执行(证据账本是它的裁判);
- **预执行桥** `onecreat/tool.preExecute` ↔ `…done`:权限判定 + 审批 + checkpoint 文件快照三合一。

> ⚠️ **为什么权限判定必须过 Go**:dsh 自己的 `bash`/`write`/`edit` 不经过 Go 的 tool registry。
> 不接这条桥,OneCreat 的 deny 名单 / ask 规则 / 计划模式在 dsh 引擎下**全部失效**,而 UI 还
> 显示着"计划模式"——安全错觉。现在 deny/ask/allow 由同一份 `permission.Policy` 判。

---

## 3. 验收结果(每条都贴了实际输出)

### 1A · `reasonix run --engine dsh` 流式对话 ✅

```
$ ./bin/reasonix run --engine dsh "用一句话介绍你自己"
  ▎ thinking
我是 OneCreat——一个以任务规划和证据验证驱动的 AI 编码助手,…
  · 10926 tok · in 10579 (10496 cached / 83 new) · out 347 (310 reasoning)
```
Usage 有数,且**前缀缓存命中 10496**(说明系统提示进的是 dsh 缓存前缀、每轮状态走用户消息)。

### 1B · 硬件 MCP 走通一道 ESP32 编译题 ✅

工作目录 `/tmp/dsh-esp32`,`engine=dsh`:检测工具链(`arduino-cli 1.2.2` / PlatformIO / esp32 core 3.2.0)
→ `hardware_project_scaffold` 建 sketch → 编译:

```
Sketch uses 296114 bytes (22%) of program storage space. Maximum is 1310720 bytes.
Global variables use 20492 bytes (6%) of dynamic memory…
```

**顺带验到证据引擎在真实回路里生效**:模型第一次 `complete_step` 引用了本轮没读过的证据文件,
被判失败(`⊘ tool failed`),补读之后才签收成功。

### 2 · Web 模式(`make build-web`,前端零改动)✅

全部经 `POST /rpc/<Method>` + 一条 SSE 驱动,即前端真实走的通道:

| 项 | 结果 |
|---|---|
| 流式对话 | `text`/`reasoning`/`message`/`usage`/`turn_started`/`turn_done` 齐全 |
| 工具审批 | `approval_request{"tool":"bash","subject":"echo hi7 > …"}` 弹出并阻塞;`Approve("main","1",true,false)` 后文件才出现 |
| 取消 | `Meta.running` true → `Cancel` → 5s 内 false + `turn_done`;`sleep 45` 被打断 |
| 计划模式 | 写操作被拒(`计划模式:现在只调研与设计…`,文件未创建)→ 模型给方案 → `approval_request{"tool":"exit_plan_mode"}` → Approve → 退出计划模式并真正执行 → 文件创建 |
| 新建会话 | `NewSession` 后 History 只剩系统提示 |
| 恢复会话 | `ResumeSession(<path>)` 后问"刚才你给我创建了哪个文件",答 `/tmp/dsh-web/plan_probe.txt` |

### 3b · 证据链真实 ✅

单测 `internal/engine/dsh/bridge_test.go`(不需要模型,确定性):

```
证据引擎拒绝:evidence 1:verification command "arduino-cli upload -b esp32:esp32:esp32"
在本轮没有匹配到成功的执行记录——command 只能填本轮真实运行成功的 bash 命令或调用过的
工具名,不能凭空声称。
```
往账本里补一条真实的 `mcp__hardware__arduino_upload` 成功收据后,同一条签收即通过。
另有 `TestConsumeFeedsLedger` 证明 dsh 的 `tool/call`+`tool/result`+`todo/write` 确实喂进了账本。

### 3d · 网关红线 ✅(用本地假 OpenAI 兼容网关实测)

网关收到的请求:

```
path: /v1/chat/completions
authorization = Bearer <登录 token>
model = tier-1                      ← 档位占位符,不是真实模型名
user-agent = deepseek-harness/0.1.0-rc.7 …    ← 见 §5 待确认项
x-deepseek-harness-user-id / -session-id      ← 归因头
```

落盘会话(dsh JSONL)扫描:

```
deepseek-official: 0   llm-deepseek: 0   deepseek-v4: 0   api.deepseek.com: 0
DEEPSEEK_API_KEY: 0    <网关 URL>: 0     <token>: 0
request/header  → {"provider":"onecreat-gateway","model":"tier-1", …}
request/context → {"provider":"onecreat-gateway","model":"tier-1","contextWindow":1000000}
```

**发现并修掉一处真实泄漏**:dsh 默认在系统提示最前面插
`You are an AI agent powered by DeepSeek Harness.`,模型会照着自报家门 —— 第一版冒烟时它就回答
"我是 DeepSeek 驱动的 AI 编程助手"。已在 profile 里 `includeHarnessIdentity: false` 关掉。

**残留一处(已评估,判为可接受)**:落盘会话里有一处 `"plugin":"@deepseek-ai/dsh-system-prompt"`
—— 那是消息来源(source)的插件包名,只在本机会话文件里,**不进任何 UI/事件/错误体**
(Go 的事件映射根本不带 `source` 字段),且它暴露的是 harness 供应商、不是模型/档位。

顺带修掉一个**严重可用性缺陷**:dsh 的 `agent/error` 是 agent 事件、不上 SDK wire,所以一次 401
在客户端表现为"什么都没发生"(rc=0、零输出)。现在映射 `turn/end` 的失败 reason:

```
401 →  ! AUTH: invalid token
断连 → ! TRANSPORT: OneCreat API request to OneCreat failed     ← 品牌与网关 URL 已被脱敏
```

### 4 · 多标签 + 退出无残留 ✅

```
ListTabs → [{"id":"main",…},{"id":"tab1",…}]      两个标签各跑一道题
pgrep -f dsh-sdk-jsonrpc-demo → 2                 每标签一个独立 sidecar
main 末条 = "用 bash 执行 echo tab1-quick"          历史互不串
tab1 末条 = "用 bash 执行 sleep 20 && echo tab2-done"
Quit → sidecar 0 个,onecreat-web 0 个
```

### 5 · 打包 ✅(darwin-arm64)

```
$ SKIP_FRONTEND=1 scripts/web-build.sh darwin/arm64 <ver>
==> 下载 Node v22.23.2 (darwin/arm64)
==> 装配 dsh 生产依赖闭包(hoisted)
==> dsh sidecar 就绪:172M
dist/onecreat-web-darwin-arm64.tar.gz  59,273,550 字节
```
解压后直接跑(`ONECREAT_ACCOUNT_MODE=local ONECREAT_ENGINE=dsh ./onecreat-web`),
用**包内** `runtime/node/bin/node` 拉起 sidecar,对话正常返回。

**体积**:8.6 MB → 59.3 MB(压缩后),解压后 +173 MB。其中 Node 二进制本身 108 MB、
依赖闭包 65 MB。见 §7 待拍板第 1 条。

### 其它前端 · `reasonix serve` ✅

```
$ ONECREAT_ENGINE=dsh reasonix serve --addr 127.0.0.1:8792
$ curl -X POST …/submit -d '{"input":"用一句话介绍你自己"}'
SSE: text 47 / reasoning 42 / message 1 / usage 1 / turn_started 1 / turn_done 1
```

### native 路径回归 ✅

```
$ ./bin/reasonix run "用一句话回答:1+1=?"     # 不带 --engine,走 native
1+1=2。
  · 18399 tok · in 18370 (256 cached / 18114 new) · out 29 · ¥0.0182
```

### 三套 + typecheck 全绿 ✅

根 `go build/vet/test`、`desktop` 的 `!web` 与 `-tags web` 三件套、`desktop/frontend` 的
`pnpm tsc --noEmit`、`dsh/` 的 `pnpm typecheck`、`gofmt -l` 干净。

---

## 4. 功能对照表(Controller 每个导出方法在 `engine="dsh"` 下的状态)

> "复用" = 代码完全没动、在 dsh 下天然照旧(它们在 runner 之上或根本不碰 runner)。

| 方法 | dsh 下状态 | 说明 / 理由 |
|---|---|---|
| `Run` `Send` `SendWithRaw` `Submit` | **已迁** | `runner.Run` 换成 sidecar;Compose/hooks/计划门全在外层照旧 |
| `Cancel` | **已迁** | `onecreat/session.cancel`(wire 级)+ Go ctx 取消,双管齐下 |
| `Running` | **已迁** | 由 `session.status` 维护 |
| `Approve` `PendingApprovals` `PendingAsks` `EnableInteractiveApproval` | **复用** | 预执行桥把 dsh 的工具调用送回 `Controller.RequestApproval`,审批 UI/会话内授权/YOLO 全同 native |
| `SetPlanMode` `PlanMode` | **已迁** | wire 下发 + Go 侧硬门控(拒非只读工具) |
| `History` `SessionPath` `SetSessionPath` `Snapshot` `SnapshotActivity` `SessionDir` | **复用** | 引擎把每轮 user/assistant 文本镜像进 Go `agent.Session`,落盘/标题/前端恢复照旧 |
| `NewSession` | **已迁** | 换会话文件 → `BindSession` 派生新的 dsh 会话 id |
| `Resume` | **已迁** | `BindSession(path)` + `onecreat/session.load` 从 dsh 持久化恢复 |
| `Checkpoints` `Rewind(code)` `InvalidateCheckpoints` | **已迁** | 预执行桥在写操作前于 Go 侧快照文件;实测 rewind 能还原 |
| `Rewind(conversation/both)` | **暂不支持** | 对话真源在 dsh 侧;截断 Go 镜像不会改变模型上下文,只会制造假象。明确报错 |
| `Fork` `ForkNamed` `Branch` `Branches` `SwitchBranch` `BranchTreeText` | **暂不支持**(Branches/BranchTreeText 只读,照常返回) | 同上:需要重写消息日志。dsh 有 `ctx.sessions.fork()`,是**下一步**可接的一等能力 |
| `Compact` `SummarizeFrom` `SummarizeUpTo` | **暂不支持** | dsh 自己有 `compaction-basic`(profile 已挂,自动压缩照常工作),但**手动**压缩/摘要要重写日志 → 明确报错 |
| `CompactRatio` `ContextSnapshot` `LastUsage` `SessionCache` | **降级**(返回 native 侧的值/零值) | 这些读的是 Go executor 的计数器,dsh 下不更新;`Usage` 事件本身是准的(前端顶栏用它) |
| `Compose` `SystemPrompt` `SetCoachMode` `Memory` `QueueMemory` `ForgetMemory` `QuickAdd` `SaveDoc` | **复用** | 每轮注入随用户消息进 dsh;实测落盘会话里能 grep 到 `<coaching-style>` |
| `Skills` `RunSkill` `Commands` `CustomCommand` `MCPPrompt` | **复用** | 在 Go 侧展开成文本再进 runner;实测 `/review` 正常 |
| `HasRefs` `ResolveRefs` | **复用** | @引用在 Go 侧解析 |
| `Ask` `AnswerQuestion` | **暂不支持**(dsh 侧不会发起) | dsh 的 `user-questions` 包没挂;`exit_plan_mode` 的审阅走的是我们自己的计划门 |
| `AddMCPServer` `ConnectMCPServer` `RemoveMCPServer` `DisconnectMCPServer` `ConfiguredMCPNames` `DisconnectedMCPNames` `ConnectConfiguredMCPServer` `Host` | **部分**:调用不报错,但只影响 Go 侧注册表 | dsh 侧的 MCP 在组合层挂(目前只挂硬件 MCP)。**热加 MCP 对 dsh 无效**,见 §7 |
| `Jobs` | **降级** | 后台任务是 Go 侧的 jobs.Manager;dsh 的 `job_*` 工具没挂 |
| `Balance` `Label` `HookRunner` `SetBypass` `Bypass` `Close` | **复用** | 与引擎无关 |

---

## 5. 会话的单一真源(规划 1.6 的决策)

- **模型可见历史的真源 = dsh 自己的 store**(JSONL,默认 zstd;位置 `<会话目录>/dsh/<工作区>/<会话id>/`)。
- **Go 侧的 `agent.Session` 是只读投影**:引擎在每轮收敛后把 user/assistant 文本追加进去,
  供 History / 会话文件 / 会话标题 / 前端恢复用。**Go 侧从不把它写回 dsh**,不存在双写。
- **两边靠会话 id 对上**:dsh 会话 id = `oc-` + Go 会话文件路径的 sha1 前 24 位;
  没有会话文件时(headless `reasonix run`)用一次性随机 id,避免所有 headless 运行共用一条会话。
- 因此:`Resume` 能对上;`Rewind(conversation)`/`Fork` 这类**重写日志**的操作在 dsh 下不支持
  (改投影不改真源 = 制造假象),这是**有意的**,不是没做完。

---

## 6. 坑(踩过的,写下来省下次的时间)

1. **resume 出来的 agent 必须带 provider/model**。官方 `HarnessSdkJsonRpcServer.createSession` 给
   `agentOptions:{provider,model}`;我们自己 `ctx.agents.resume()` 时漏了它,恢复出的 agent 没有
   模型路由,收到 followup **直接回 idle** —— 表现为"发消息什么都没发生、退出码 0、零输出",
   非常难查。修法:在 `initialize` 时记下路由事实,resume 时带上。
2. **dsh 的失败不上 wire**。`agent/error` 是 agent 事件,SDK server 只转 `session/event` 与
   `session.status`。唯一能看见失败的是 `turn/end` 的 `reason`。不映射它,401/断网就是静默失败。
3. **`TokenUsage` 的字段语义与我们不同**:`inputTokens` 是**未命中缓存**的输入,命中的单列
   `cacheReadTokens`(计费输入 = 三者之和)。直接当 `PromptTokens` 用会少算。
4. **`tool/result` 的 callId 在 `message.content[0].toolCallId` 里**,不在事件顶层;不解开就没法
   和 `tool/call` 配对(证据账本要的就是这个配对)。
5. **profile 里的相对插件路径是相对 cordis.yml 所在目录**,不是相对项目根 —— `./plugins/…` 会去
   `profiles/plugins/…` 找。
6. **`dsh-session-checkpoint-policy` 不挂就只在优雅退出时落盘**,被杀就整段丢。
7. **pnpm 默认的符号链接 node_modules 一打包解压就断链**,发行包必须 `--node-linker=hoisted`。
8. **macOS 的 `/tmp` 是 `/private/tmp` 的符号链接**:checkpoint 记的路径不解符号链接,rewind 会
   报 "escapes workspace"。
9. **`dsh-agent-spine-demo` 默认往系统提示塞 harness 身份句**(见 §3 的 3d)。

---

## 7. 没做到 / 替你决定 / 待你拍板

> **2026-08-19 补记(本报告之后的修复,别再照下表当现状)**:
> 1. 「Windows / Linux 发行包里的 dsh sidecar」**已解决** —— `release-web.yml` 拆成
>    `sidecar` 矩阵(各平台自装 `runtime/` 传 artifact)+ `release` 两阶段,`web-build.sh` 新增
>    `DSH_RUNTIME_DIR` 直接套用预装 runtime。仅 linux/arm64 仍无(矩阵里没有 arm64 Linux runner)。
>    即下面「待你拍板」的第 4 条已经做掉。
> 2. 修掉两个网关路径的真 bug:wire model 之前恒为占位符、**没把用户选的档位传给网关**
>    (现改读 `ONECREAT_TIER`);登录 token 刷新**传不进子进程**(现补 `onecreat/credentials.set`,
>    每轮 prompt 前按需补发),否则约 50 分钟后 dsh 模式必然 `! AUTH: invalid token`。
> 3. **门禁收口见 [07](07_门禁缺口解决报告与收口执行方案.md)**(2026-08-21)。本报告里"门禁靠 Go 半边单测 +
>    一次手工 Web 验证"的状态**已过时**:`internal/engine/dsh/e2e_gate_test.go` 用真 sidecar + 假网关
>    钉死了 deny / ask 批 / ask 拒 / 计划模式 / 取消 fail-closed 五条,`internal/boot/dshengine_test.go`
>    表驱动钉住 `dshDecider`,并已写进 `dsh/README.md` 的升级必过项。

### 没做到(诚实清单)

| 项 | 原因 |
|---|---|
| Windows / Linux 发行包里的 dsh sidecar | 依赖闭包含原生模块(node-pty / koffi / spawn helper),只能在目标平台上装。脚本对非本机平台**跳过并告警**,那些包里 `engine=dsh` 不可用(`engine=native` 照常)。需各自平台的 CI 装配 |
| `Fork` / `Branch` / 手动 `Compact` / `Summarize` / 对话 `Rewind` | 见 §4:都要重写消息日志,而真源在 dsh 侧。dsh 有 `ctx.sessions.fork()` 与 compaction 接缝,是明确的下一步 |
| 热加 MCP(`AddMCPServer`)对 dsh 生效 | dsh 的 MCP 走组合层;要热加得让控制面插件支持运行时 `ctx.plugin(McpClient, …)` 并从 Go 下发。目前只在启动时挂硬件 MCP |
| 真机烧录 + 串口的端到端复跑 | 本次没插板子;1B 只做到"编译成功"。烧录/串口路径的 Go 侧代码没变(硬件面板不经 Controller),但**未实测** |
| `reasonix chat`(TUI)与 `acp` 在 dsh 下的冒烟 | 与 Web 走同一个 Controller,理应可用,但**本次未实测**。已实测的前端:`reasonix run`、`reasonix serve`(SSE 流式正常:`text`/`reasoning`/`message`/`usage`/`turn_started`/`turn_done`)、Web 模式 |
| 网关的**真实** token 端到端 | 本机没有活的老师账号 token;用本地假 OpenAI 兼容服务器做的等价验证(请求头/占位符/错误脱敏/落盘扫描) |

### 替你决定的(按保守默认做了)

1. **接缝选在 `agent.Runner` 而不是新抽 `engineBackend` 全量接口** —— 改动面最小、native 路径可证不变。
2. **Go 侧 `agent.Session` 做只读镜像** —— 换来 History / 会话落盘 / 标题 / 前端恢复零改动;
   代价是"重写日志"类操作在 dsh 下不支持(已明确报错,不静默)。
3. **计划模式加 Go 侧硬门控** —— dsh 的 plan mode 只是软引导,不拦写操作。不加就会有安全错觉。
4. **权限策略走预执行桥** —— 否则 dsh 自带工具完全绕过 deny/ask 规则。
5. **每轮运行时状态仍随 `Compose` 进用户消息**(而不是改走 `onecreat/inject`)—— 与 native 行为
   逐字一致优先;`onecreat/inject` 已实现并留作 turn 中途注入的接缝。
6. **脱敏替换文案用产品名 "OneCreat"** 而不是档位占位符(否则错误体读成 "tier-1 API request to tier-1")。
7. **网关模式才开脱敏**(直连模式用户用的就是自己的 key,擦掉反而莫名其妙)。

### 待你拍板

1. **发行包体积**:带 sidecar 从 8.6 MB 涨到 59.3 MB(解压 +173 MB,Node 二进制占 108 MB)。
   选项:(a) 默认带(现状);(b) 出两个包,普通版 + "dsh 版";(c) 首次切到 dsh 引擎时按需下载运行时。
2. **归因头**:dsh 会往网关发 `User-Agent: deepseek-harness/…` 与 `x-deepseek-harness-*`。
   这是发给**我们自己的平台**、不是对用户泄漏,但要确认平台网关**容忍**这些头(不因未知头报错),
   并且**不要把它们回显进任何面向用户的错误信息**。
3. **默认引擎什么时候切**:现在 `engine` 默认仍是 `native`。建议按规划 Phase 3 先自己日常用一周。
4. **跨平台 sidecar 的 CI**:要不要给 `release-web.yml` 加 Windows/Linux runner 来装各自的
   `node_modules`(否则那两个平台的包里没有 dsh 引擎)。
5. **`Fork`/`Branch`/手动压缩**要不要现在就接 dsh 的 `ctx.sessions.fork()` 与 compaction 接缝
   (工作量中等),还是先留"暂不支持"。

---

## 8. 下一步(建议顺序)

1. 用真实老师账号 token 跑一遍网关端到端(补 §7 的最后一项验证)。
2. `reasonix chat` / `serve` / `acp` 各冒烟一遍。
3. 插真板子复跑 1B 的**烧录 + 串口验证**全流程(这是硬件护城河的最后一公里)。
4. 接 dsh 的 fork/compaction,把 §4 里三行"暂不支持"消掉。
5. 跨平台 sidecar 的 CI 装配。
6. 自己日常用一周(Phase 3 灰度),再谈默认切换。
