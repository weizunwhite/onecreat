# 任务:OneCreat 底层 agent 迁移到 DeepSeek Harness(dsh)—— Phase 1 收尾(接线)+ Phase 2 护城河搬迁

> 仓库 `/Users/localwork/06_System/onecreat`,从 `main-v2` 切新分支 **`feat/dsh-engine`**,所有工作在其上,只 commit 不 push。
> 先读:`CLAUDE.md`、`docs/dsh迁移执行规划.md`、`docs/dsh调研/00–04`(尤其 `01` §3/§6/§7 的 wire 缺口与 MCP 接法、`04` 的 spike 现状与坑)、`internal/engine/dsh/*`(已有驱动层骨架:JSON-RPC 客户端/事件映射/脱敏/进程守护,三套全绿,**但未接进 Controller**)。
> dsh 源码参考克隆在 `/Users/localwork/06_System/_ref/deepseek-harness`(tag `dsh-v0.1.0-rc.7`,与调研一致;**本任务锁定这个版本**,别升级)。

## 用户已拍板(不要再论证、不要做 A/B 对比)

- **直接迁移 dsh**。用户自己用过 dsh 觉得好,判断是:dsh 缺的正是我们的插件(证据引擎/硬件 MCP/板卡事实/诚实护栏)+ 知识库。你的任务是把这些搬上去,让 `engine="dsh"` 成为可用的引擎。
- 迁移终态架构(规划文档 §1):`control.Controller` 以下换成 dsh sidecar,以上(Wails/Web 前端、账号档位、串口/OTA、证据引擎)不动。**Web 模式是主分发形态**(`docs/Web模式.md`),验收以 `reasonix run` + Web 模式为准。
- 网关红线不变:真实 provider/model 名不得出现在 UI/日志/错误体/落盘会话;用户只见档位。

## 关键事实(省你踩坑)

- **凭证**:本机 `~/.env` 含 `DEEPSEEK_API_KEY`,内核 `internal/config` 的 `loadDotEnv()` 会把它装进进程环境。驱动层只要把 `os.Environ()`(配置加载后)透传给 dsh 子进程,dsh 的 `dsh-llm-deepseek` 就能直连 `api.deepseek.com`。**你不需要也不要 cat `~/.env`**(上次 spike 就是被"无凭证"卡死的——其实凭证一直在,只是要靠内核加载)。网关模式(`ONECREAT_GATEWAY_*`)本机没有活 token,网关适配器只做静态/假服务器验证。
- **MCP**:dsh 工具名 `mcp__<server>__<tool>` 与我们同形;`onecreat-hardware-mcp`(stdio)零改动可挂,但只能走组合层(cordis.yml/profile),不能经 ACP `session/new` 传(`01` §7)。
- **wire 缺口**(`01` §6):SDK JSON-RPC 路径无 cancel、审批 dead、无 resume/fork;ACP 有 cancel+审批但无流式/工具事件。**不要二选一,也不要等上游**——dsh "一切皆插件",我们自己写薄插件把缺的方法补到 wire 上(见 Step 2)。
- **provider 品牌**:`deepseek-official`/`llm-deepseek` 会出现在错误体和事件里,纯配置藏不住(`02` 漏点④),需要自命名适配器插件(Step 3d)。
- **npx 坑**:`npx @deepseek-ai/dsh` 并发撞 npm cache(用隔离 `npm_config_cache`);`dsh web` 分发不含 `dsh-sdk-jsonrpc-server`;首启构建以分钟计。**所以我们自带一个 dsh 组合包**(Step 0),不依赖 npx 全局安装。
- **证据引擎**(`internal/evidence`)是诚实性唯一来源,必须随迁,不是可选。

## 硬规则

- 不派子代理;全程无人值守不停下来问;需拍板的按保守默认做并写进报告"替用户决定"。
- 遵守 CLAUDE.md:Session 单写者/`Snapshot()`;`a.mu` 不跨 `boot.Build`;改 Go 签名同步 `bridge.ts`+mock;`desktop/wire.go` 与 `internal/serve/wire.go` 一致;DeepSeek 前缀缓存纪律——**每轮运行时状态(板卡事实/人设/记忆)走 pre-step 注入,不进系统提示**;新代码中文注释。
- `engine` 默认仍 `native`,native 路径行为不能变;每步结束三套 `build·vet·test`(根 / desktop !web 与 -tags web / frontend tsc)全绿,`dsh/` 目录自己的 `pnpm typecheck` 也要绿。
- 分步 commit(中文,末尾 `Co-Authored-By: Claude Opus <noreply@anthropic.com>`),不 push。
- 做到哪算哪,**按下面顺序**,诚实报告。别为了"全做完"把没验证的写成验证过。

