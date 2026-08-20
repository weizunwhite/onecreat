# `dsh/` —— OneCreat 自带的 DeepSeek Harness 组合包

这个目录是 OneCreat 在 `engine = "dsh"` 下拉起的 **sidecar 运行时**:一个版本锁死的
dsh(DeepSeek Harness)组合(cordis profile)+ OneCreat 自己的插件。Go 内核以
newline-delimited JSON-RPC over stdio 驱动它(见 `internal/engine/dsh`)。

## 为什么自带一份,而不是 `npx @deepseek-ai/dsh`

1. `dsh web` 的分发**不含** `dsh-sdk-jsonrpc-server`(驱动嵌入必需)。
2. `npx` 首启要下载+构建,以分钟计,并且并发跑会撞 npm cache 权限。
3. dsh 是 developer preview,官方明说会有破坏性变更 —— 必须**锁死精确版本**。
4. 我们要往里塞自己的插件(控制面 / 网关适配器)。

## 版本锁定策略

所有 `@deepseek-ai/dsh-*` 依赖锁到 **`0.1.0-rc.8`**,`pnpm-lock.yaml` 入仓。
**升级 dsh 版本是一件独立的任务**:改版本 → 跑 `pnpm typecheck` →
跑 `go test ./internal/engine/dsh/...` → 用 `reasonix run --engine dsh` 过一遍
1A/1B 验收,全绿才提交。

### 升级记录

| 日期 | 版本 | 要点 |
|---|---|---|
| 2026-08-19 | `0.1.0-rc.7` | 迁移基线(`docs/dsh调研/` 的调研版本) |
| 2026-08-20 | `0.1.0-rc.8` | 主要动机是 `llm-deepseek` 的 CoT 每轮回传(生成质量修复)。协议/agent/tools/审批/system-prompt 全部零源码改动;控制面照 rc.8 官方 sdk/server 补了 `initialize` 前的 loader await。完整差异复核见 [`docs/dsh调研/06_rc8升级笔记.md`](../docs/dsh调研/06_rc8升级笔记.md) |

> `@deepseek-ai/cordis` 是底座框架,自己的版本线(`4.0.1`),不跟 dsh 的 rc 号。

## 怎么跑

```sh
pnpm install          # 首次
pnpm typecheck        # 类型检查(JS + JSDoc,checkJs)
pnpm start            # 手动起一个 stdio JSON-RPC server(调试用)
```

`pnpm start` 起来后进程用 stdin/stdout 说 JSON-RPC,**不是给人打字用的**;调试用
`node scripts/...` 之类的客户端,或直接让 Go 侧 `reasonix run --engine dsh` 拉起它。

正常情况下不需要手动跑:`boot.Build` 在 `engine="dsh"` 时会用
`node node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/bin.js profiles/onecreat.cordis.yml`
把它拉起来。

## 运行时环境变量(全部由 Go 驱动层注入)

| 变量 | 用途 |
|---|---|
| `ONECREAT_DSH_BASE_URL` | provider 的 base URL(网关地址或 `https://api.deepseek.com`)。**机密,不写进 profile** |
| `ONECREAT_DSH_API_KEY` | provider 凭证(网关 token 或 DeepSeek key)。**机密,不落盘** |
| `DSH_CWD` | agent 工作区(bash/fs 工具的 cwd) |
| `DSH_SYSTEM_PROMPT` | 系统提示(Go 侧 `Controller.SystemPrompt()` 下发) |
| `DSH_SESSION_ROOT` | dsh 自己的会话 store 目录 |
| `DSH_SESSION_PLAIN` | 设了就用未压缩 JSONL(便于红线 grep 检查) |
| `ONECREAT_HARDWARE_MCP` | OneCreat 硬件 MCP 二进制的绝对路径;设了且存在才挂载 |

## 插件清单

| 行 | 包 | 作用 |
|---|---|---|
| `onecreat-control` | `plugins/control/index.js` | **OneCreat 自己的控制面**:拥有 stdio 传输,内部复用官方 `HarnessSdkJsonRpcServer`,并在同一条 wire 上补齐取消 / 审批桥 / 计划模式 / resume / 每轮注入 / 工具桥 / 预执行钩子;按需挂硬件 MCP |
| `onecreat-gateway` | `plugins/gateway/index.js` | **自命名 provider 路由**(路由名 `onecreat-gateway`),复用官方 OpenAI 兼容传输实现,隐藏厂商品牌名,base URL/凭证只从环境读,关闭 model catalog 广播 |
| `approval` | `@deepseek-ai/dsh-user-approval` | 审批接缝(policy `ask`);终端 answerer 是 `onecreat-control` |
| `plan-mode` | `@deepseek-ai/dsh-plan-mode` | 计划模式 |
| 其余 | 官方包 | subprocess/bash/fs/todo/token-meter/compaction/JSONL 持久化 |

**profile 里故意没有** `@deepseek-ai/dsh-sdk-jsonrpc-server` 这一行 —— 它会和
`onecreat-control` 抢 stdin(`JsonRpcLineTransport.onRequest` 是"后注册覆盖前一个")。
控制面插件在内部 `new HarnessSdkJsonRpcServer(...)` 复用它的实现。

**profile 里也故意没有任何 UI / 凭证面 / model selector 包**(`ui-model-selection`、
`ui-settings-models`、`credentials-local` …)—— 那些会让用户看见并修改模型与 API key,
和 OneCreat"只见档位、不见模型"的红线直接冲突。

## 补在 wire 上的 OneCreat 方法/通知

请求(Go → dsh):

| 方法 | 载荷 | 说明 |
|---|---|---|
| `onecreat/session.cancel` | `{sessionId}` | 取消当前 turn(`agent.cancel({kind:'user'})`),不再靠杀进程 |
| `onecreat/planMode.set` | `{sessionId, active}` | 驱动 `dsh-plan-mode` |
| `onecreat/inject` | `{sessionId, text}` | **每轮运行时状态注入**(板卡事实/人设/记忆),走 `agent.inject()` 进下一次 pre-step,**不进系统提示**,保住前缀缓存 |
| `onecreat/session.load` | `{sessionId}` | 从持久化 resume,并回传消息投影 |
| `onecreat/session.history` | `{sessionId}` | 取消息投影(前端 History) |

通知(dsh → Go,Go 按 `id` 回一条应答通知):

| 出站 | 应答 | 说明 |
|---|---|---|
| `onecreat/approval.request` | `onecreat/approval.resolve` | 审批桥:Go 侧变成现有 `event.Approval`,复用 native 的审批 UI/语义 |
| `onecreat/tool.invoke` | `onecreat/tool.result` | 工具桥:`complete_step` 在 Go 侧执行(证据引擎是它的裁判) |
| `onecreat/tool.preExecute` | `onecreat/tool.preExecute.done` | 预执行钩子:Go 侧在写操作前做文件快照(checkpoint/rewind) |

## 红线(改这里之前先读)

- **绝不**把真实 provider/model 名、网关 URL 写进本目录任何文件;它们只从环境变量来。
- **绝不**往 profile 里加 stdout logger 或终端 UI —— stdout 是协议通道。
- **绝不**加 dsh 的 Web UI / 凭证面 / model selector 包。
