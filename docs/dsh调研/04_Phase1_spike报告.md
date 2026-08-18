# 04 · Phase 1 Spike 报告(分支 A)

> 日期 2026-08-18。无人值守执行。分支 `spike/dsh-engine`(从 `main-v2` 切,**未 push,main-v2 指针未动**)。dsh 版本 `0.1.0-rc.7`。
> G0 走宽松版判"有条件进 Phase 1",故执行分支 A。**本报告诚实标注做到哪、验到哪、什么被凭证/环境卡住。**

## 1. 做了什么(已在分支上 commit)

新增 Go 包 **`internal/engine/dsh`**(纯增量,不改现有 native 路径):

| 文件 | 职责 | 验证 |
|---|---|---|
| `protocol.go` | dsh SDK JSON-RPC wire 类型(InitializeParams/Result、SessionPromptParams/Result、session.event/status 通知)+ 方法/事件名常量,对齐 dsh `packages/sdk/protocol` | 编译 |
| `linerpc.go` | **newline-delimited JSON-RPC 2.0 客户端** `LineClient`:请求/通知/响应分派、pending map、畸形行忽略、并发安全、EOF 拒绝在飞请求 | `linerpc_test.go`(net.Pipe + 忠实复刻协议的 fake server:initialize/prompt 往返、通知投递、错误响应) |
| `mapper.go` | dsh 会话事件 → `internal/event.Event` 映射(text/reasoning delta、message+usage、tool call/result、turn start/end) | `mapper_test.go`(含**网关红线测试**:request/header、request/context 必被丢弃) |
| `scrub.go` | **模型名兜底脱敏**:把残留真实 provider/model/网关串从事件文本擦成占位符 | `scrub_test.go` |
| `sidecar.go` | 进程守护 + `Engine` 门面:Start(拉起+initialize 握手)、Submit(session/prompt)、Cancel(=杀进程)、Shutdown(shutdown RPC + SIGTERM→SIGKILL 梯)、通知路由→映射→脱敏→sink;childEnv 从环境注入网关 base URL/token(不落盘)、下发档位占位符 model | 编译;逻辑单测由 mapper/scrub/linerpc 覆盖 |
| `tailbuffer.go` | dsh 子进程 stderr 尾缓冲(诊断走 stderr,协议走 stdout) | 编译 |

配置(`internal/config/render.go` + `render_test.go` 已 commit;`config.go` 见 §4 未 commit 说明):新增顶层 `engine = "native"|"dsh"` + `[dsh]` 段(bin_path/args/version/startup_timeout_sec/gateway_base_url/gateway_token_env/model_placeholder),`TestRenderTOMLRoundTrips` 已扩展并通过。

## 2. 验收目标达成情况(诚实标注)

| 目标 | 状态 | 说明 |
|---|---|---|
| 三套 `build·vet·test`(根)+ desktop `build·vet·test` + frontend `pnpm tsc --noEmit` 全绿 | ✅ **全绿** | 已实跑:根 build/vet/test 通过、desktop build/vet/test 通过、frontend tsc 通过;gofmt 干净 |
| `reasonix run "<ESP32 编译题>"` 在 `engine="dsh"` 下走通 | ❌ **未达(凭证阻塞)** | 需要 ①活的网关/DeepSeek 凭证 ②把 Engine 接进 control.Controller(见 §3)。本机无凭证,Controller 接线为控绿未做 |
| Go 驱动层能与真实 dsh sidecar 握手(keyless 冒烟) | ⚠️ **部分** | `dsh web` 的 npx 分发**不含** `dsh-sdk-jsonrpc-server`(只捆 web 插件;该包在 npm 单独发布 0.0.1-rc.5),故未对真实 SDK server 冒烟;LineClient 已用**忠实复刻 dsh 协议源码的 fake server** 单测。另:无 key headless 跑通到模型调用前一步(见 §5) |
| 桌面端切 dsh 引擎聊天+工具+审批+取消 | ❌ **未做** | 依赖 Controller 接线;且审批在 SDK wire 上缺失(见 01/§6) |