## 步骤

### Step 0 — OneCreat 自带的 dsh 组合包(`dsh/` 目录,TS/ESM)
目的:一个可被 Go 拉起的、版本锁死的、含我们插件的 dsh 运行时,开发与打包都用它。
1. 新建仓库顶层 `dsh/`:`package.json`(pnpm,依赖 `@deepseek-ai/dsh*` 锁到 rc.7 对应包版本)、`profiles/onecreat.cordis.yml`(组合:dsh 核心 + `dsh-sdk-jsonrpc-server` + `dsh-mcp-client`(hardware) + 我们的插件)、`plugins/`(TS 源码)、`tsconfig`、`pnpm typecheck`/`pnpm build`/`pnpm start -- --profile onecreat` 脚本。参考 `_ref/deepseek-harness/examples/jsonrpc-agent` 与 `AGENTS.md` 的插件写法(`ctx.effect()/ctx.on()`,注册返回 disposer)。
2. 本地能用 `pnpm -C dsh start` 起一个 stdio JSON-RPC server,手动 initialize + prompt 一轮(直连 DeepSeek)。
3. 写 `dsh/README.md`:怎么装、怎么跑、插件清单、版本锁定策略。

### Step 1 — Phase 1 收尾:`dsh.Engine` 接进 `control.Controller`
按 `04` §3 设计:
1. `internal/control` 抽最小 `engineBackend` 接口(Submit/Send/Cancel/Running/Approve/PendingApprovals/SetPlanMode/History/NewSession/Resume/SessionPath + 事件 sink 注入),现有逻辑成 `nativeBackend`,`dsh.Engine` 适配成 `dshBackend`;其余 Controller 方法对 dsh 后端返回明确"dsh 引擎暂不支持"错误(每个都列进 Step 6 的对照表)。
2. `boot.Build` 按 `cfg.Engine` 分流;dsh 侧启动命令默认指向 `dsh/` 组合包(开发)或打包内置路径(Step 5),`[dsh]` 配置段已有,按需补字段并过 `TestRenderTOMLRoundTrips`。
3. **验收 1A**:在有 `~/.env` 的环境下 `reasonix run --engine dsh "用一句话介绍你自己"`(或配置文件 `engine="dsh"`)流式输出正常、`Usage` 事件有数。
4. **验收 1B**:`reasonix run` 一道 ESP32 编译题(硬件 MCP 已在 profile 里),看到 `mcp__hardware__*` 工具调用与真实编译产物。

### Step 2 — 补 wire 缺口(我们自己的 `onecreat-control` 插件)
优先在 `dsh-sdk-jsonrpc-server` 上**追加方法**(查它是否允许插件注册额外 method/notification;不允许就 fork 一份薄 server 插件进 `dsh/plugins/`),补:
- `session/cancel`(调 dsh 的 abort 接缝,不再靠杀进程);
- 审批桥:把 `ctx.approval` 请求转成 JSON-RPC 通知 `onecreat/approval.request` → Go 侧变成现有 `event.Approval` → `Controller.Approve(id, allow, session)` 回 `onecreat/approval.resolve`;语义(一次/本会话)与 native 一致;profile 里**关掉**自动批,所有写操作/命令执行都要过审批;
- `onecreat/planMode.set`(驱动 `dsh-plan-mode`);
- `onecreat/session.load`(用持久化 `prepare/load(id)` 实现 Resume)、`onecreat/session.fork`(有余力)。
Go 侧 `internal/engine/dsh` 对应接上;单测用 fake server 复刻协议。**验收 2**:Web 模式(`make build-web`,`engine="dsh"`)里聊天 + 工具审批弹窗 + 取消 + 计划模式开关 + 新建/恢复会话都能用,前端零改动。

