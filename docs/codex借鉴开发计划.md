# Codex 借鉴开发计划(onecreat)

> 写给执行者(AI)的开发文档。参考库:`/Users/zunwei/Desktop/codex`(OpenAI Codex CLI,Apache-2.0,Rust)。
> 完整盘点结论见会话记录;本文只保留**决定要做的**和**明确暂缓的**。

## 硬约束(执行时必须遵守)

1. **不开发任何与网络有关的功能**(模型安全限制)。凡涉及对外服务、网络监听、远程调用、对外发布的项,一律归入「暂缓」区,由用户后续用其他方式或其他模型完成。本计划内的开发只允许:读写本地文件、本地进程、既有的本地 MCP 子进程通信。
2. 不换底座:onecreat 继续 Go + Wails,Codex 只作参考答案,逐项移植思想而非代码。
3. 外科手术原则:每项独立可验证、可提交;不顺手重构无关代码。
4. 每项完成标准:`go test ./...`(根模块 + desktop)全绿 + `tsc --noEmit` 干净;有新逻辑必有钉死行为的测试。
5. 产品哲学:零配置、中文优先、对弱模型(deepseek-v4-flash)友好。任何引入"档位/旋钮"的设计先问是否违背零配置。

---

## 范围内(纯本地,按优先级)

### 1. edit_file 模糊匹配容错(治弱模型改文件失败)✅ 已完成

**问题**:`internal/tool/builtin/editfile.go` 的 `edit_file` 要求 `old_string` 与文件内容**逐字节精确匹配且唯一**。flash 这类弱模型复述代码时常带行尾空格差异、缩进 tab/空格差异,导致 `old_string not found`,改文件反复失败。

**参考**:Codex `codex-rs/apply-patch/src/lib.rs` 的 seek_sequence:精确匹配失败后,降级做「忽略行尾空白」匹配,再降级「忽略首尾空白」匹配;每级都要求唯一;结果里如实标注用了哪级模糊。

**设计**(不引入新工具、不教模型新格式,保持 edit_file 接口不变):
- 在 editfile.go 抽一个 `findUniqueMatch(content, old string) (start, end int, note string, err error)`:
  - 第 0 级:现有 `strings.Count` 精确匹配(行为不变);
  - 第 1 级:按行滑窗,比较时 `strings.TrimRight(line, " \t")`(忽略行尾空白);唯一才命中;
  - 第 2 级:按行滑窗,比较时 `strings.TrimSpace(line)`(忽略缩进差异);唯一才命中;要求窗口内至少一行非空,防误配;
  - 命中模糊级时,**替换的是文件里的原始窗口文本**(不是模型给的 old_string),new_string 原样写入;
  - 多处命中 → 报"不唯一,请加上下文"(与现有口径一致);全部失败 → 维持原 `old_string not found` 错误。
- 返回消息如实标注:`edited xxx (fuzzy: ignored trailing whitespace)` / `(fuzzy: ignored indentation)`,让 transcript 可审计。
- `multi_edit`(multiedit.go)如复用同类匹配逻辑,共用该 helper。
- **测试**(write_tool_test.go 或新文件):精确路径回归;行尾空格差异命中;tab/空格缩进差异命中;模糊级多处命中报错;完全不匹配报错;替换写回的是原始窗口。

### 2. 指令文件:ONECREAT.md 第一公民 + 生态兼容 ✅ 已完成

**盘点结论(动手前核实)**:internal/memory 已经具备 Codex agents_md.rs 的全部能力且更强——
多文件名发现、用户全局 + 祖先链(以 .git 为根、不越界)+ 项目 + local 覆盖、@path 导入、
同目录多文件全加载并按物理文件去重(防 symlink 双注入)。**无需新写发现逻辑。**