**一句话**:spike 的**驱动层内核(协议/映射/脱敏/进程守护)已建成并三套全绿**;**端到端"engine=dsh 跑 ESP32 编译题"因无凭证 + Controller 未接线,未达成**——这两件都需要你提供凭证 / 拍板后才能自主推进。

## 3. 未做:Controller 接线(设计,非代码)

为控绿且避免在无凭证下大改 `internal/control` 破坏 native 路径,**本 spike 未把 `dsh.Engine` 接进 `control.Controller`**。设计建议(Phase 1 收尾按此做):

- **抽接口而非改类**:在 `internal/control` 定义一个最小 `engineBackend` 接口(Submit/Send/Cancel/Running/Approve/PendingApprovals/SetPlanMode/History/NewSession/Resume/SessionPath),现有 native 逻辑实现它为 `nativeBackend`,`dsh.Engine` 适配为 `dshBackend`。Controller 持有一个 `engineBackend`,其余方法对 dsh 后端返回明确的"dsh 引擎暂不支持"错误。
- **boot.Build 分流**:读 `cfg.Engine`,`"dsh"` 时构 `dshBackend`(New+Start,注入 sink、CWD、网关 base URL、`os.Getenv(cfg.DSH.GatewayTokenEnv)`、SecretsToScrub=[真实 model 占位映射来源]),否则走现有 native 装配。**boot 秒级,别持 a.mu 跨 Start**(桌面多标签同规矩)。
- **事件 Kind**:本 spike 的映射只用了现有 Kind(Text/Reasoning/Message/ToolDispatch/ToolResult/Usage/TurnStarted/TurnDone/Notice),**无需新增 event.Kind,`desktop/wire.go`/`internal/serve/wire.go` 不用动**。若后续要映射审批,再评估。

## 4. `config.go` 未纳入 commit 的说明(保住你的账号改动)

`main-v2` 当前有一批**未提交的账号系统改动**,其中 `internal/config/config.go` 已是 modified。我的 `engine`/`[dsh]` 字段 + `DSHConfig` 结构 + `Default()` 改动**叠加在**这些账号改动之上,同处一文件、无法无损自动拆分。为**保住你未提交的账号改动**(若把 config.go 提交到 spike,checkout 回 main-v2 时会把账号改动吞掉),我**故意没把 config.go 纳入 spike commit**,它连同账号改动留在工作树(未提交)。

- 现状:工作树里 config.go 含 dsh 字段 → 三套全绿是**在含该改动的工作树上验的**。
- 影响:单独 checkout spike HEAD 不含 config.go 的 dsh 字段,`render_test.go`/`engine` 引用的 `DSHConfig` 会缺 → 隔离编译不过。这是**为保账号改动付的代价**,正式做 Phase 1 时应:先把账号改动 commit/stash 到 main-v2,再在干净分支上把 config.go 的 dsh 字段一起 commit。
- **待你拍板**:是否允许我把 config.go 的账号改动也一并 commit(那样 spike 自洽可编译,但会把账号在飞改动卷进 spike 历史)。默认选了更保守的"不卷入"。

## 5. 发现的坑(spike 期实测)

1. **npm cache 权限冲突**:并发 `npx @deepseek-ai/dsh` 撞 `~/.npm/_cacache`(`EACCES/EEXIST rename`)→ 用隔离 `npm_config_cache` 解决。打包/CI 要注意。
2. **web 分发不含 SDK jsonrpc server**:`dsh web` 只捆 web 插件;驱动嵌入要的 `dsh-sdk-jsonrpc-server` 需单独装或走 headless/源码。**打包 sidecar 时要显式把 jsonrpc-server + 依赖闭包装进去**。
3. **无 key headless 错误体泄漏**(运行时实证):`dsh --profile headless "hi"` 无 key → stderr `MISSING_CREDENTIAL: llm-deepseek: no API key for provider route "deepseek-official"; ...export DEEPSEEK_API_KEY`——**泄漏 `deepseek-official`/`llm-deepseek`/`DEEPSEEK_API_KEY`**。验证了 Scrubber 的 secrets 集必须包含这些品牌串。
4. **首启慢**:bundled 运行时首次构建以分钟计、期间 stdout 空;打包版要预构建避免首启卡顿。