### Step 3 — Phase 2 护城河搬迁
a. **硬件 MCP**:Step 0 已挂;确认 `HardwarePanel` 的检测/安装/烧录按钮(走 Go 侧直调 MCP)在 dsh 引擎下照常。
b. **证据引擎**:先看 `internal/evidence` 现在从哪里拿输入(`todo_write`/`complete_step` 工具调用 + 命令/文件/串口产物)。dsh 侧没有这两个工具 → 把我们需要的**专有内置工具**(todo_write/complete_step 及证据相关,清单你按 `internal/tool/builtin` 判断,**不要**把 fs/shell 也搬过去,dsh 自己有)以 MCP 形式暴露给 dsh(若 `reasonix mcp` 子命令或 `internal/plugin` 已能把内置工具当 MCP server 提供就复用;否则加一个最小 stdio MCP 入口),Go 驱动层消费 dsh 的 tool call/result 事件喂证据引擎。**验收 3b**:1B 那道题跑完 `HardwareEvidenceStatus`/证据导出有真实链条,谎报"烧录成功"会被判未完成。
c. **板卡事实注入 / 诚实收尾护栏 / 教练人设 / 记忆框架**:现在在 `Controller.Compose` 等处每轮拼进 prompt。dsh 侧做一个 `onecreat-inject` 插件,监听 `agent/pre-step`,把 Go 经 wire 下发的"本轮注入文本"塞进上下文(不进系统提示,保前缀缓存)。Go 侧 `dshBackend.Submit` 复用现有 Compose 逻辑生成注入文本。逐条对照 `internal/skill/builtins.go` 与板卡事实来源,能删的提示词工程先别删,保持行为一致优先。
d. **网关适配器 `onecreat-gateway`**(`dsh/plugins/gateway`):OpenAI 兼容 chat-completions provider,路由名 `onecreat-gateway`,从 env 读 `ONECREAT_GATEWAY_URL/TOKEN`,wire `model` 用档位占位符;错误体/日志不带品牌与 baseURL;Go 侧 `scrub.go` 仍兜底。用本地假 OpenAI 兼容服务器(httptest 或 python)验证请求头/占位符/错误脱敏;**验收 3d**:跑完一轮后 grep dsh 落盘会话与 stderr,不出现真实 model 名、`deepseek-official`、`llm-deepseek`、网关 URL。
e. **Skills / 自定义命令**:优先 Go 侧复用现有 `RunSkill/CustomCommand` 展开成文本再 Submit(零 TS);只在 dsh 侧确有必要时映射到它的 skills。
f. **文件级 checkpoint/rewind**:dsh 无等价物,保留 `internal/checkpoint`:监听 dsh `tools/pre-execute`(或我们插件转发的预执行通知,含文件路径参数)在 Go 侧先快照。做不完就把 `Rewind` 列为"暂不支持"并在 UI 报错清楚。
g. **Session 单一真源**:dsh store 是对话真源;我们 session 文件只存元数据(tab/workspace/引擎类型/dsh sessionId);`History` 从 dsh 事件回放或 load 得到。写进 `docs/Web模式.md` 或新文档。

### Step 4 — 多标签与 Web 模式打通
每个桌面 tab(`tabRuntime`)一个独立 sidecar 进程(隔离、并行,与现有"每 tab 一 Controller"对齐);`CreateTab/CloseTab/SwitchWorkspace` 生命周期正确,退出无残留(复用 Web 收口时的退出清理思路)。**验收 4**:Web 模式下两个 tab 同时跑两道题互不干扰;`Quit` 后 `pgrep` 无 dsh/node 残留。

### Step 5 — 打包(做到哪算哪)
`scripts/web-build.sh` 增加 dsh sidecar 装配:每平台内置 Node 运行时(锁版本,下载缓存到 `build-cache/`)+ 预构建的 `dsh/` 组合包(prune 后的 node_modules,含 jsonrpc-server 闭包)→ 发行目录 `runtime/dsh/`;Go 驱动层按"与主程序同目录的 `runtime/dsh`"解析。量一下包体积变化并写进报告。Windows 只保证交叉编译与目录布局,不实测。

### Step 6 — 文档与报告
- `docs/dsh迁移执行规划.md`:按实际更新 Phase 1/2 状态;新增**功能对照表**(Controller 每个方法:已迁/映射/暂不支持+理由)。
- `docs/dsh调研/05_Phase1-2_实施报告.md`:做到哪、怎么验的(贴原始输出摘要)、没做的、替用户决定的、坑、下一步。
- `CLAUDE.md`:`dsh/` 模块说明 + `engine` 配置一行 + 如何本地跑 dsh 引擎。

## 完成标准(按优先级,越靠前越必须)
1. Step 1 验收 1A/1B 通过(`engine=dsh` 能聊、能走硬件 MCP 编译)。
2. Step 2 验收 2 通过(Web 模式下审批/取消/计划/新建/恢复可用)。
3. Step 3b/3d 通过(证据链真实、无品牌/模型名泄漏)。
4. Step 4 通过;Step 5 至少能出 darwin-arm64 含 sidecar 的包并在解压目录跑通 1A。
5. 三套 + `dsh/` typecheck 全绿;native 路径回归无变化。
最后一条消息:新增 commit 列表、改动文件、每条验收的实际输出、没做到/替用户决定/坑、"待用户拍板"清单。然后停止。