**实际改动**(产品对外只有 OneCreat):
- `docNames` → `{ONECREAT.md, REASONIX.md, AGENTS.md, CLAUDE.md}`(读取四名全认:
  旧项目 REASONIX.md 不用改名,Codex/CC 项目的 AGENTS.md/CLAUDE.md 照常被读);
- 新建默认 `ONECREAT.md` / `ONECREAT.local.md`(原为 AGENTS.md;品牌一致优先,
  需要跨工具共享的项目可手动用 AGENTS.md);写入仍优先既有文件、不分裂;
- /init 技能、记忆面板、i18n 提示、mock 全部改为 ONECREAT.md 口径;
- fileRemarks 给 ONECREAT.md/.local.md 加注记,REASONIX.md 标"旧名"。
- **测试**:TestDocPathDefaultsToOnecreat、TestOnecreatMdDiscoveredAndPreferred;
  旧名兼容由既有 TestDocPathPrefersExisting(REASONIX.md/CLAUDE.md)继续钉死。

### 3. 协作模式预设(干净版的"模式",非 auto-plan 回潮)✅ 已完成

**参考**:Codex `tui/collaboration_modes.rs`——用户显式选,不做关键词猜测。

**实际实现**(会话级 persona,正交于 计划/YOLO 审批维度):
- 复用 Compose 的会话级注入机制(同 plan marker / memory-update):controller 加
  `coachPreamble` 字段 + `SetCoachMode(preamble)`;Compose 把 persona 以
  `<coaching-style>…</coaching-style>` 放在用户文本**之后**注入——既像 system
  reminder,又让首条消息开头仍是用户问题、预览天然干净;previewSession 再剥一道
  防短问题露出元指令。**不动系统提示缓存**(prefix 不变,切模式即时生效)。
- 3 个模式(文案在前端常量、中文;label/desc 走 i18n):默认 / 学生引导(引导式、
  不直接给答案、每步让学生复述、落实"学生必须能逐行解释")/ 老师助手(产出完整
  材料 + 每个技术点附「为什么」教学解释)。默认空 persona = 现状,不强迫选。
- UI:composer 底部 GraduationCap chip + 下拉,和 知识库/技能/审批模式 并排。
- setCoach 作用于活动 tab 的 controller;App 用 effect 在「换模式或切 tab」时重注入,
  新 tab 自动续上当前 persona。
- **测试**:TestComposeCoachMode(默认原样 / 注入在尾部 / 每轮持续 / 清空复位);
  既有 previewSession 测试 + 新剥离逻辑覆盖预览干净。root 40 包 + desktop + tsc 全绿。

### 4. 系统提示措辞借鉴(纯文案)✅ 已完成

**参考**:`codex-rs/core/gpt_5_codex_prompt.md`。

**实际改动**:`internal/config/config.go` 的 `DefaultSystemPrompt` 新增一段
`Responding`(保持英文,缓存稳定 + 与现有一致;flag 模型双语可读),精炼 4 条:
- 解释改动直接进正文,别以 "summary" 开头(原 "briefly summarize" 易诱导套话);
- 不复述命令/工具输出(用户看得到),只提炼关键结果;
- 引用文件用 inline-code 包裹的 `path:line`(前端 extractFileReferences 能提取成
  可点击文件条),不用 file://、vscode://、https:// 链接;
- 扁平项目符号、简单确认不堆格式。
- 范围:只改全局默认 prompt(ROI 最高);硬件 prompt 已很长很细、主出接线图/代码,
  不顺手膨胀。注意 raw string 内不能放字面反引号,示例用文字"(backticks)"表达。
- **验证**:root 40 包 + desktop 全绿(无测试 hard-code prompt 内容);纯文案。

### 5. 底部状态条增强(/status 思想)✅ 已完成

**参考**:`tui/src/status.rs`——一行常显关键状态。

**核实结论**:onecreat 状态条已经很丰富(模型·effort·上下文%·now/avg 缓存·jobs·
余额·plan 标记),强加"目录短名/会话用时"是为凑数(顶部已有项目名)。

