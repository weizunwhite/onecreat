> 归档自 2026-08-23 调研会话,只读事实快照,以当日代码为准

# OneCreat 现状与架构约束调研报告

> 目标仓库：`/Users/localwork/06_System/onecreat`（Go，分支 `main-v2`，HEAD `71e6f2a9`）
> 调研日期：2026-08-23 ｜ 全程只读，未改动任何文件
> 用途：为「NAS App 科创工作流方法论整体迁入 OneCreat」做迁移架构设计提供事实底座

---

## 0. 一句话总结

OneCreat 的**引擎层已经是一个成熟的通用 agent 平台**（六服务门面 + 单一引擎接缝 + 工具策略流水线 + 事件流 + 作用域生命周期，全部被 AST 守卫测试钉死）；而**"科创/教培"这一层还极其初期**——只有 7 张硬编码的启动卡（`Welcome.tsx`）、2 个 coaching persona 字符串（`App.tsx`）、1 个通用证据引擎（`internal/evidence`）、1 套硬件专用的持久化证据链（hardware MCP），**没有任何"课题 / 项目 / 阶段 / 学生 / 教师"的数据结构**，也**没有 OneUp 项目包契约**。要迁入九阶段方法论，等于**新建一个领域模块**，而不是改造现有模块。

---

## 1. 已有的「科创 / 教培」能力盘点

### 1.1 证据引擎 `internal/evidence` — 成熟度：**可用（但只是"单轮内存账本"，不是持久化证据链）**

| 项 | 事实 |
|---|---|
| 文件 | `/Users/localwork/06_System/onecreat/internal/evidence/evidence.go`（约 470 行）+ `evidence_test.go` |
| 核心类型 | `Ledger`（带 `sync.Mutex` 的 `[]Receipt`）、`Receipt`、`TodoItem`、`TodoStepMatch` |
| 生命周期 | **按用户轮清空**（`Ledger.Reset()`）。注释明写：`Receipt` 只活在当前 agent turn，**不序列化进 prompt，也不进会话状态** |
| 注入方式 | `evidence.WithLedger(ctx, ledger)` / `evidence.FromContext(ctx)` —— 由 `toolpolicy.Pipeline.Before` 在 context 注入阶段挂上 |

**它验证什么**（`Ledger` 的匹配器）：
- `HasSuccessfulCommand(cmd)` — 模型声称"我跑了这条命令"必须对得上本轮真实的 bash 收据。容忍引号/空白差异与截短（`commandsEquivalent`，最短护栏 12 字符）；MCP 工具型验证走 `toolCoreName()` 剥 `mcp__<server>__` 前缀后的包含匹配。
- `HasSuccessfulWrite(paths)` / `HasSuccessfulReadOrWrite(paths)` — 路径按 **路径段边界后缀匹配**（`pathRefersTo`），相对/绝对路径都能对上。
- `HasDeviceActionThisTurn()` — 白名单 `deviceActionTools`（`arduino_upload` / `arduino_ota_upload` / `platformio_run` / `esp_idf_run` / `mpremote_run` / `ssh_deploy_run`）。用途是**堵 `manual` 后门**：同一轮内不可能有用户新输入，所以"用户已确认烧录后现象"必然是编造的。
- `LatestTodos()` / `MatchLatestTodoStep()` / `UnverifiedCompletedTodos()` — 把 `complete_step` 与最近一次成功的 `todo_write` 列表对账，找出"偷偷翻成 completed 却没有 complete_step 签收"的项。

**消费方**：`internal/tool/builtin/completestep.go` 的 `complete_step` 工具。它强制 `evidence[]` 至少 1 条，`kind ∈ {verification, diff, files, manual}`；`verification` 必须带 `command` 且账本里查得到，否则报错（中文报错文案）。`internal/agent/agent.go` 在轮末做 todo reconcile 提醒（经 `toolpolicy.Pipeline.EndOfTurnReminder`）。

**导出格式**：`internal/evidence` **本身没有导出**。真正能"导出成竞赛材料"的是另一套——见 1.5。

**判断**：这是一台**通用、零硬件耦合的诚实性引擎**，正是 CLAUDE.md 说的"平台存在的理由"。但对科创工作流而言它缺一条腿：**跨轮/跨会话的持久化证据链**。九阶段方法论要的是"阶段三的成功标准在阶段八被验证过"，那是跨月的事，现有 `Ledger` 一轮就清空。

### 1.2 技能系统 `internal/skill` — 成熟度：**成熟（机制）/ 初期（科创内容）**

| 项 | 事实 |
|---|---|
| 文件 | `internal/skill/skill.go`（517 行，`Store` / `Skill` / `Scope` / `RunAs`）、`builtins.go`（221 行）、`index.go`（76 行）、`tools.go`（363 行，`run_skill` 工具） |
| 发现路径 | 项目根与 home 下的 `config.ConventionDirs`（`.reasonix` / `.agents` / `.agent` / `.claude`）各自的 `skills/` 子目录 + 用户自配 `CustomPaths`。跟随符号链接；支持目录布局 `SKILL.md` 与扁平 `<name>.md` |
| 优先级 | `project > custom > global > builtin`（`Scope` 常量） |
| 执行模式 | `RunInline`（body 折进父轮当 tool result）/ `RunSubagent`（隔离子循环，只回最终答案，可用 `allowed-tools` 收窄工具集、`model:` 覆盖模型） |
| 索引 | `ApplyIndex` 只把 **名字 + 描述** 拼进 cache-stable 系统提示；`IndexMaxChars = 12000`，单行 220 runes。**注释明写这个数字是为教培场景调的**："老师装了 39 个教培技能时 4000 会把索引尾部挤掉"、"描述尾部的 `Trigger on: 论文, 研究报告, 金鹏…` 触发词被剪掉=弱模型认不出技能" |

**内置技能只有 6 个，全是通用编码技能，没有一个是科创/教培的**（`builtins.go` `builtinSkills()`）：
`init` (inline) · `explore` (subagent) · `research` (subagent) · `review` (subagent) · `security-review` (subagent) · `test` (inline)

> ⚠️ **重要澄清**：用户全局 `~/.claude/skills/` 下那批科创技能（`tech-proposal-generator`、`competition-paper-generator`、`lesson-plan-generator`、`project-tutorial-manual`、`jinpeng-video-ppt`、`jinpeng-anonymization-checker`、`photo-checklist-generator`、`project-research-log`、`icc-competition-prep`、`trifold-board-designer`、`project-drawio-generator`、`research-material-templates` 等）**不在本仓库里**，是本机 `~/.claude/skills/` 的用户级技能，OneCreat 靠 `ScopeGlobal` 的约定目录扫描把它们捡起来。仓库内**唯一**的科创相关技能示例是 `docs/examples/ota/ota-remote-flash-skill/SKILL.md` 和 `ota-publish-skill/SKILL.md`（硬件 OTA，示例性质，不是内置）。

**缺口（对迁移最关键的一条）**：`Skill` 结构体**没有 `category` / `vertical` / `icon` 元数据字段**；`desktop/skills_app.go` 的 `SkillView` 只有 `{Name, Description, Scope, RunAs}`。这正是 `docs/平台重构蓝图.md` 里 **Phase 3 标记为 ❌ 未开始** 的那一项。所以现在启动台卡片是**前端硬编码数组**，不是数据驱动。

### 1.3 Coaching persona / 计划模式 / todo — 成熟度：**可用（persona 极初期）**

**Coaching persona（协作模式）**：
- 后端：`control.Controller.SetCoachMode(string)`（`controller.go:877`）→ `turnState.SetCoachMode`（`turn_state.go:173`）→ 在 `input.go:54` 于 **Compose 时**把文本包成 `<coaching-style>…</coaching-style>` 拼到用户输入尾部。**刻意不进 cache-stable 系统前缀**（DeepSeek prefix cache 铁律）。`agent/save.go:179` 在会话落盘时把这段剥掉。
- 前端：**persona 正文是硬编码在 `desktop/frontend/src/App.tsx` 第 44 / 51 行的两个中文字符串**：
  - **学生引导**（`coach.student`）："你正在辅导一名 1-9 年级的学生做科技创新项目。用引导式教学：多用提问启发…学生必须能在答辩时逐行解释自己的项目，AI 只是辅助的手…"
  - **老师助手**（`coach.teacher`）："你正在帮一线科技教育老师备课、准备项目材料…每个关键技术点都要附一句「为什么这么做」的教学解释…默认面向 1-9 年级。"
- 选择器 UI：`Composer.tsx:80`，与 计划/YOLO 审批维度**正交**。
- 判断：这是**目前 OneCreat 里最"教培"的一处代码**，但它只是两段 prompt 常量，没有分级、没有学段、没有阶段感知。**极初期。**

**计划模式（plan mode）**：
- `turnState.SetPlanMode` + `toolpolicy.Pipeline` 的第一阶段只读门。
- **计划种子**：`internal/control/plan_seed.go` 的 `seedPlanTodos` / `PlanTodosJSON` / `parsePlanTodos` —— 用户批准计划的那一刻，把计划正文的 markdown 列表解析成两级 todo 并**合成一条 `todo_write` 事件**注入。注释明写理由："『批准即执行』要求列表在执行第一步之前就存在，**证据引擎（complete_step）也要对着这份列表核对**"。这是现有系统里**最接近"阶段—任务—验收"结构化**的一处。
- **auto-plan**：`internal/control/auto_plan.go` + `auto_plan_classifier.go`，`off/ask/on` 三态（`normalizeAutoPlan`，默认 `ask`）。蓝图里说"auto-plan 强切已删"指的是不再无条件强切。

