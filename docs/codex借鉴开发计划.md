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

### 2. AGENTS.md 兼容 + 向上多级合并

**参考**:Codex `codex-rs/core/src/agents_md.rs`——从项目根(以 .git 为标记)到 cwd 逐级收集 AGENTS.md,按"根→cwd"顺序拼接,分隔符注明来源目录,总量上限 32KB,不越项目根向上。

**设计**:
- 找到 onecreat 读 REASONIX.md 的位置(internal/ 内 memory/boot 相关,grep `REASONIX.md`),在同一注入点增加 AGENTS.md 读取:同目录下若同时存在 REASONIX.md 与 AGENTS.md,**REASONIX.md 优先、AGENTS.md 跳过**(避免双重注入);只有 AGENTS.md 时读它。
- 实现逐级向上收集(止于 .git 所在根),按根→cwd 顺序拼接,带 `--- AGENTS.md ({dir}) ---` 分隔,总预算 32KB 截断。
- **测试**:仅 AGENTS.md 时被注入;两者并存时只注入 REASONIX.md;嵌套目录合并顺序;32KB 截断。

### 3. 协作模式预设(干净版的"模式",非 auto-plan 回潮)

**参考**:Codex `tui/collaboration_modes.rs`——开局可选模式,每个模式 = 一套参数化开场指令;**用户显式选**,不做关键词猜测。

**设计**(轻):
- 在首页启动台或 composer 模式选择处,增加 2~3 个教培模式:「学生自学」(多引导提问、不直接给完整答案、每步让学生复述)、「作业助手」(直接干活)、「老师备课」(产出教学材料口径)。本质 = 不同的开场 system 注入文案,复用现有 onPrompt/手段,不动内核。
- 默认 = 现状(无模式),不强迫选择,符合零配置。
- **验证**:选模式后首条 prompt 带上对应指令;不选则与现状完全一致。

### 4. 系统提示措辞借鉴(纯文案)

**参考**:`codex-rs/core/gpt_5_codex_prompt.md`(69 行)。值得抄的点:
- "不要以'总结/Summary'开头,直接进入正文";
- "不要复述命令输出,用户看得到";
- 文件引用统一 `path:line` 格式(前端可点击);
- 最终回答:简洁、分组清晰、不嵌套项目符号。

**设计**:把以上 3~4 条揉进 onecreat 默认 system prompt(internal/config 的 DefaultSystemPrompt)与硬件 prompt 的输出要求段;中文表述。不改结构,只改文案。
- **验证**:tsc/go 编译即可(纯文案),人工抽查一轮回答风格。

### 5. 底部状态条增强(/status 思想)

**参考**:`tui/src/status.rs`——一行常显:模型/目录/token/成本。

**设计**(轻):onecreat 底部已有模型/effort/上下文%/费用 chips;补齐缺的(如当前工作目录短名、会话用时),并保证字段点击有 tooltip 解释。不做新面板。
- **验证**:前端渲染正常,tsc 干净。

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

## 执行顺序与提交

1 → 2 → 3 → 4 → 5,每项独立 commit(中文 commit message,说明借鉴自 Codex 哪个文件)。
每项完成后在本文档对应小节标 ✅ 与 commit hash。
