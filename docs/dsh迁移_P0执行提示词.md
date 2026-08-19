# 任务:OneCreat 底层 agent 迁移到 DeepSeek Harness(dsh)—— 无人值守执行 Phase 0(验证)+ 视结论继续 Phase 1 spike / pi 对照

## 背景(必读)

- 仓库:`/Users/localwork/06_System/onecreat`(分支 `main-v2`,Go 内核 + Wails 桌面端;项目说明见仓库 `CLAUDE.md`,**先读它**)。
- 决策已定:候选 pi / dsh 里选 **dsh**(DeepSeek 官方 harness,<https://github.com/deepseek-ai/deepseek-harness>),理由:产品 DeepSeek-first、"一切皆插件"正对我们的护城河(证据引擎/硬件 MCP/网关档位)。
- 完整分阶段规划在 **`docs/dsh迁移执行规划.md`**,**先读它**。本任务只做其中 **Phase 0**,目标是回答一个问题:**"dsh + DeepSeek 是否明显强于我们现在的 Go 内核,且能接我们的平台网关"**。答不出"是",后面就不迁。
- Phase 0 **不改仓库任何代码**。所有产出是文档、笔记、对比数据。

## 硬约束

1. 不派子代理,自己从头做到尾。
2. 用中文写所有产出。
3. **网关红线**:OneCreat 在线版走平台 AI 网关(OpenAI 兼容端点,token 环境变量 `ONECREAT_GATEWAY_TOKEN`,实现见 `internal/provider/openai/openai.go`,策略见 `internal/config/config.go` 的 `ModelPrivacyPolicy`),用户只能看到档位(标准/高级/旗舰),**真实模型名是机密**。验证过程中任何会把真实模型名打到 UI/日志/错误体的地方都要记下来,这是决定性缺陷之一。
4. 网关地址/token 从本机环境或 `.env` 取,不要写进任何提交文件。
5. dsh 是 developer preview,会有破坏性变更——记录你实测时的 **dsh 精确版本号**。
6. **全程无人值守,我在睡觉,任何情况都不要停下来问我。** 遇到需要拍板的事,自己按下面的默认值决定、写进报告"待确认"一节,继续往下做:
   - 找不到网关配置 → 改用本机 `DEEPSEEK_API_KEY` 直连 DeepSeek 官方 API 做 Step 1/3,Step 2 标"网关未验证,原因:…",列出你找过的位置。
   - 真机不在/串口找不到 → 只跑纯编译题,标"未真机"。
   - 某步报错卡住 → 最多自查修 3 轮,不行就记录现象跳过,别死磕。
   - 要装工具/花 API 钱 → 直接装、直接跑,量级正常即可(每题 2 次、总共十几个任务)。
   - 任何二选一 → 选更保守的那个,记录理由。
7. 诚实汇报:哪些做了、哪些没做、哪些结论是猜的、哪些是替我决定的。
8. **能自主做的都做完再停**:Phase 0 做完后按 Step 5 继续(G0 过 → 在独立分支做 Phase 1 spike;G0 不过 → 用 pi 走一遍 B 计划验证)。只有需要我实际拍板/花大钱/碰生产环境的事才留给我。

## 执行步骤

### Step 1 — 安装并摸清 dsh(产出:`docs/dsh调研/01_架构与扩展点.md`)

1. `npx @deepseek-ai/dsh web` 跑起来,记录版本号、安装/启动坑。
2. clone 官方仓库到 `/Users/localwork/06_System/_ref/deepseek-harness`(仓库外),精读:`README.md`、`AGENTS.md`、`docs/architecture.md`、`docs/` 下开发文档、`packages/core`(agent-loop)、`packages/llm`(provider)、`sdk/`(JSON-RPC 协议+server+TS client)、ACP server 相关代码。
3. 输出一页笔记,必须回答:
   - agent loop 结构;文档化的扩展点清单(tool 前/后钩子、系统提示注入点、事件类型完整列表、skill 机制、MCP 接入方式、审批/权限模型、plan 模式)。
   - **嵌入方式**:JSON-RPC over stdio 还是端口?协议消息一览;`--profile headless` 怎么用;ACP server 怎么起。评估"Go 进程拉起 dsh 子进程并驱动"的可行性。
   - session 存储:格式、位置、是否有 fork/summary、外部程序能否安全读(注意生态里出过双写损坏)。
   - provider 层:是否只有 DeepSeek?base URL、header 是否可配?
   - 与我们现有概念的映射表(Controller 方法 ↔ dsh 能力):至少覆盖 Submit/Cancel/Approve/PlanMode/History/NewSession/Resume/Rewind/Fork/Compact/MCP。Controller 方法清单看 `internal/control/`。

### Step 2 — 接平台网关(产出:`docs/dsh调研/02_网关接入验证.md`)

1. 找到本机网关配置(看 `onecreat.toml`、`~/.config/onecreat/config.toml`、`.env`、`internal/boot/boot.go` 网关分支)。
2. 让 dsh 的 provider 指向网关(改 base URL + 带 token header)。记录:改哪、能不能只靠配置不改源码。
3. 跑一次简单任务确认通。
4. **泄漏检查**:故意制造错误(错 token、错模型名、超长输入)+ 正常对话,检查 dsh 的 Web UI、终端日志、session 文件、错误响应里是否出现真实模型名/路由信息。逐条列出。
5. 检查 dsh 有没有 UI 让用户改模型/API 配置——有的话记下来(将来必须锁死)。
6. 结论:可行 / 可行但有 N 处要堵 / 不可行。

### Step 3 — 实测对比(产出:`docs/dsh调研/03_实测对比.md`)

1. 用例集:优先复用仓库里已有的硬件 dogfood 用例(在 `docs/`、`internal/hardware/`、e2e 目录、以及 Claude 记忆库里找 "dogfood / 真机 / ESP32 实测" 相关记录);找不到就自己定 6 题,覆盖:ESP32 点灯+串口 / 传感器读数 / WiFi 连接 / 一个会引发编译错误需要自修的题 / 一个板卡不支持要求诚实拒绝的题 / 一个多步骤需要计划的题。写明每题的验收标准。
2. 真机:CH340 ESP32(串口 `/dev/cu.usbserial-*`)。真机不在就只做纯编译题,标注"未真机",不要等。
3. 两边跑:**A = 现有 OneCreat 内核**(`reasonix run "<task>"` 或桌面端),**B = dsh 原生**(headless 或 Web UI),都走网关同一档位。dsh 侧把 `bin/reasonix-hardware-mcp` 作为 MCP 注册给它(这是我们现成的硬件工具),没有它 dsh 没法编译烧录——这一步本身就是"硬件 MCP 能否零改动接入"的验证,单独记结论。
4. 每题记:一次成功率、工具调用轮数、token、耗时、是否胡说/伪造结果(诚实性)、是否需要人工干预。每题至少跑 2 次。
5. 结论表格 + 三句话总结。**要求诚实**:没有明显差距就写没有。

### Step 4 — G0 结论(产出:`docs/dsh调研/00_G0结论.md`)

汇总 Step 1–3,明确写:
- 网关:可行/不可行 + 要堵的漏点清单。
- 能力:dsh 明显更强 / 相当 / 更弱,证据是哪张表。
- 建议:进 Phase 1 / 不迁 / 需要补哪些验证再定。
- 如进 Phase 1,列出你在调研中发现的、规划文档里没写到的风险与工作量修正。

写完 G0 结论后**不要停**,按下面 Step 5 继续。

### Step 5 — 按 G0 结论继续(二选一,自己判断)

**分支 A:G0 = 进 Phase 1**(网关可行或仅有可堵的漏点,且 dsh 能力明显更强或相当但生态/官方维护价值明确)→ 做 Phase 1 spike:

0. 先 `git stash` 或 commit 当前工作区未提交改动(现在 `main-v2` 有一批未提交的账号系统改动,别弄丢);然后从 `main-v2` 切新分支 `spike/dsh-engine`,**所有代码只在这个分支上做,不 push,不动 main-v2**。
1. 读 `docs/dsh迁移执行规划.md` Phase 1 的 1.1–1.4,按它做:
   - 新包 `internal/engine/dsh`:拉起/守护 dsh 子进程(headless + JSON-RPC 或 ACP,用 Step 1 里验证过的那种),进程生命周期、超时、崩溃重启。
   - Controller 接入:先看 `internal/control` 现状,选侵入最小的方式(抽接口或加第二实现),实现最小子集 `Submit/Send/Cancel/Running/Approve/PendingApprovals/SetPlanMode/History/NewSession/Resume/SessionPath`,其它方法先返回"dsh 引擎暂不支持"的明确错误。
   - 事件映射:dsh 事件 → `internal/event.Event`(text delta / tool start-end / approval / usage / error / done)。`desktop/wire.go` 与 `internal/serve/wire.go` 只在需要新 Kind 时才动,并保持两边一致。
   - 配置:`engine = "native" | "dsh"`(默认 native)+ `[dsh]` 段(可执行路径/版本/额外参数),接进 `boot.Build`;`internal/config/render.go` 渲染新字段并扩展 `TestRenderTOMLRoundTrips`。
   - 硬件 MCP:把 `reasonix-hardware-mcp` 注册给 dsh 引擎。
   - 网关:dsh provider 指向网关;Go 驱动层对事件/错误做模型名兜底过滤(对齐 `ModelPrivacyPolicy`)。
2. 验收目标(能到哪算哪,诚实标注):
   - `reasonix run "<ESP32 编译题>"` 在 `engine="dsh"` 下走通(必达)。
   - 桌面端一个 tab 切 dsh 引擎能聊天+工具+审批+取消(尽力,前端零改动为原则,实在需要改前端就只改 `bridge.ts`/`types.ts` 并同步 mock)。
   - `go build ./... && go vet ./... && go test ./...`(根)+ `cd desktop && go build ./... && go vet ./... && go test ./...` + `cd desktop/frontend && pnpm tsc --noEmit` 全绿。
3. 每到稳定点就在分支上 commit(中文 commit message,末尾加 `Co-Authored-By: Claude Opus <noreply@anthropic.com>`),不 push。
4. 打包(1.5)和 session 单一真源(1.6)不做,只在报告里写你的设计建议。
5. 产出 `docs/dsh调研/04_Phase1_spike报告.md`:做到哪一步、怎么验的、没做的、发现的坑、对规划文档 Phase 2 工作量的修正、以及给 Phase 1 收尾/Phase 2 的下一份执行提示词草稿。

**分支 B:G0 = 不迁 dsh**(网关不可行且不可堵,或能力明显不如现内核)→ 用 **pi**(<https://github.com/badlogic/pi-mono>,`pi-agent-core` / `pi-coding-agent`,有 RPC 模式与多 provider)把 Step 1–3 **原样再走一遍**,产出 `docs/dsh调研/05_pi对照验证.md`,并在 `00_G0结论.md` 末尾加一节"pi 作为 B 计划的结论"。**不写代码。**

### 收尾

最后一条消息:所有产出文档路径 + G0 一句话结论 + Step 5 走的哪条分支及做到哪 + "待你拍板"清单(每条一句话)。然后停止。