**todo**：`internal/tool/builtin/todo.go` 的 `todo_write`（含 `level` 字段支持两级）；前端 `TodoPanel.tsx`（68 行）+ `TaskContextBar.tsx`（141 行，**含硬件上下文文件白名单** `hardware_manifest.json` / `docs/wiring.md` / `docs/verification.md` / `docs/board_profile.md` / `tests/hardware_checklist.md` / `tests/hardware_evidence.jsonl` / `src/main.cpp` / `platformio.ini`，和一组硬件关键词）。

### 1.4 知识库（KB）与记忆 — 成熟度：**可用（KB 是第一版，检索很朴素）**

**知识库**（纯 desktop 层，**内核里没有**）：
- 文件：`desktop/knowledge_app.go`（约 780 行）+ `desktop/frontend/src/components/KnowledgePanel.tsx`（344 行）
- 存储：`os.UserConfigDir()/onecreat/knowledge/`（`knowledgeRootDir()`），一个 JSON store（`knowledgeStore` / `knowledgeBaseRecord` / `knowledgeDocumentRecord` / `knowledgeChunkRecord`）+ 复制进来的原文件
- 检索：`knowledgeSearch` + `knowledgeScore` + `knowledgeTokens`（**自研词袋打分，含 `isCJK` 中文分词兜底**），无向量、无 embedding
- 注入：`KnowledgeBuildPrompt(baseIDs, question, limit)` → 前端 `App.tsx:713` 在提交前把检索片段拼成增强 prompt（"Mode A"）
- 限制（硬编码在 `knowledgeImportOne`）：单文件 ≤ 8MB；**只支持文本/Markdown/代码**，"PDF/Word/PPT/Excel 后续再接入解析"；不支持导入文件夹
- 判断：**这是迁移科创方法论最直接可用的载体之一**（把九阶段文档、问卷模板、查新流程灌进去），但 **不能导入 docx/pdf 是硬伤**——方法论资料多半是 Word。

**记忆**：
- `internal/memory`（`doc.go` / `store.go` / `remember.go` / `forget.go` / `queue.go` / `quickadd.go`）
- 文档名优先级：`docNames = []string{"ONECREAT.md", "REASONIX.md", "AGENTS.md", "CLAUDE.md"}`；新建默认 `defaultDocName = "ONECREAT.md"`
- 作用域：`ScopeUser`（用户配置目录）/ `ScopeAncestor`（项目根之上各级）/ `ScopeProject`（`./ONECREAT.md`）；支持 `@import` 展开（`resolveImports`，有深度限制与去环 `docSeen`）
- 工具：`remember` / `forget`；`memory.Queue` 让工具在 context 里排队笔记，随下一轮上车
- 服务：`control.memoryService`（`internal/control/memory.go`）+ `desktop/memory_service.go`（`MemoryView` / `MemoryDoc` / `MemoryFact` / `MemoryScope`）+ `MemoryPanel.tsx`（574 行）
- 判断：**成熟**。九阶段方法论的"项目级事实"（课题句子、成功标准、形态锁定结论）天然适合落在项目根的 `ONECREAT.md`。

### 1.5 硬件 MCP 与硬件证据链 — 成熟度：**成熟**

- 二进制：`cmd/reasonix-hardware-mcp/main.go`（约 2500+ 行），**27 个工具**。清单：
  `hardware_detect` · `hardware_board_profile` · `hardware_module_spec` · `hardware_project_scaffold` · `hardware_project_context` · `hardware_project_validate` · `hardware_project_audit` · `hardware_project_repair` · `hardware_repair_catalog` · `hardware_evidence_record` · `hardware_evidence_status` · `hardware_device_verify_plan` · `arduino_compile` · `arduino_core_install` · `arduino_lib_install` · `hardware_install_toolchain` · `hardware_install_arduino_cli` · `hardware_install_core` · `arduino_upload` · `arduino_ota_upload` · `firmware_publish` · `arduino_monitor_sample` · `platformio_run` · `esp_idf_run` · `esp_idf_mcp_config` · `mpremote_run` · `ssh_deploy_run`
- 板卡事实：`internal/hardware/boards/boards.json`（851 行）+ `boards.go`，经 `App.HardwareBoardFacts` 在写代码前**硬注入 prompt**（`HardwareBoardFactsView{Found, Facts}`）
- **持久化证据链（与 1.1 的内存账本是两套东西）**：
  - 落盘：`<project>/tests/hardware_evidence.jsonl` + `<project>/tests/hardware_checklist.md`
  - 写入：MCP 工具 `hardware_evidence_record`（会与本会话真实跑过的对应工具输出核对，编造的串口日志算不上 `hardware_verified`）
  - 读出：`desktop/hardware_service.go` 的 `EvidenceStatus()`（调 `hardware_evidence_status`）与 `EvidenceExport(projectDir)`（`hardware_service.go:391`）
  - **导出格式**：`renderEvidenceMarkdown()` 产出一份中文 Markdown，标题「# 真机验证证据（onecreat 自动导出）」，按 `evidenceStageLabel()` 翻成「编译/语法 / 烧录 / 串口·运行日志 / 真机部署 / MicroPython 部署」，每条含 时间(UTC) / 平台·板卡 / 端口 / 结果 / 命令 / 输出片段（动态反引号围栏防提前闭合）。注释明写用途：**"可作为研究日志、论文的原始验证依据。请勿手工编造数据"**
- 判断：**这是 OneCreat 目前唯一真正跑通的"过程 → 证据 → 竞赛材料"闭环**，但**只覆盖硬件验证阶段（九阶段的第六、七阶段）**，前四阶段（问题发现/分析/课题定义/调研）与后两阶段（测试迭代/成果表达）完全没有对应物。

### 1.6 前台"垂直启动台" — 成熟度：**初期（硬编码）**

`desktop/frontend/src/components/Welcome.tsx`（132 行）的 `VERTICALS` 数组，**7 张卡，全部硬编码**：

| key | 标题（zh locale） | 行为 |
|---|---|---|
| `hardware` | 硬件项目 | `opensHardware: true` → 切 `mainView="hardware"` |
| `proposal` | 技术方案 | 注入中文起手 prompt |
| `paper` | 竞赛论文 | 注入起手 prompt |
| `lesson` | 课程教案 | 注入起手 prompt |
| `tutorial` | 辅导手册 | 注入起手 prompt |
| `log` | 研究日志 | 注入起手 prompt |
| `jinpeng` | 金鹏材料 | 注入起手 prompt（"注意全程匿名化要求"） |

- 每条 prompt 后面统一追加 `OUTPUT_DIR_NOTE`：**"本任务生成的所有文档统一保存到当前项目的 `产出/` 子目录"**。这是仓库里**唯一的产出目录约定**，且只是一段 prompt 字符串，**没有任何 Go 侧强制**。
- 卡片可见性由 `useCan(v.key)` 门控 —— 这 7 个 key 与 teacher 平台 `ONECREAT_ALL_FEATURES` 的前 7 个**一一对应**。

### 1.7 「项目 / 阶段 / 课题」概念的数据结构 — **不存在**

我做了全仓搜索（`OneUp` / `oneup` / `项目包` / `P26` / `课题` / `立项`，覆盖 `.go` `.ts` `.tsx` `.md`）：

- **没有 OneUp 项目包契约**，没有 `project.json` 概念，没有 `P26X-NNN` 编号，没有"课题库/在研项目"任何痕迹。
- `oneup` 唯一命中是 OTA 默认口令字符串 `"oneup1234"`（`desktop/ota_scaffold.go:112`、`OTAPanel.tsx:60`）。
- 仓库里"项目"这个词指的**永远是 workspace 文件夹**（`hardware_service.resolveProjectDir` / `tabRuntimeService.SetWorkspace`）。
- 唯一与"项目分类"沾边的结构化字段是 `session.Record.Kind`（写一次不可改），**当前只有一个取值 `"hardware"`**，用途是历史侧栏加个 Cpu 图标。
- 「阶段」概念**仅存在于 prompt 与文档**：`toolpolicy` 的 `complete_step`、`plan_seed` 的 todo 两级、hardware MCP 的 `stage` 字段（`compile/upload/monitor/ssh/mpremote/manual`，**硬件专用**）。
- 九阶段方法论正文在**对端仓库**：`/Users/zunwei/system/teacher/docs/methodology/科创方法论-九阶段-详细版.md`（752 行，第一幕立题=问题发现/问题分析/课题定义/研究调研，第二幕实现=方案设计/组装构建/程序调试/…，每阶段有「阶段目标 / AI 使用建议 / 辅导要点 / 📝 学生记录与产出」四段固定骨架）。**OneCreat 仓库里没有它的任何副本或引用。**

> 这条是本次调研最重要的结论之一：**"OneCreat 里已经有这部分内容"其实指的是 1.3 的两段 persona + 1.6 的 7 张卡 + 1.5 的硬件证据链，仅此而已。** 方法论本体、阶段模型、产出物清单、学生记录模板，一件都还没进来。

---

## 2. 内核架构约束（迁移必须遵守的）

### 2.1 `control.Controller` = 六服务门面

`internal/control/controller.go`。Controller **自身不持任何锁**，只做转发。六个服务各自持有自己的状态**和自己的锁**：

| 服务 | 文件 | 拥有什么 |
|---|---|---|
| `approvalBroker` | `approval.go` | 审批/ask 提示、会话级授权、YOLO/bypass、刚批准计划的时间窗 |
| `checkpointService` | `checkpoints.go` | checkpoint store、单调 turn 计数、turn→消息下标边界（`cpBound`） |
| `sessionStore` | `session_store.go` | 会话目录、活动文件、每轮自动保存（**只拥有文件，不拥有消息日志**） |
| `mcpService` | `mcp.go` | plugin host、活的工具注册表、热加 MCP 的 context |
| `memoryService` | `memory.go` | 记忆快照 + 排队待上车的笔记 |
| `turnState` | `turn_state.go` | running/busy 互斥、cancel、turn 计数、**plan mode**、**coaching persona** |