**实际改动**(聚焦、不冗余):状态栏右侧加**协作模式常驻标记**(仅非默认时,
GraduationCap 图标)。理由同 plan 标记——composer chip 在输入区,对话滚动时不在
视线,状态栏常驻底部让学生/老师随时确认当前 persona。App 传 coachLabel,
StatusBar 渲染。**验证**:Claude Preview 实点协作 chip 选「学生引导」,截图确认
composer chip 与状态栏标记同步显示;tsc + desktop build 全绿。

---

## 暂缓区(涉及网络,本计划内不开发)

> 以下各项**明确不做**。文档保留设计要点,后续由用户以其他方式或其他模型完成。

| 项 | 一句话设计要点(留给后续) | 参考 |
|---|---|---|
| **MCP Server 暴露**(把 onecreat agent 当 MCP 工具供学管系统/教学平台调用) | 网络监听 + 对外协议;参考 rmcp 的 server 端,工具面=会话级 run/approve | `codex-rs/mcp-server/` |
| **TypeScript SDK / npm 发布**(教培公司二次开发) | 进程封装 + JSONL 事件流;`startThread/run/runStreamed` API 面 | `sdk/typescript/src/codex.ts` |
| **自更新机制**(版本检查/下载) | 启动后台查版本(20h 缓存)、横幅提示、按安装渠道给升级命令 | `tui/src/updates.rs`、`cli/src/doctor/updates.rs` |
| **cloud-tasks 远程任务** | SaaS 化才需要;开源侧只有 mock,核心在 OpenAI 云 | `codex-rs/cloud-tasks*/` |
| **exec --json 无头模式对外集成**(若用于 CI/远程触发则涉网) | 本地 JSONL 事件流本身不涉网,但其 CI/远程用法归入暂缓;如仅本机批量跑材料可另议 | `codex-rs/exec/src/exec_events.rs` |

## 不采纳(评估后主动放弃,非暂缓)

- **进程级沙箱(seatbelt/landlock/bwrap)**:用户场景是老师在自己电脑跑自己项目,规则级把关足够;Go 移植与维护成本远超收益。
- **app-server 协议层拆分**:仅在做 IDE 插件/多前端时才值;现 Wails 单体够用。
- **审批 5 档 / profiles 档位矩阵**:与零配置哲学冲突;「协作模式预设」(第 3 项)已覆盖其实用部分。

## 品牌统一第二批(深层标识,单独一刀)

产品对外已统一为 OneCreat(指令文件/UI/文档口径,见第 2 项)。以下**代码内部标识**
仍为 reasonix,刻意留到单独批次——它们牵涉用户数据迁移,不能顺手改:

| 标识 | 现状 | 改名要点 |
|---|---|---|
| 用户配置/会话目录 | `~/Library/Application Support/reasonix/`(config.toml、sessions、memory) | 改名必须带**启动时自动迁移**(旧目录存在且新目录不存在→整体搬迁),否则用户配置/历史全部"丢失" |
| Go module 路径 | `module reasonix`(全仓库 import) | 纯机械替换+全量测试,但 diff 巨大,单独提交 |
| 环境变量 | `REASONIX_LANG` / `REASONIX_CONFIG_DIR` / `REASONIX_HARDWARE_MCP` | 新名优先、旧名兜底读一段时间 |
| 二进制名 | `reasonix-hardware-mcp`、`reasonix-desktop` | 牵动打包脚本/NSIS/release workflow/resolveHardwareMCP 查找名 |
| 旧版内核文档 | docs/SPEC.md、MIGRATING.md、CHECKPOINTS.md 等 | 描述的就是 reasonix 内核,随深层改名一起更新 |

## 执行顺序与提交

1 → 2 → 3 → 4 → 5,每项独立 commit(中文 commit message,说明借鉴自 Codex 哪个文件)。
每项完成后在本文档对应小节标 ✅ 与 commit hash。