## 6. 对规划文档 Phase 2 工作量的修正

1. **审批是架构分叉不是工程量**:SDK wire 无审批(dead capability),ACP 有审批但丢流式。桌面端交互审批要么等上游给 SDK 补 `request_permission`,要么 dsh 侧派生一个带审批的自定义协议插件,要么桌面端接受"headless 自动批 + 关键写操作走 Go 侧二次确认"。规划 2.3 需加这条决策。
2. **隐藏 provider 品牌需自命名适配器插件**:纯配置藏不住 `deepseek-official`(错误体/事件都带)。Phase 2.3 要加"写一个 `onecreat-gateway` 路由名的薄 OpenAI 兼容适配器"这件事(小,但不是零)。
3. **证据引擎是诚实性唯一来源,必须随迁**:接入点已定位(`tools/post-execute`/`tool/result`/`todo/write` 事件流,Go 驱动层消费)。规划 2.2 的"先不重写"成立,但要强调"不迁=诚实性退化",不是可选。
4. **打包要显式装 jsonrpc-server 闭包**(见 §5.2),规划 1.5 的 sidecar 装配要点补上。
5. **session 单一真源**(规划 1.6):dsh JSONL(zstd)/SQLite 与我们 Go 会话文件格式/位置都不同,`engine=dsh` 时让 dsh 独占其 store、Go 侧不写会话文件即可干净切分;但**dsh store 落盘含 model/provider**,必须保证下发的是占位符(否则本机会话文件泄漏真名)。

## 7. 给 Phase 1 收尾 / Phase 2 的下一份执行提示词草稿

> 前置:你已提供一个可用的 `DEEPSEEK_API_KEY`(直连)或网关 `baseURL`+token,并已把 main-v2 账号改动 commit/stash。

1. 在干净的 `spike/dsh-engine` 上,把 `config.go` 的 `engine`/`[dsh]` 字段一起 commit(§4)。
2. 装 dsh sidecar 运行时:`npm i @deepseek-ai/dsh-sdk-jsonrpc-server @deepseek-ai/dsh-llm-deepseek`(+ 依赖),写最小 `cordis.yml`(仿 `examples/jsonrpc-agent/cordis.yml`:jsonrpc-server + llm-deepseek[baseURL=网关] + subprocess/bash/fs + session-jsonl + agent-spine + `dsh-mcp-client`→`bin/onecreat-hardware-mcp`)。
3. 写 `internal/engine/dsh` 的**真实 sidecar 集成测试**:`Engine.Start`→`Submit("编译一个 ESP32 blink")`→消费 session.event→断言出现 `mcp__hardware__arduino_compile` 工具调用且编译成功;解压 dsh JSONL 会话核对 `request/header.model` 是占位符。
4. 按 §3 把 `dsh.Engine` 接进 `control.Controller`(抽接口),`boot.Build` 按 `cfg.Engine` 分流,`reasonix run "<ESP32 编译题>" `(engine=dsh)跑通(G1 必达项)。
5. 桌面端一个 tab 切 dsh 引擎冒烟(前端零改动为原则)。
6. 三套 build·vet·test·tsc 全绿;每稳定点 commit 不 push。
7. 产出 Phase 2 工作量修正 + 证据引擎迁移的接线设计。

## 8. 未做清单(汇总)

- Controller 接线(§3,设计已给)。
- 真实 dsh sidecar 集成测试(§2,凭证 + jsonrpc-server 安装阻塞)。
- `reasonix run` engine=dsh 端到端(凭证 + 接线阻塞)。
- 桌面端切 dsh 冒烟。
- config.go 的 dsh 字段入 commit(§4,为保账号改动主动搁置)。