**守卫**：`internal/control/facade_test.go`
- `TestControllerHoldsNoDomainLock` — AST 扫 `type Controller struct`，字段名必须在白名单里（`engine/executor/sink/label/systemPrompt/commands/skills/hooks/cleanup/autoPlan/classifier/startedOnce/balanceURL/balanceKey/balanceClient/jobs/reg/wsRoot/gateway` + 六服务 `approvals/session/memory/mcp/ckpt/turn`），且**不得出现任何 `sync.*` 类型字段**。
- `TestControllerFileStaysAFacade` — `controller.go` **≤ 950 行**。

➡️ **迁移含义**：科创工作流的领域状态（当前阶段、课题元数据、阶段产出登记）**不能挂到 Controller 上**，必须新建第七个服务并把字段名加进白名单（这是一次显式的、有审计痕迹的决定，正是守卫的用意）。

### 2.2 `engine.TurnEngine` = 唯一接缝

`internal/engine/engine.go`：

```go
type TurnEngine interface { Start(ctx, TurnRequest) (TurnHandle, error) }
type TurnHandle interface { Cancel() error; Wait(ctx) error }
```

**能力（`Capability`）**：`streaming` / `approval` / `resume` / `fork` / `hosted-tools` / `gated-tools`。**未声明即不支持**（`Supports` 对不实现 `Capable` 的引擎返回 false）。

| 引擎 | 声明 | 不声明 |
|---|---|---|
| `native`（`internal/engine/native/native.go`，包 `agent.Runner`） | streaming, approval, resume, fork, **hosted-tools** | — |
| `dsh`（`internal/engine/dsh/turn_engine.go` 的 `caps`） | streaming, approval, resume, **gated-tools** | **fork**、**hosted-tools** |

**`CapResume` vs `CapFork` 口径拆分**（HEAD 前一个提交 `4f3722a0` 刚做的）：
- `CapResume` = "引擎能把一个会话接着跑" → dsh 有（dsh 会话 id = `oc-` + Go 会话文件路径 sha1 前 24 位，跨进程 resume 实测过）
- `CapFork` = "**OneCreat 自己的消息日志就是模型可见历史的真源**" → dsh 没有（它的是投影）
- 因此在 dsh 下被拒绝的操作：`Compact`（`controller.go:414`）、`Rewind(conversation)`（`:514`）、`SummarizeFrom/UpTo`（`:570`）、`Fork`/`Branch`/`SwitchBranch`（`branches.go:195/251/309`）。**代码级 rewind（文件 checkpoint）不受影响**。
- `CapResume` 守的是 `NewSession`（`:460`）与 `ResumeSession`（`:828`）。
- 入口统一在 `internal/control/capability.go` 的 `Controller.Supports` / `requireCap`，**必须在触碰任何状态之前调用**，否则会留下"半改的影子会话 + UI 以为成功"。

**`boot.requireToolGating` fail-closed**（`internal/boot/engine.go`）：引擎必须声明 `CapHostedTools || CapGatedTools`，否则装配期返回类型化 `engine.UnsupportedError` 直接拒绝。**没有任何配置开关能绕过**（注释："一个能被一行配置关掉的安全门等于没有门"）。未知引擎名也在装配期报错，**不静默回退 native**。

**守卫**：`internal/engine/boundary_test.go`
- `TestTurnEngineStaysMinimal` — `TurnEngine` 只能有 `Start`
- `TestTurnHandleStaysMinimal` — `TurnHandle` 只能有 `Cancel`/`Wait`
- `TestEngineInterfacesRejectApplicationPolicy` — `TurnEngine`/`TurnHandle`/`Capable` 上不得出现 `forbiddenMethods`：`Approve, PendingApprovals, History, Resume, Fork, Rewind, Plan, SetPlanMode, NewSession, SessionPath, Compact, Branch, Submit, Send`
- `TestEngineBoundaryDependsOnNoPolicy` — `internal/engine/*.go` 不得 import `control / toolpolicy / permission / checkpoint / evidence / memory / billing / hook / skill / tool / plugin` **以及 `internal/agent`**
- `TestDSHAdapterIsNotASecondCore` — `internal/engine/dsh/*.go` 不得 import 上述 policy 包（不含 agent，dsh 需要 `agent.Session` 做镜像）
- `TestNativeDeclaresFullCapabilities` / `TestDSHDoesNotClaimWhatItCannotDo` / `TestUndeclaredCapabilitiesFailClosed`

➡️ **迁移含义**：科创工作流是**应用策略**，只能长在 Controller 之上，**绝不能碰引擎接缝**。任何"阶段"概念都不许出现在 `TurnEngine` 上。

### 2.3 `toolpolicy.Pipeline` 阶段顺序（已发布契约）

`internal/toolpolicy/policy.go` 的 `Pipeline.Before(ctx, Call) (context.Context, *Block)`：

```
1. plan mode 只读门     （writer 工具 + plan mode → Block）
2. Gate.Check           （权限策略；交互式审批就阻塞在这里）
3. Hooks.PreToolUse     （exit 2 = 拒绝）
4. PreEdit 检查点快照    （仅 !ReadOnly 且工具实现 Previewer；Preview 出错就跳过）
5. context 注入          （evidence.WithLedger → jobs.WithManager → memory.WithQueue）
```

**快照刻意排最后**：被拒绝的调用不该留下一个"根本没发生的改动"的回退点。
`After(ctx, call, result, err)` 做 PostToolUse hook 与记账。另有 `EndOfTurnReminder(alreadyReminded)` 做轮末 todo 对账提醒。

`agent.Agent` 只持一个 `policy` 字段，在 `t.Execute` 前后调 `Before`/`After`。
**守卫**：`internal/agent/policy_boundary_test.go`
- `TestEngineDoesNotImportProductPolicy` — `internal/agent` 不得 import `evidence/memory/diff/permission/checkpoint/hook/billing`，且**必须**仍 import `toolpolicy`（"策略接缝没了"也算失败）
- `TestExecuteOneIsThin` — `executeOne` **≤ 55 行**

➡️ **迁移含义**：如果科创工作流要加"阶段门"（例如"未完成阶段三签署不许进入阶段五"），它属于 `toolpolicy.Pipeline` 的一个新阶段，**不是 agent 循环里的 if**。这样 dsh 引擎经 `boot.dshDecider` 自动同样受管。

### 2.4 事件流：`event` / `eventwire` / `eventstream`

- `internal/event/event.go`：`Kind.Delivery()` 只有 `Reasoning` / `Text` / `ToolProgress` 三种是 `Ephemeral`，**其余一律 `Durable`（默认安全）**。新增 Kind 未分类 = 自动 durable。
- `internal/eventstream/hub.go`：两条 SSE 传输共用的唯一投递实现。落后的订阅者丢 ephemeral 帧、queue durable 帧；实在消费不动就 `stream_reset` 断开让它重同步。`Publish` **绝不阻塞**（跑在 agent run-loop goroutine 上）。
- `internal/eventwire/`：**JSON 事件契约的唯一真源** —— `KindNames` 映射 + wire 结构 + `Encode` + `Stamper`。V2 信封字段：`schemaVersion / eventId / sequence / sessionId / tabId / timestamp / durable`；`sequence` 无空洞，客户端能**检测**丢失。
  - `desktop/wire.go` 与 `internal/serve/wire.go` 都是它的薄委托。
  - 重同步三触发：`stream_reset` / 重连 / sequence 跳号 → 前端统一走 `resync()`；`GET /snapshot` 返回权威状态（transcript + running + plan mode + 待审批 + 待回答）。
- **守卫**：`internal/eventwire/wire_test.go` 的 `TestKindNamesCoversEveryDeclaredKind`（新增 Kind 不注册就红）、`TestKindWireNamesAreStable`、`TestEncodeJSONFieldContract`、`TestStamperMakesLossDetectable`、`TestEncodeLeavesTheEnvelopeEmpty`、`TestConcurrentStamping`；`desktop/wire_test.go` 的 `TestKindNamesComplete`。

➡️ **迁移含义**：科创工作流要往 UI 推"阶段推进 / 产出登记 / 证据补齐"这类事件，**必须新增 `event.Kind` 并在 `eventwire.KindNames` 注册**（否则测试红），且它们默认是 durable —— 这正是你想要的（阶段推进不能丢帧）。

### 2.5 Runtime scopes：Process → Workspace → Session → Turn

`internal/runtime/scope.go`：
- `Process.OpenWorkspace(ws)` **按 root 引用计数**返回 `*Workspace`；`Workspace.Release()` 减一
- `Workspace.NewSession(id)` → `*Session`；`Session.BeginTurn()` → `*Turn`
- 关闭一个 scope：先关子 scope，再按注册**逆序**跑自己的 `Defer`
- **取消只向下流**：取消 Turn 绝不动 Session

**资源绑定**（`internal/boot/factory.go`）：
- `Factory.OpenWorkspace` — LSP manager + CodeGraph daemon 挂 **workspace** scope（跨会话共享）
- `boot.Build` 开一个 **session** scope，MCP plugin host 挂那里
- dsh sidecar 的 `Shutdown` 也挂 session scope（`internal/boot/engine.go`）
- **不**做 workspace 级的：MCP host、jobs（会话所有）、skill/memory 索引（每次 build 快照，缓存会喂陈旧 `AGENTS.md`）、CodeGraph 二进制路径
- `Factory` **显式传递，绝不全局**（`boot.Options.Factory`；nil = 私有一份）
- **守卫**：`desktop/lifecycle_test.go` 的 `TestEveryBuildSharesTheFactory`（AST 扫每个 `boot.Build` 调用点必须传 Factory）、`TestRebuildPathsHoldTheWorkspace`（三条 rebuild 路径必须 `holdWorkspace`/`Factory.Hold` 跨越 swap）、`TestHoldWorkspaceKeepsProjectOpen`；`internal/control/turn_scope_test.go` 的 `TestTurnRootIsTheScopeNotBackground` 等 5 个。

