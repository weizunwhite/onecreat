# 任务:dsh sidecar 从 0.1.0-rc.7 升级到 0.1.0-rc.8(备用,待用户说"开始升级"再执行)

> 起草 2026-08-20(基于当天对 rc.7→rc.8 diff 的调研,见文末"调研快照")。执行时**先重做 Step 0 的差异复核**——
> 如果那时已经出了 rc.9+ 或正式版,本文档的结论要重新核对,不可照抄。
>
> 仓库 `/Users/localwork/06_System/onecreat`。从 `main-v2`(若 dsh 工作已合入)或 `feat/dsh-engine` 切新分支
> `chore/dsh-rc8`,所有工作在其上,分步 commit(中文,末尾 `Co-Authored-By: Claude Opus <noreply@anthropic.com>`),不 push。
> 先读:`CLAUDE.md`、`dsh/README.md`、`docs/dsh调研/05_Phase1-2_实施报告.md`(§2 wire、§6 坑)、
> `internal/engine/dsh/`(尤其 `protocol.go`/`sidecar.go`/`e2e_gateway_test.go`)、`dsh/plugins/control/index.js`、`dsh/plugins/gateway/index.js`。

## 为什么升(2026-08-20 调研结论)

1. **`llm-deepseek` 修复**:"pass reasoning content back on every reasoned turn"(commit 583894f7ae)——DeepSeek
   推理模型每轮把 reasoning content 传回,直接的生成质量修复,是本次升级的主要动机。
2. **SDK server 修竞态**:`initialize` 现在 `await ctx.get('loader')?.await()`(packages/sdk/server/src/index.ts)——
   MCP 工具发现没完成前不应答 initialize。我们受益(硬件 MCP 在 initialize 后立即可用,不再有窗口期)。