### 2.6 `internal/session` Registry

`internal/session/session.go`：
- `Record{ID, Engine, Store, Workspace, Title, Ephemeral, Kind, CreatedAt, UpdatedAt, Display}`
- `Registry` 一个会话目录一份 `.sessions.json`（`IndexFile`）
- **`Workspace` 与 `Kind` 写一次不可改**（`SetKind` 只在空时写；`SetWorkspace` 同理）；`Title` 自由改
- `Store` 是引擎的转录引用（native 是文件路径），**registry 从不解析它**
- `Ephemeral` = 引擎把会话留在自己进程里、本地没有转录文本（dsh 相关），前端必须显示为 ephemeral 且不提供 history/resume/fork
- 并发：`Registry` 串行化 read-modify-write，但原子 rename 挡不住 lost update ⇒ **desktop 只能有一个 `session.Open`**，守卫 `desktop/session_owner_test.go` 的 `TestOnlyOneSessionRegistryIsOpened`
- 四个旧侧车 `.titles/.display/.cwds/.kinds`（`internal/session/legacy.go`）一次性导入后**故意留在磁盘上**供降级

### 2.7 `internal/account` Gateway 与 CredentialSource

- `Gateway{url, token, tier}` 带 `sync.RWMutex`；`CredentialSource` 接口 `Token(ctx) (string, error)` —— provider **每次请求都问**，所以 token 刷新能到达已在跑的会话，无需重建
- `EnvCredential{Var}`（自带 key 的普通 provider）/ `StaticCredential`
- 环境变量 `ONECREAT_GATEWAY_URL` / `_TOKEN` / `ONECREAT_TIER` **只作为传输**：`account.FromEnv()` 进程启动导入一次，`Gateway.Env()` 投影给子进程
- **守卫**：`internal/account/boundary_test.go` 的 `TestNothingElseReadsTheAccountEnvironment` —— **全树扫描非测试源码**，任何别处读写这三个变量就红。dsh 引擎因此拿 `TierFunc` / `APIKeyFunc` 闭包而不是直接读 env（`internal/boot/dshengine.go` 的 `dshTierFunc`）

### 2.8 config 解析顺序与 render 回环

- 顺序：**flag > `<workspace>/onecreat.toml` > `~/.config/onecreat/config.toml` > 内置默认**
- `[[plugins]]` 与项目根 `.mcp.json` 合并，`onecreat.toml` 同名胜出；`.mcp.json` 条目带 `PluginEntry.Source`，**不回写** TOML
- `internal/config/render.go` 渲染带注释 TOML，**必须能 round-trip 过 `Load`** —— 守卫 `TestRenderTOMLRoundTrips`（另见 `internal/config/edit_test.go` 的 `TestSaveToRoundTrips`）。**新增 config 字段 = 渲染它 + 扩展该测试**
- `.env` **绝不进程级注入**：`config.Env` 是每 workspace 的只读覆盖层（进程 env > 本 workspace `.env` > `~/.env`），每个子进程显式收 `cfg.Env().Environ()`（bash `ConfineWorkspace`、hook `Runner.SetEnv`、MCP `plugin.Spec.BaseEnv`、LSP `NewManagerWithEnv`）。守卫在 `internal/config/env_test.go`（`TestTwoWorkspacesDoNotShareTheirDotEnvKeys`、`TestDotEnvDoesNotTouchTheProcessEnvironment` 等 10 个）
- 状态根：`config.stateRoot()` = `REASONIX_CONFIG_DIR` 或 `os.UserConfigDir()/onecreat`（含从旧 `reasonix` 目录的幂等迁移）；`config.SessionDir()` = `<stateRoot>/sessions`

### 2.9 Web 模式的 RPC allowlist 与 gen-bindings

- `desktop/rpc_surface.go` 的 `rpcPublicMethods` —— **显式白名单，共 120 个方法**。加一个导出的 `*App` 方法**不会**自动暴露成 HTTP 端点
- 它同时是 **前端契约的输入**：`desktop/cmd/gen-bindings`（`//go:generate go run ./cmd/gen-bindings .`）读这份清单 + 各方法的 Go 签名与 doc comment，生成 `desktop/frontend/src/lib/bindings.generated.ts` 的 `AppBindings` 接口。**绝不手改**
- 签名限制：不许可变参数；返回值只能是 `()` / `(T)` / `(error)` / `(T, error)`
- **守卫**：`desktop/rpc_test.go` 的 `TestAppMethodsAreRPCCompatible`、`TestFrontendBindingsAreUpToDate`（逐字节比对）、`TestRPCAllowlistRejectsUnlistedExportedMethod`，另有 `TestRPCDecodesPositionalArgs`/`TestRPCReturnSignatures`/`TestRPCUnknownMethod404`/`TestRPCBadArgs400`/`TestRPCRejectsNonPost`/`TestRPCPanicBecomes500`
- **数据类型**（`frontend/src/lib/types.ts`）仍是**手工镜像**，且必须与 Go 类型同名（生成的接口按名引用）

### 2.10 `desktop.App` 门面限制

- `App` 字段白名单（`desktop/app_facade_test.go` `TestAppHoldsNoDomainState`）：`ctx, shell, tabs, factory, gateway, hw, mcp, files, memory, sessions, rt, serial`，**不得有 `sync.*` 字段**
- `TestAppFileStaysAFacade`：`app.go` **≤ 900 行**
- `TestAppMethodsAreThin`：`app.go` 里单个方法体 **≤ 26 行**
- 服务用注入函数（`root` / `ctrl` / `shell` / `ctx`）拿依赖，**绝不回指 `*App`**；测试用 `newBareApp` 而不是裸 `&App{}`
- `*App` **不得 import wails runtime** —— 一切宿主相关走 `Shell` 接口（`desktop/shell.go`；`shell_wails.go` `!web` / `shell_web.go` `web`）
- 多标签：`desktop/tabmanager.go` 每 tab 一个独立 `control.Controller` + event sink + 会话文件 + **workspace root**；`tabManager` 是唯一真源，读用 `tabs.View(id)`/`Ctrl(id)`，写用 `tabs.Update(id, fn)`，`""` = 活动 tab。build/rebuild 在 `desktop/tab_runtime.go`（`tabRuntimeService`）。**慢 rebuild 后必须按发起 id 写回**；**绝不持锁跨 `boot.Build`**（秒级）；**每次 `boot.Build` 必须传该 tab 的 `Workspace`**

### 2.11 全部守卫测试速查表

| 文件 | 守什么 |
|---|---|
| `internal/engine/boundary_test.go` | 引擎接缝最小化（方法数/禁用动词/禁用 import）、能力矩阵诚实、未声明即不支持 |
| `internal/control/facade_test.go` | Controller 无锁、字段白名单、`controller.go ≤ 950` 行 |
| `internal/control/turn_scope_test.go` | turn 根是 scope 而非 `context.Background()`、关会话取消在飞的 turn、取消 turn 不伤会话、兄弟会话隔离 |
| `internal/agent/policy_boundary_test.go` | agent 不 import 产品策略、仍 import toolpolicy、`executeOne ≤ 55` 行 |
| `internal/account/boundary_test.go` | 全树扫描：别处不得读写三个网关 env |
| `internal/cli/acp_assembly_test.go` | ACP 不自建运行时、必须调 `boot.Build`、`acp.go ≤ 160` 行、必须设 `HostProvidesCodeIntel` |
| `internal/eventwire/wire_test.go` | Kind 全覆盖、wire 名稳定、JSON 字段契约、序号可检测丢失、信封不由 Encode 写、并发盖章 |
| `desktop/app_facade_test.go` | App 无锁、字段白名单、`app.go ≤ 900` 行、方法体 ≤ 26 行 |
| `desktop/lifecycle_test.go` | 每个 `boot.Build` 共享 Factory、三条 rebuild 路径持 workspace、`holdWorkspace` nil-safe |
| `desktop/session_owner_test.go` | 只能开一个 `session.Open` |
| `desktop/rpc_test.go` | RPC 签名兼容、bindings 生成物最新、allowlist 生效、各类 HTTP 状态码 |
| `desktop/wire_test.go` | 事件 → wire 映射逐 Kind、`TestKindNamesComplete` |
| `internal/config/edit_test.go` / `render` | TOML round-trip |
| `internal/config/env_test.go` | 两 workspace 的 `.env` 不串、`.env` 不进进程环境 |
| `internal/tool/builtin/delete_symbol_test.go` | （AST 工具自身测试，非架构守卫） |

### 2.12 ✅ 「加一个新领域模块」必过清单

以"科创工作流模块"为例，从内核到 UI 一条不落：

**A. 内核 / 领域层**
1. 新建 `internal/<domain>/` 包（例：`internal/project` 或 `internal/curriculum`）。**不得**被 `internal/engine`、`internal/engine/dsh`、`internal/agent` import（否则 `boundary_test` / `policy_boundary_test` 红）。
2. 若要拦截工具调用（阶段门）：作为 `toolpolicy.Pipeline` 的新阶段，**不要**改 `agent.executeOne`（≤55 行硬顶）。改 `Pipeline` 后确认 native 与 dsh 两条路都受管（dsh 走 `boot.dshDecider`）。
3. 若要新增工具：`internal/tool/builtin/<name>.go` + `init() { tool.RegisterBuiltin(...) }`。**注意**：dsh 侧工具只读性判定在 `internal/boot/dshengine.go` 的 `dshToolReadOnly()` 有一份兜底名单，新增只读工具要同步加进去，否则在 dsh 下会被当 writer 挡在 plan mode 门外。
4. 若要新增内置技能：`internal/skill/builtins.go` 的 `builtinSkills()`。

**B. Controller 层**
5. 领域状态放**新服务**（自己持锁），不放 Controller 字段；确需 Controller 持句柄时，把字段名加进 `internal/control/facade_test.go` 的 `allowed` 白名单。
6. `controller.go` 别涨过 **950 行**。
7. 任何"会话结构性操作"必须先 `requireCap`，且在**改动任何状态之前**。

**C. 装配层**
8. `internal/boot/boot.go` 的 `Options` 加字段（如需），并保证**每个 `boot.Build` 调用点**都传（desktop 的 `TestEveryBuildSharesTheFactory` 同款要求会扩散）。
9. 资源生命周期挂对 scope：跨会话共享 → workspace scope（`Factory`）；会话独占 → session scope。

**D. 事件层**
10. 新增 `event.Kind` 常量 → **必须**在 `internal/eventwire` 的 `KindNames` 注册（`TestKindNamesCoversEveryDeclaredKind`）→ 补 `desktop/wire_test.go` 的 `TestKindNamesComplete` → 确认 Delivery 分类（默认 Durable，通常正确）。

**E. 配置层**
11. `internal/config/config.go` 加字段 → `render.go` 渲染它 → 扩展 `TestRenderTOMLRoundTrips` / `TestSaveToRoundTrips`。

**F. 会话元数据**
12. 若要给会话打领域标记：复用 `session.Record.Kind`（**写一次**）或新增字段（改 `.sessions.json` schema，注意向后兼容与 `legacy.go` 导入路径）。

**G. Desktop / Web 传输层**
13. `desktop/` 加服务文件（`<domain>_service.go`），依赖用注入函数拿。
14. `desktop/app.go` 只放**委托 + DTO 映射**（方法体 ≤ 26 行，文件 ≤ 900 行）；`App` 新字段必须加进 `app_facade_test.go` 白名单。
15. 方法要能被前端调 → 加进 `desktop/rpc_surface.go` 的 `rpcPublicMethods`。
16. 跑 `cd desktop && go generate ./...` 重生成 `frontend/src/lib/bindings.generated.ts`（**不要手改**）。
17. 手工同步 `frontend/src/lib/types.ts` 的数据类型（**名字必须与 Go 类型同名**）。
18. 更新 `bridge.ts` 的 `makeMockApp` mock 与调用点（否则 `pnpm dev` 裸浏览器路径挂）。
19. 宿主能力（对话框/打开外链/窗口）一律走 `Shell`，不许 import wails runtime。

**H. 自查三连（`docs/开发工作流.md`）**
```sh
CGO_ENABLED=0 go build ./... && go test ./...
cd desktop && go build ./... && go vet ./... && go test ./...
cd desktop && go build -tags web ./... && go vet -tags web ./... && go test -tags web ./...
cd desktop/frontend && pnpm tsc --noEmit && pnpm build
```

---

## 3. dsh 引擎现状

### 3.1 Go 侧（`internal/engine/dsh/`，共 ~3300 行含测试）

| 文件 | 行数 | 职责 |
|---|---|---|
| `sidecar.go` | 896 | `Engine` 主体：启动、握手、`Run`、通知处理、审批/预执行桥、凭证同步 |
| `mapper.go` | 284 | dsh 事件 → `event.Event` 映射（**直接丢弃 `request/header` 与 `request/context`**，这是脱敏第一道防线） |
| `protocol.go` | 204 | wire 结构体（`RawSessionEvent` / `ToolPreExecuteNotification` / `PreExecuteDecision` …） |
| `linerpc.go` | 211 | 换行分隔 JSON-RPC 2.0 over stdio |
| `runtime.go` | 142 | `resolveLaunch` / `resolveRuntimeDir` / `resolveNode` |
| `turn_engine.go` | 123 | `engine.TurnEngine` 适配 + `caps` 能力声明 |
| `scrub.go` | 62 | `Scrubber` 兜底脱敏（第二道防线） |
| `tailbuffer.go` | 34 | sidecar stderr 尾缓冲 |
| 测试 | ~1100 | `e2e_gate_test.go`（372 行，门禁 5 条 e2e）、`e2e_gateway_test.go`、`e2e_reasoning_test.go`、`gateway_test.go`、`bridge_test.go`、`mapper_test.go`、`linerpc_test.go`、`recorder_test.go`、`scrub_test.go` |

**sidecar 启动**（`runtime.go`）：
- runtime dir 解析顺序：`[dsh].runtime_dir` → 主程序旁 `runtime/dsh`（发行包）/ `dsh` / `../dsh` → 从 cwd 逐级向上找 `dsh/`
- node 解析：`[dsh].bin_path` → 发行包内置 `<runtime>/../node/bin/node`（Windows `node.exe`）→ PATH 里的 `node`（要 Node 20+）
- 入口：`node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/bin.js <profile>`，默认 profile `profiles/onecreat.cordis.yml`
- 握手超时 `defaultStartupTimeout = 60s`（可用 `[dsh].startup_timeout_sec` 覆盖）
- 调试：`ONECREAT_DSH_DEBUG` 非空 → sidecar stderr 直透本进程 stderr

**门禁 `tools/pre-execute` → `dshDecider`**：
```
dsh-tools 调度器（派发前）
  → tools/pre-execute waterfall
  → dsh/plugins/control（OneCreat 自有控制面插件）await Go 侧裁决
  → Engine.handlePreExecute（sidecar.go:814）
      decide(name, args)          ← boot.dshDecider(pipeline, registry)
        └─ toolpolicy.Pipeline.Before(...)   ← 与 native 完全同一条流水线
      decision == ask 且 approver != nil → 阻塞等用户；approver == nil（headless）→ 放行
      decision == allow 且 preEdit != nil → 文件快照
  → notify(NotifyToolPreExecuteDone, {ID, Decision, Reason})
  → deny 时 sidecar 不执行该工具
```
`dshToolReadOnly()`（`boot/dshengine.go`）给 dsh 内建工具兜底只读判定：`read / grep / glob / ls / todo_write / complete_step / exit_plan_mode`；能在 Go 注册表查到同名工具时以注册表为准。

**事件映射与脱敏（两道防线）**：
1. `mapper.go` 直接丢弃 `request/header`、`request/context`（真实 model/provider 名的来源）
2. `Scrubber`（`scrub.go`）对 `Text / Reasoning / Tool.Args / Tool.Output / Tool.Err` 做子串替换。敏感串由 `boot.dshBrandSecrets(gatewayURL)` 运行时注入：`deepseek-official, llm-deepseek, DeepSeek, deepseek, api.deepseek.com, DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL` + 网关 URL。**直连模式不擦**（用户用的就是自己的 key）
3. wire model：`Engine.wireModel()` —— 网关模式下发**当前档位**（`tier-N`，经 `TierFunc` 从 `*account.Gateway` 取），退化到 `[dsh].model_placeholder`（默认 `"onecreat"`）；直连模式下发 `[dsh].direct_model`（默认 `deepseek-v4-flash`）

**凭证刷新**：`APIKeyFunc` 闭包（不是快照）。平台 token 约 50 分钟被后台刷新一次，而子进程环境是 spawn 时的死快照 —— 引擎每轮重取当前值，变了就经 `onecreat/credentials.set` 补发（`syncCredentials`，`sidecar.go:388`）。

**resume**：dsh 会话 id = `oc-` + Go 会话文件路径 sha1 前 24 位（`sessionIDFor`，`sidecar.go:227`）；`BindSession(path)` + `loadIfNeeded` 走 `onecreat/session.load` 从 dsh 自己的 store 恢复。dsh store 落在 `<config.SessionDir()>/dsh/<工作区>/<会话id>/`（JSONL，默认 zstd）。

**fork 不支持的原因**：模型可见历史的真源在 dsh 侧，Go 侧 `agent.Session` 只是**每轮结束追加 user/assistant 文本的只读投影**，Go 侧**从不写回 dsh**。改投影不改真源 = 制造假象。因此 dsh 下 `Compact / Rewind(conversation) / Fork / Branch / SwitchBranch / Summarize` 一律报"暂不支持"；**文件级 rewind 照常可用**。

**Cancel 降级**：dsh 目前没有 mid-turn cancel RPC，`Engine.Cancel` 降级为关掉 sidecar 进程（`turn_engine.go` 与 `engine.go` 的 `TurnHandle.Cancel` 注释均写明"允许尽力而为"）。

### 3.2 TS 侧（`dsh/`，版本 rc.8）

```
dsh/
  package.json          # name: onecreat-dsh-runtime, private, type: module
  pnpm-lock.yaml
  tsconfig.json
  profiles/onecreat.cordis.yml    # cordis profile（唯一入口配置）
  plugins/control/index.js        # OneCreat 自有控制面插件（把 pre-execute await 到 Go）
  plugins/gateway/index.js        # OneCreat 自有网关适配器（隐藏 provider 品牌名）
  node_modules/@deepseek-ai/…     # 22 个 dsh 包 + cordis + schemastery
```