3. **Windows**:pwsh 持久 pty(PR #2300)合入,Windows sidecar 的 shell 工具更稳。
4. **`llm-pi-ai` 成熟**(226 个 commit):多 provider 适配器,路由名自声明 + `apiKeyEnv` 按请求解析 +
   OpenAI 兼容网关纯配置——**有机会退役我们手写的 `dsh/plugins/gateway`**(见 Step 4,可选)。

## 已知风险(执行时逐项核)

- **Session 存储改了 SQLite v2 布局**(commit 93b4b98ef3 "optimize SQLite persistence layout" + session.list
  projection)——我们的 resume(`onecreat/session.load`,dsh 会话 id 由 Go 会话文件路径 sha1 派生)和
  落盘泄漏扫描都按 rc.7 行为写的,必须复验;**rc.7 时代的旧会话在 rc.8 下能否 load 要实测**,不能就在
  报告里写明"升级后旧会话不可恢复"并让 Go 侧对 load 失败给出友好错误(新开会话,不崩)。
- **官方明说 rc 间会破坏兼容**。我们自有 `dsh/plugins/control/index.js` 里 `new HarnessSdkJsonRpcServer(...)`
  **直接构造官方 server 类**并在同一 transport 上加自有方法——这是最脆的接缝,rc.8 里该类构造签名/
  transport 行为变没变要逐行 diff(`packages/sdk/server`)。
- 品牌/branding 相关动了不少(deployment branding slots、GUIDELINES.md)——`includeHarnessIdentity: false`
  仍存在(已确认 packages/core/system-prompt 还有),但**要重跑全套泄漏扫描**。
- `dsh-sdk-jsonrpc-demo`(我们 sidecar 的入口 bin)与 agent-teams 相关目录在 rc.8 有改名动作
  (worktree-rename-team / prefix experimental)——确认我们 profile 引用的包名、`dsh/package.json`
  依赖名在 npm 上 rc.8 版本都存在。

## 硬规则

- 不派子代理;无人值守不停下来问;需拍板按保守默认并写进报告。
- **锁精确版本**:`dsh/package.json` 全部 `@deepseek-ai/*` 依赖从 rc.7 对应版本改到 rc.8 对应版本
  (逐个查 npm 实际发布号,不用 `^`/`~`),`pnpm-lock.yaml` 重新生成提交。
- 网关红线不变:真实 provider/model 名、`deepseek-official`/`llm-deepseek`、网关 URL 不进 UI/日志/错误体/落盘。
- 过不了验收就**停在哪步报哪步**,不硬迁;整个分支可丢弃(这正是开独立分支的意义)。

## 步骤

### Step 0 — 差异复核(先做,半天)
1. `cd /Users/localwork/06_System/_ref/deepseek-harness && git fetch --tags`,确认目标 tag(默认
   `dsh-v0.1.0-rc.8`;有更新版本则停下来把本文档"为什么升/风险"两节重核一遍再继续)。
2. 逐个 diff 我们依赖的接缝,输出一页笔记 `docs/dsh调研/06_rc8升级笔记.md`:
   - `packages/sdk/protocol` + `packages/sdk/server`:方法名/参数/通知结构/`HarnessSdkJsonRpcServer`
     构造与 `createSession` 的 `agentOptions:{provider,model}`(05 报告 §6 坑 1)变没变;
   - `packages/llm/llm-deepseek`:env 名(`DEEPSEEK_*`)、`baseURL`/`apiKeyEnv` 配置形状;
   - `tools/pre-execute`/`tool/call`/`tool/result`/`todo/write` 事件形状(证据桥依赖 §6 坑 4 的
     `message.content[0].toolCallId` 配对);
   - `dsh-plan-mode`/`compaction-basic`/`dsh-session-checkpoint-policy`/`dsh-user-approval` 的 id 与配置;
   - `TokenUsage` 字段语义(§6 坑 3)。
3. 据此列"必须改的清单";与本文档预判不符的地方以实际 diff 为准。

### Step 1 — 升依赖 + 修编译
`dsh/package.json` 锁 rc.8 → `pnpm -C dsh install` → 按 Step 0 清单修 `dsh/plugins/*` 与
`internal/engine/dsh`(wire 类型变了就同步 `protocol.go`)→ `pnpm -C dsh typecheck` 绿 +
根/desktop(!web 与 -tags web)/frontend 三套全绿。

### Step 2 — 行为回归(全部实跑)
1. 单测:`go test ./internal/engine/dsh/ -count=1` 全绿;`ONECREAT_DSH_E2E=1 go test ./internal/engine/dsh/ -run 'E2E|Credentials|Tier' -v`
   (真 node sidecar + 假网关:tier 下发、token 轮换)全绿。
2. 直连冒烟:`bin/reasonix run --engine dsh "用一句话介绍你自己"`(~/.env 有 key,内核 loadDotEnv 加载,别读文件)
   ——流式正常、**reasoning 内容确认在传回**(对照升级动机 1:看 usage 的 reasoning token 与回答质量)。
3. 工具+证据:`--engine dsh` 跑一道 ESP32 编译题,`mcp__hardware__arduino_compile` 走通、证据账本有真实链条。
4. Web 模式:`make build-web` + `ONECREAT_ENGINE=dsh`,审批弹窗/取消/计划模式/新建会话正常;
   **resume 专项**:rc.8 下新建会话→退出→ResumeSession 正常;再用一个 rc.7 时代的旧会话文件试 load,
   记录结果(见风险 1)。
5. 泄漏扫描:假网关跑一轮 + 故意 401,grep dsh 落盘会话(注意 SQLite v2:用它自己的导出或直接扫库文件)与
   stderr:真实模型名/`deepseek-official`/`llm-deepseek`/网关 URL/token 全部 0 命中;
   `includeHarnessIdentity: false` 仍生效(问"你是什么模型"不自报家门)。
6. 退出:多 tab 各自 sidecar,Quit 后无 node 残留。

### Step 3 — 打包回归
`SKIP_FRONTEND=1 scripts/web-build.sh darwin/arm64 v0.0.0-rc8`:dsh-bundle 装的是 rc.8 闭包;解压到
临时目录、无环境变量运行,`ONECREAT_ENGINE=dsh` 跑通一轮对话。记录包体积变化(rc.7 基线:59.3 MB 压缩 / +173 MB 解压)。

### Step 4 —(可选,时间允许才做)评估 `llm-pi-ai` 替代 gateway 插件
1. 用 `@deepseek-ai/dsh-llm-pi-ai` 在 profile 里声明自定义路由 `onecreat-gateway`(baseURL=网关,
   `apiKeyEnv=ONECREAT_DSH_API_KEY`,OpenAI 兼容协议),替换 `dsh/plugins/gateway`。
2. 重跑 Step 2.1 的 e2e + 2.5 的泄漏扫描:**尤其错误体**——`MISSING_CREDENTIAL` 等报错里带不带
   `llm-pi-ai`/`pi-ai` 字样,带就要么配置能改要么保留我们的插件,把结论写进报告。
3. 成了就删 `dsh/plugins/gateway` 并更新 `dsh/README.md`;不成就保留原插件,记录原因。
   注:credentials.set 轮换(`process.env` 每请求读取)必须仍然生效——llm-pi-ai 的 "apiKeyEnv 按请求解析"
   理论上正好兼容,实测为准。

### Step 5 — 文档与交付
- 更新 `dsh/README.md`(版本、升级记录)、`docs/dsh迁移执行规划.md`(版本号)、`CLAUDE.md` 若有涉及。
- 产出 `docs/dsh调研/06_rc8升级笔记.md`(Step 0)+ 在其末尾附回归结果表。
- 最终消息:commit 列表、每条验收实际输出、旧会话兼容性结论、Step 4 做没做及结论、没做到/坑/待拍板。

## 验收门(全过才算升级成功,否则弃分支留在 rc.7)
Step 1 全绿 + Step 2 的 1/2/3/4(新会话部分)/5/6 全过 + Step 3 打包跑通。
旧会话不兼容**不阻塞**升级,但必须有友好降级 + 写进报告。

---

## 调研快照(2026-08-20,升级时用于对照,不可替代 Step 0)

- rc.7→rc.8:1604 文件、+54064/-10533 行;参考克隆在 `/Users/localwork/06_System/_ref/deepseek-harness`
  (已 fetch 到 rc.8 tag)。
- 关键 commit:583894f7ae(llm-deepseek reasoning 传回)、93b4b98ef3(session SQLite v2)、
  e7d24de36c(pwsh 持久 pty)、d66841ea3f(web 默认自动开浏览器)、sdk/server initialize await loader。
- `llm-pi-ai` README 关键句:"an OpenAI-compatible gateway … is configuration rather than a code change";
  `apiKeyEnv` 是按请求解析的凭证引用;路由可自声明(命名不含厂商即满足品牌隐藏)。
- `includeHarnessIdentity` 在 rc.8 仍存在(packages/core/system-prompt)。
- 包名未变:`@deepseek-ai/dsh-sdk-jsonrpc-server` / `@deepseek-ai/dsh-sdk-protocol`。