**依赖闭包（全部锁死 `0.1.0-rc.8`）**：`dsh-agent, dsh-agent-spine-demo, dsh-bash-local, dsh-compaction-basic, dsh-fs-local, dsh-fs-observation-policy, dsh-llm, dsh-llm-deepseek, dsh-mcp-client, dsh-plan-mode, dsh-sdk-jsonrpc-demo, dsh-sdk-jsonrpc-server, dsh-sdk-protocol, dsh-session, dsh-session-checkpoint-policy, dsh-session-persistence-jsonl, dsh-subprocess-local, dsh-token-meter, dsh-tool-fs, dsh-tool-todo, dsh-tools, dsh-user-approval` + `@deepseek-ai/cordis@4.0.1` + `@deepseek-ai/schemastery@3.18.1`；devDeps `typescript@5.7.3`、`@types/node@22.13.1`。
`pnpm.onlyBuiltDependencies`：`dsh-subprocess-local, koffi, node-pty` —— **原生模块，只能在目标平台上装**。

**发行包平台矩阵**（`docs/Web模式.md`）：`release-web.yml` 分两阶段（sidecar 矩阵 macos-14 / macos-13 / windows-latest / ubuntu-22.04 各自跑 `scripts/dsh-bundle.sh` 出 `runtime/`，release job 用 `DSH_RUNTIME_DIR` 喂 `scripts/web-build.sh`）。
- ✅ 带 dsh：darwin/arm64、darwin/amd64、windows/amd64、linux/amd64
- ❌ 不带：linux/arm64（无 arm64 Linux runner），只能 `engine=native`

### 3.3 Web 模式下怎么选引擎

- 解析优先级（`internal/boot/dshengine.go` 的 `engineName`）：**`boot.Options.Engine`（显式） > `ONECREAT_ENGINE` 环境变量 > `cfg.Engine`（`onecreat.toml` 的 `engine = "..."`）**；空 = `native`
- **未知名字原样返回**，由 `selectEngine` 在装配期报错（不静默回退）
- Web 模式的 `main_web.go` **没有 `--engine` flag**（只有 `--port` / `--host` / `--no-open` / `--workspace`）⇒ Web 下切引擎只能靠 `ONECREAT_ENGINE=dsh` 或改 `onecreat.toml`
- 前端展示：`desktop/app.go` 的 `engineLabel(ctrl)` —— controller 建好后读 `ctrl.EngineName()`，未建好时看 `ONECREAT_ENGINE` 推断；经 `Meta.Engine` 到 UI

---

## 4. 账号 / 商业化现状

### 4.1 客户端侧

| 组件 | 位置 | 事实 |
|---|---|---|
| 账号对象 | `internal/account/account.go` | `Gateway{url, token, tier}` + `CredentialSource`。desktop 一份对象跨所有 tab 共享 |
| 登录/登出/切档 | `desktop/accounts_app.go` | `AccountLogin` / `AccountLogout` / `AccountSessionInfo` / `SetOnecreatTier(index)` / `RefreshAccountSession` |
| 模式开关 | `platformAccountEnabled()` | env `ONECREAT_ACCOUNT_MODE`（`platform`/`teacher`/`gateway` 均算开）> ldflags `-X main.defaultAccountMode=platform` |
| 平台地址 | `platformBaseURL()` | env `ONECREAT_PLATFORM_URL` > `https://t.weizunxy.com` |
| 会话落盘 | `accountSessionPath()` | `os.UserConfigDir()/onecreat/session.json`（0600），`persistedSession` 含 token/refreshToken/expiresAt/tiers/points/selectedTier |
| 并发保护 | `sessionFileMu` | 切档与轮末 refresh 会并发读改写同一文件；无锁会把旧档位回写覆盖新档（历史 bug H3） |
| 网关兜底 | `gatewayActive()` | 已登录时后端拦 `SaveProvider` / `SetModel` / `SetProviderKey` 等，返回 `errGatewayManaged`（防 devtools 绕过） |
| 模型隐私 | `config.ModelPrivacyPolicy` | 由 `boot.Build` 无条件追加到系统提示，即使用户自定义 system_prompt |
| 点数/余额 | `internal/billing` | ⚠️ **只是 DeepSeek `GET /user/balance` 的钱包读数**（`Balance` / `Info` / `Fetch`），**不是**机构点数。机构点数是 `AccountSession.Points *float64`，由登录/刷新接口带回，客户端**只读展示** |
| 门控 | `desktop/frontend/src/lib/account.ts` 的 `useCan(key)` | 本地模式默认全开；平台模式按 `features[]` |
| 登录门 | `App.tsx` + `LoginGate.tsx`（63 行） | `session.platformMode && !session.loggedIn` → `<LoginGate>`。**Web 模式下同样生效**（`docs/Web模式.md` 实测确认） |

### 4.2 平台侧（`/Users/zunwei/system/teacher`，只读确认）

- 端点（4 个）：`POST /api/onecreat/login`、`GET /api/onecreat/session`、`POST /api/onecreat/v1/chat/completions`、~~`POST /api/onecreat/refresh`~~（**未实现**，客户端防御式跳过，平台靠 JWT 拉到 1 周兜底）
- 表 / 列：`organizations.onecreat_features TEXT[]`、`organizations.onecreat_points NUMERIC`、流水表 `onecreat_point_ledger`（RPC `consume/allocate/reserve/settle_onecreat_points`）、`onecreat_tiers(tier_index 1–3 → ai_models.id)`
- 功能 key（`src/lib/onecreat/catalog.ts` 的 `ONECREAT_ALL_FEATURES`，**共 10 个**）：
  `hardware / proposal / paper / lesson / tutorial / log / jinpeng / knowledge / ota / skills`
- 点数换算：`src/lib/onecreat/points.ts`，**中国区官方人民币价 × `ONECREAT_POINTS_PER_YUAN = 100`**（⚠️ 与 onecreat 侧文档写的"USD×1000"口径已不一致，以平台侧为准）
- 档位默认映射：档1 标准→`deepseek-v4-flash`；档2/3 高级/旗舰→`deepseek-v4-pro`
- 超管后台：机构详情页勾功能/充点、`/admin/teachers` 建账号、`/admin/onecreat-tiers` 配档
- **权限粒度就到"机构（organization）"为止**：`resolveOnecreatSession()` 读老师 `active_organization_id` 所属机构的 `onecreat_features`；超管 `role='admin'` 全开

### 4.3 判断：把「科创工作流」做成付费功能并按机构/老师/学生分权，现有账号模型缺什么

**能直接复用的**：
- 加一个 feature key（如 `workflow` / `methodology`）是**现成路径**：平台 `catalog.ts` 加一条 + 超管勾选 UI 自动带上；客户端 `useCan("workflow")` 门控入口 + mock 全功能清单。零架构改动。
- 按档位差异化（例如"旗舰档才有九阶段自动编排"）也现成：客户端读 `AccountSession.SelectedTier`。
- 计量已经在**每次 AI 调用**这一层做了（点数），工作流跑得多就自然扣得多。

**缺什么（按缺口严重度排）**：

1. **没有"学生"这个主体。** 整个账号模型只有两个角色：超管 与 老师（`teacher_type='platform'`）。`AccountSession` 只有 `{Account, IsAdmin, Permissions, Tiers, Points, SelectedTier, PlatformMode}` —— **没有 userId、没有 orgId、没有 role 枚举、没有名下学生列表**。科创工作流的核心对象是「一名学生的一个课题」，现在客户端连"这台机器上正在辅导谁"都表达不了（现有做法是 `App.tsx:322` 的 **localStorage 文件夹备注名（学生名）**，纯本地、不同步、不可审计）。

2. **权限粒度只到机构，不到人。** `onecreat_features` 挂在 `organizations` 上，同机构所有老师权限完全一致。要做"张老师能看自己的 12 个课题、看不到李老师的"，需要新增**老师级 / 课题级**的授权模型（平台侧新表 + 端点，客户端侧新的 session 字段）。

3. **没有任何服务端的课题/项目实体。** 课题目前只是**本机一个文件夹**。跨设备、跨老师协作、机构看板、"这个项目做到第几阶段"全都无处安放。要做成付费功能且可运营（续费看得见价值），至少需要平台侧一张 `onecreat_projects` 表（id / org_id / owner_teacher_id / student_alias / stage / created_at）+ 客户端把本地 workspace 与它绑定。

4. **计费维度单一。** 只有"AI 调用扣点"一种。工作流类功能的价值在**产出物**（技术方案、论文、答辩包），而不在 token；如果要按"每个课题包 X 点"或"每套竞赛材料 Y 点"计费，平台侧 ledger 需要新的 `reason` 类型 + 客户端需要在产出完成时上报一次结算事件（**现在客户端根本不上报任何业务事件，只读点数**）。

5. **登录续期仍是缺口。** `/api/onecreat/refresh` 至今未实现。科创工作流会话可能跨小时甚至跨天，1 周 JWT 缓解但没根治。

6. **离线/断网。** 老师在教室常无稳定网络。平台模式**强制登录**且每轮刷新 session，一断网整个工作流不可用。方法论内容如果全放服务端会加剧这个问题 —— 建议**方法论正文随发行包内置（skill/KB），只有身份与计量走网**。

---

## 5. 前端现状

### 5.1 结构

- 入口：`desktop/frontend/src/App.tsx`（**1894 行**，单体）
- 42 个组件（`src/components/`），最大几个：`HardwarePanel.tsx` 1459 · `Composer.tsx` 1071 · `SettingsPanel.tsx` 888 · `CapabilitiesPanel.tsx` 771 · `WorkspacePanel.tsx` 682 · `MemoryPanel.tsx` 574 · `KnowledgePanel.tsx` 344
- 共 11758 行 TSX

### 5.2 视图与面板模型（**不是"顶栏几个 tab"，而是一个主视图 + 一堆抽屉**）

- **主视图**：`mainView: "chat" | "hardware"`（`App.tsx:507`）。顶部原来的"对话 | 硬件编程"双 tab **已删**（蓝图 Phase 1）。硬件入口降级为侧栏按钮（`sidebar__hardware`，`App.tsx:1359`）/ 首页硬件卡 / 对话内按钮。
- **抽屉 / 覆盖层**（各一个 boolean state）：`workspacePanelOpen`（工作区文件树，可 resize/最大化/预览）、`settingsOpen`、`capsOpen`（MCP 与技能）、`knowledgeOpen`（知识库）、`helpOpen`，另有 `MemoryPanel` / `HistoryPanel` 以 props 控制。
- **左侧栏**：会话列表，**按文件夹（workspace）分组**，支持 备注名（学生名）/ 置顶 / 在 Finder 打开 / 删除该文件夹会话（`App.tsx:1134`）。备注名与置顶用 **localStorage**（键=文件夹绝对路径，`lib/fileRemarks.ts` / `App.tsx:322`）。
- **空状态首页**：`Welcome.tsx` = 7 张垂直启动卡（见 1.6）。
- **对话区**：`Transcript.tsx` + `Message.tsx` + `ToolCard.tsx`；`Composer.tsx` 含 persona 选择器 / 计划模式 / YOLO / 模型 / effort / @文件引用 / 粘贴附件。
- **辅助条**：`TaskContextBar.tsx`（当前 todo + 相关文件）、`TodoPanel.tsx`、`SessionArtifacts.tsx`（右下角"本次产出"，聚合本会话 `write_file`/`edit_file`/`multi_edit` 成功写过的文件，点开在工作区面板打开 —— 注释明写为竞赛论文/教案场景做的）、`StatusBar.tsx`、`UpdateBanner.tsx`、`WebQuitButton.tsx`。
- **详细度开关**：`lib/detailMode.ts` —— 简洁（默认，给学生/老师看人话，隐藏 reasoning）vs 详细（给老师/高年级看原始工具名+完整命令）。这是**另一处真正的教培设计**。

### 5.3 状态管理

- **无 Redux / Zustand / Jotai**。核心是 `lib/useController.ts` 的 `useReducer`：把扁平 `WireEvent` 流归约成结构化 transcript。
- 关键性能设计：`LiveStream = {id, text, reasoning}` 把在飞的流式片段**放在 `items` 之外**，O(1) 更新不重建 backlog。⚠️ **红线：不要从 reasonix 上游搬流式优化**（合帧 / `useDeferredValue` 那批实测更差且多 bug，已全撤回 —— `docs/开发工作流.md`、记忆 `onecreat-streaming-perf-dont-port`）。
- 账号态：`lib/account.ts`（session store + `useCan`）。
- i18n：`lib/i18n.tsx` + `locales/zh.ts` / `en.ts`（`DictKey` 类型驱动）。
- 其它零散 store：`layoutPreferences.ts`、`theme.ts`、`detailMode.ts`、`fileRemarks.ts`、`session.ts`（均 localStorage）。

### 5.4 bridge.ts / bindings 机制（三级解析）

```
window.go.main.App 存在        → Wails 绑定（桌面版）
else window.__ONECREAT_WEB__   → Proxy → POST /rpc/<Method>（位置参数 JSON 数组）+ 一条 /events SSE
else                           → makeMockApp（pnpm dev 裸浏览器）
```
三条路共用 `AppBindings` 接口（`lib/bindings.generated.ts`，**由 `desktop/cmd/gen-bindings` 生成，禁止手改**）。改 Go 签名 ⇒ `go generate` ⇒ 更新 `makeMockApp` + 调用点。数据类型 `lib/types.ts` 手工镜像且必须同名。

### 5.5 有没有「项目 / 课题 / 看板」类 UI？

**没有。** 最接近的三处：
1. 左侧栏按 workspace 文件夹分组 + localStorage 备注名（学生名）—— 但这只是文件夹列表，不是项目实体。
2. `SessionArtifacts`「本次产出」—— 单会话粒度，不跨会话汇总。
3. `HardwarePanel` 的证据卡 + 「导出证据」按钮 —— 硬件专用。

`WorkflowSurface.tsx` **不存在**（蓝图 Phase 2 ❌ 未开始，`HardwarePanel.tsx` 仍是 1459 行单体）。

### 5.6 新增一个「科创工作台」级别的大 UI 模块应该挂在哪

**建议挂点（按侵入度从低到高）**：

- **方案 A（最低风险，推荐起步）**：**新增一个 `mainView` 取值** `"workflow"`，与 `"chat"` / `"hardware"` 并列。侧栏加一个入口按钮，`Welcome.tsx` 加一张卡。这与硬件面板走的是同一条已验证的路径。
  - 改点：`App.tsx`（`mainView` 联合类型 + 渲染分支 + 侧栏按钮 + `layout--workflow` class）、新组件 `components/WorkflowPanel.tsx`、`styles.css`、`locales/zh.ts` + `en.ts`（`DictKey` 会强制两边同步）。
- **方案 B（若要"随时可查、不占主视图"）**：做成右侧抽屉（复用 `ResizableDrawer.tsx`），与 `WorkspacePanel` 并列。
- **⚠️ 蓝图铁律**：**"绝不一个垂直一个定制面板"**（`docs/平台重构蓝图.md` 第二条设计铁律）。九阶段工作流如果只是"点卡进对话跑 skill"，就**不该**做定制面板；只有当它真的需要"阶段推进按钮 / 产出登记表 / 证据补齐清单"这类**非对话交互**时，定制面板才成立（硬件面板就是因为要"编译/烧录/串口"按钮才成立的）。做之前先确认这一点。

**要过的生成 / 测试**（在 2.12 清单的 G 段基础上，前端特有）：
1. 新 Go 方法 → `rpc_surface.go` → `go generate ./...` → `bindings.generated.ts` 更新（`TestFrontendBindingsAreUpToDate` 逐字节比对）
2. `types.ts` 手工加同名类型
3. `bridge.ts` 的 `makeMockApp` 加 mock（否则裸浏览器开发路径 crash）
4. 新事件 Kind → `eventwire.KindNames` + `desktop/wire_test.go`
5. `locales/en.ts` 的 `DictKey` 是类型真源，zh 缺 key 会 `tsc` 报错
6. `pnpm tsc --noEmit && pnpm build`
7. Web 标签也要绿：`cd desktop && go test -tags web ./...`

---

## 6. 存储与工作区

### 6.1 各类数据存哪

| 数据 | 路径 | 归属 |
|---|---|---|
| 全局配置 | `<stateRoot>/config.toml`，`stateRoot` = `$REASONIX_CONFIG_DIR` 或 `os.UserConfigDir()/onecreat` | 进程级 |
| 项目配置 | `<workspace>/onecreat.toml` | workspace |
| MCP 项目配置 | `<workspace>/.mcp.json` | workspace（不回写 TOML） |
| 会话转录（native） | `<stateRoot>/sessions/*.json`（`config.SessionDir()`） | 全局目录，按文件区分 |
| 会话索引 | `<stateRoot>/sessions/.sessions.json`（`session.IndexFile`） | 全局 |
| 旧侧车 | `<stateRoot>/sessions/.titles.json` / `.display.json` / `.cwds.json` / `.kinds.json` | 一次导入后保留供降级 |
| 会话转录（dsh） | `<stateRoot>/sessions/dsh/<工作区>/<会话id>/`（JSONL，默认 zstd） | dsh 自己的 store（**模型可见历史真源**） |
| 知识库 | `os.UserConfigDir()/onecreat/knowledge/`（store JSON + 复制的原文件） | **全局，不分 workspace** |
| 记忆 | `<workspace>/ONECREAT.md`（或 `REASONIX.md`/`AGENTS.md`/`CLAUDE.md`）、祖先目录同名文件、`<userConfigDir>/ONECREAT.md` | workspace + 祖先 + user |
| 硬件证据链 | `<workspace>/tests/hardware_evidence.jsonl` + `tests/hardware_checklist.md` | **workspace（项目目录内）** |
| 硬件清单/接线 | `<workspace>/hardware_manifest.json`、`docs/wiring.md`、`docs/verification.md`、`docs/board_profile.md` | workspace |
| 内容类产出 | `<workspace>/产出/`（**仅 prompt 约定，无强制**） | workspace |
| 检查点（代码 rewind） | `internal/checkpoint` 的 store（会话级，写前快照） | session |
| 平台账号 | `os.UserConfigDir()/onecreat/session.json`（0600） | 机器级单例 |
| Web 单实例锁 | `<stateRoot>/web.lock`（0600，`{pid, port, token, startedAt}`） | 进程 |
| 技能 | `<workspace>/{.reasonix,.agents,.agent,.claude}/skills/` + 自配路径 + `~/{...}/skills/` | project/custom/global |
| 前端偏好（备注名/置顶/布局/主题/详细度/KB 选择） | 浏览器 localStorage | 机器级、不同步 |
| CodeGraph 索引 | `<workspace>/.codegraph/` | workspace |

### 6.2 `workspace.Context` 语义

`internal/workspace/workspace.go`：
- 不可变、**始终绝对**、只有一个字段 `root`（未导出，防被重新赋值或设成相对路径）
- 零值 = "无显式 workspace" = 进程 cwd 语义（`Root()` 返回 `""`，`Resolve` 原样返回）
- `New(root)` 校验存在且是目录；`Current()` 用进程 cwd
- `Resolve(p)`：绝对 p 原样返回（**写入约束由 confinement 负责，不由这里**）；`""`/`"."` → root
- `RootOr(fallback)` 让"需要具体目录"的调用点显式写出 fallback，而不是隐式继承 cwd
- **运行期切 workspace 绝不许 `os.Chdir`**；只有**启动时**可以（`desktop.resolveStartupWorkspace`、web 的 `--workspace` flag）

### 6.3 多 workspace 并行

- desktop 每 tab 一个 workspace（`tabRuntimeService.SetWorkspace` / `SwitchWorkspace`）
- `boot.Factory.OpenWorkspace(ws)` **按 root 引用计数**共享 LSP manager + CodeGraph daemon；关一个 tab 只停该 tab 的 MCP 子进程，不动兄弟 tab 正在用的项目服务
- `config.Env` 是**每 workspace 的只读 `.env` 覆盖层**，两个 workspace 的 key 不串（`TestTwoWorkspacesDoNotShareTheirDotEnvKeys`）

### 6.4 如果引入「项目（P26X-NNN）」作为一等公民 —— 我的建议

**核心判断：项目 ≠ workspace，项目 ⊃ 会话；项目应当是 workspace 之上的一层"身份 + 元数据"，而不是替代 workspace。**

理由与摆法：

**(1) 项目应当 1:1 绑定一个 workspace 目录，但身份不由目录路径承载。**
现状 `session.Record.Workspace` 已经是"这个会话属于哪个项目根"，且**写一次不可改**（注释理由："哪个项目 ≠ 用户后来切了文件夹"）。这个语义正好。缺的只是：目录被改名/搬家后身份就丢了。
➡️ **建议**：在项目根放一个 `.onecreat/project.json`（或复用 `ONECREAT.md` frontmatter），存 `{id: "P26C-020", shortName, stage, assignee, createdAt}`。**id 是身份，目录路径只是当前位置**。这与用户在 NAS 侧已有的 `project.json` 约定同构，迁移成本最低。

**(2) 在 `session.Record` 上加一个 `ProjectID` 字段（write-once，与 `Workspace`/`Kind` 同款语义）。**
这样历史侧栏可以按项目而不是按文件夹路径分组，目录搬家后历史不断。改动很小：`internal/session/session.go` 加字段 + `Registry.ensure` 时从 workspace 根读 `project.json` 填入。**注意**：`.sessions.json` 是 JSON，加字段天然向后兼容。

**(3) 项目状态（当前阶段、阶段产出登记、证据完成度）放**项目目录内**，不放全局 store。**
理由：
- 与现有硬件证据链（`tests/hardware_evidence.jsonl` 落在项目内）一致，**证据必须和它证明的东西住在一起**，才能随项目一起备份/交付/归档；
- 全局 store 会在多机（老师办公室 + 教室）之间打架，而项目目录本来就在 NAS/网盘上同步；
- 不需要引入新的全局并发问题（`.sessions.json` 的 lost-update 教训）。
➡️ 建议落 `<workspace>/.onecreat/workflow.jsonl`（append-only 事件流：阶段进入/退出、产出登记、成功标准签署）+ 一份可读的 `<workspace>/产出/研究日志.md` 由它渲染。**append-only 比可变 JSON 更适合"证据链"这个语义**，也和 `hardware_evidence.jsonl` 同构。

**(4) 项目 ≠ session：一个项目跨几十次会话、几个月。**
所以"当前阶段"**不能**放 `turnState`（那是每轮的运行态），也**不该**放 Controller（会话级）。它属于 **workspace 作用域**。
➡️ 建议：新建 `internal/project` 包，其服务由 `boot.Factory.OpenWorkspace` 创建并挂 **workspace scope**（与 LSP/CodeGraph 同级），这样同一项目的多个 tab 共享同一份阶段状态，refcount 到 0 才释放。这是现有架构**已经准备好、但还没有人用**的扩展点。

**(5) 平台侧（teacher）只存"项目影子"用于运营看板与计费，不存内容。**
`onecreat_projects(id, org_id, owner_teacher_id, student_alias, stage, updated_at)` —— 客户端在阶段推进时上报一次。这样机构看板/续费价值可见，但**离线仍能干活**（内容与证据都在本地项目目录）。

**(6) 不要做的事**：
- ❌ 不要把项目做成"全局 store 里的一条记录 + 指向某个目录的指针" —— 目录一搬就烂，且与已有的 workspace 引用计数模型打架。
- ❌ 不要让项目取代 workspace 成为 `boot.Options` 的输入 —— `workspace.Context` 的不可变+绝对路径语义是一堆 confinement/env/skill 发现的基础，动它成本极高。
- ❌ 不要把"阶段"塞进 `session.Record.Kind`（write-once，且它的语义是"用过哪个垂直界面"，不是"进行到哪一步"）。

---

## 7. 附录

### 7.1 读过的关键文件（绝对路径）

**文档**
- `/Users/localwork/06_System/onecreat/CLAUDE.md`
- `/Users/localwork/06_System/onecreat/docs/开发工作流.md`
- `/Users/localwork/06_System/onecreat/docs/Web模式.md`
- `/Users/localwork/06_System/onecreat/docs/账号系统与教学平台互通.md`
- `/Users/localwork/06_System/onecreat/docs/平台重构蓝图.md`
- `/Users/localwork/06_System/onecreat/docs/dsh调研/00_G0结论.md`（其余 01–08 只看了标题与行数）
- `/Users/zunwei/system/teacher/docs/onecreat-integration.md`
- `/Users/zunwei/system/teacher/src/lib/onecreat/catalog.ts`、`tiers.ts`
- `/Users/zunwei/system/teacher/docs/methodology/科创方法论-九阶段-详细版.md`（**仅读标题结构，未读正文**）

**内核**
- `internal/evidence/evidence.go`
- `internal/tool/builtin/completestep.go`
- `internal/skill/skill.go`、`builtins.go`、`index.go`
- `internal/engine/engine.go`、`boundary_test.go`
- `internal/engine/dsh/turn_engine.go`、`sidecar.go`（片段）、`runtime.go`、`scrub.go`
- `internal/engine/native/`（仅经 boundary_test 确认能力）
- `internal/boot/engine.go`、`dshengine.go`、`factory.go`（函数清单）、`boot.go`（Options）
- `internal/toolpolicy/policy.go`
- `internal/control/controller.go`（struct + capability 调用点）、`capability.go`、`plan_seed.go`、`auto_plan.go`、`facade_test.go`
- `internal/agent/policy_boundary_test.go`
- `internal/account/account.go`、`boundary_test.go`
- `internal/billing/balance.go`
- `internal/session/session.go`、`legacy.go`
- `internal/workspace/workspace.go`
- `internal/runtime/scope.go`（API 清单）
- `internal/event/event.go`（Delivery 段）
- `internal/config/config.go`（Config / DSHConfig / DefaultSystemPrompt / ModelPrivacyPolicy / stateRoot / SessionDir）
- `internal/memory/doc.go`（docNames）
- `cmd/reasonix-hardware-mcp/main.go`（工具清单与证据段）
- `internal/hardware/boards/`

**Desktop / 前端**
- `desktop/rpc_surface.go`、`app.go`（片段）、`app_facade_test.go`、`rpc_test.go`、`lifecycle_test.go`、`session_owner_test.go`
- `desktop/accounts_app.go`、`knowledge_app.go`、`skills_app.go`、`hardware_service.go`、`memory_service.go`、`tabmanager.go`、`tab_runtime.go`
- `desktop/frontend/src/App.tsx`（结构与 persona 段）
- `desktop/frontend/src/components/Welcome.tsx`、`TaskContextBar.tsx`、`SessionArtifacts.tsx`
- `desktop/frontend/src/lib/useController.ts`、`bridge.ts`
- `dsh/package.json`、`dsh/` 目录树

### 7.2 不确定 / 未验证之处（诚实标注）

1. **未运行任何测试或构建** —— 本次全程只读，所有"守卫会红"的断言来自阅读测试源码，未实际触发。
2. **`internal/skill/tools.go`（363 行）只看了函数签名**，`run_skill` 的参数展开与 subagent 隔离细节未逐行读。
3. **`internal/engine/dsh/mapper.go` / `protocol.go` 未逐行读**，"直接丢弃 `request/header` 与 `request/context`"来自 `scrub.go` 的注释交叉引用与 `docs/dsh调研/00` 的漏点表，未在 mapper 源码中亲眼核对该分支。
4. **`dsh/plugins/control/index.js` 与 `gateway/index.js` 未读**（JS）。控制面插件的 await 语义来自 Go 侧注释与 `docs/dsh调研/07`。
5. **`docs/dsh调研/01–08` 除 00 外只看了文件名与行数**，未读正文；07（498 行，门禁缺口解决报告）与 08（475 行，路线 A 合并方案）里可能有本报告未覆盖的口径细节。
6. **teacher 平台只做了"瞄一眼"**：读了 `catalog.ts`、`tiers.ts` 头部、`docs/onecreat-integration.md`。未看 `points.ts` 正文、未看任何 migration SQL、未看网关 route 实现。"点数换算 = 人民币 × 100"来自 `onecreat-integration.md` 的陈述（且它明确说与 onecreat 侧文档写的 USD×1000 不一致，我采信平台侧文档但**未看源码确认**）。
7. **`~/.claude/skills/` 下那批科创技能未逐个核对**（它们不在本仓库，由用户全局配置提供）。第 1.2 节列出的技能名来自本次会话的可用技能清单，**不是从磁盘扫描得来的**；实际 OneCreat 运行时能发现哪些，取决于 `config.ConventionDirs` 的实际命中情况，建议真机跑 `/skill paths` 确认。
8. **`internal/serve`（老的 HTTP+SSE 服务）只列了文件名**。`docs/Web模式.md` 说它与 Web 模式功能重叠、"长期应二选一"，但本次未评估它对迁移的影响。
9. **`internal/checkpoint` 未读源码**，只从 CLAUDE.md 与 `toolpolicy` 的 `PreEdit` 接入点推断。
10. **前端 `styles.css` 未读**，`layout--hardware` 等 class 的样式约定未核实。
