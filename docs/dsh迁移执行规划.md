# OneCreat 底层 agent 迁移到 DeepSeek Harness(dsh)——执行规划

> **状态更新 2026-08-19**:Phase 1(sidecar 打通)与 Phase 2(护城河搬迁)**已实施完成并逐条验收**,
> 在分支 `feat/dsh-engine` 上(未 push)。做到哪、怎么验的、没做的、坑、待拍板 全部见
> **[`docs/dsh调研/05_Phase1-2_实施报告.md`](dsh调研/05_Phase1-2_实施报告.md)**,那份报告里的
> **功能对照表**(Controller 每个方法:已迁/复用/降级/暂不支持+理由)是本规划 G2 闸门要的东西。
> 一句话:`engine="dsh"` 已是可用引擎(CLI + Web 模式全通、硬件 MCP/证据引擎/网关红线都过),
> **默认仍是 `native`**,下一步是 Phase 3 灰度。
>
> 起草 2026-08-18。**2026-08-19 用户拍板:直接迁移,不再做 A/B 对比**(用户自己试过 dsh 效果好;判断 dsh 缺的正是我们的插件+知识库)。Phase 0 的 G0 视为通过,Phase 1 spike 已有驱动层骨架(`internal/engine/dsh`,见 `docs/dsh调研/04_Phase1_spike报告.md`),下一步直接做 Phase 1 收尾(Controller 接线)+ Phase 2 护城河搬迁。
> 原文:状态:**待拍板**。前置结论见对话:两个候选(pi / dsh)里选 dsh,原因是产品 DeepSeek-first、dsh 是模型厂自家 harness、"一切皆插件"正对上我们的护城河(证据引擎/硬件 MCP/网关档位)。
> 硬约束:dsh 目前是 developer preview,官方声明会有破坏性变更 → 本规划每一阶段都设"闸门",不过闸不进下一阶段;旧 Go 内核在最后一阶段之前**不删**。

## 0. 目标与非目标

**目标**:把 `internal/agent`(run loop)+ 模型调用这一层换成 dsh sidecar,其余全部保留——Wails 桌面/React 前端、账号与三档、平台网关与模型隐私、证据引擎、硬件 MCP/串口/OTA、skills、serve/acp/cli 三个前端。

**非目标**(本轮明确不做):
- 不重写 React 前端,不改走"dsh Web UI + web 插件"路线(留作后续选项,原因:官方 Web UI 里锁不住模型选择/API 配置,和"只见档位不见模型"冲突)。
- 不把账号/点数/档位逻辑搬进 dsh。
- 不同时迁 pi。pi 只作为 G0/G1 失败后的 B 计划。

## 1. 架构形态(目标态)

```
React 前端(不动) ── Wails bridge(不动) ── desktop/app.go(不动)
                                              │
                                     control.Controller ←── 现有接口不变
                                              │
                          ┌───────────────────┴───────────────────┐
                 engine = "native"                        engine = "dsh"
                 internal/agent(现内核)              internal/engine/dsh(新)
                                                     │  JSON-RPC over stdio / ACP
                                                     ▼
                                                dsh sidecar(内置 Node 运行时,版本锁死)
                                                     ├─ provider: 平台网关(OpenAI 兼容,ONECREAT_GATEWAY_TOKEN)
                                                     ├─ MCP: reasonix-hardware-mcp(现成二进制,零改动)
                                                     └─ dsh 插件(TS): 证据钩子 / 系统提示注入 / skills
```

关键判断:`control.Controller` 已经是"前端无关的编排对象",事件走 `event.Sink`——**这层抽象就是换内核的接缝**。工作量集中在写一个新的 Controller 后端,而不是碰任何前端。

## 2. 阶段与闸门

### Phase 0 — 验证(1–2 周,零代码改动)

目的:回答"dsh + DeepSeek 是不是明显强于我们现在的内核",不明显就不迁。

| # | 事项 | 产出 |
|---|---|---|
| 0.1 | 本机装 dsh(`npx @deepseek-ai/dsh web` 或 hairyf Tauri 版),读 `docs/architecture.md`、`sdk/`、`llm/` 源码 | 一页笔记:扩展点清单(tool 前后钩子、系统提示注入、事件类型、session 格式、fork、headless/JSON-RPC 是否稳定) |
| 0.2 | **接网关**:dsh 的 DeepSeek provider 能否改 base URL + 自定义 header;错误体/UI/日志里会不会漏真实模型名 | 可行/不可行 + 漏点清单 |
| 0.3 | **实测对比**:用已有 ESP32 dogfood 题(记忆库那批,5–8 题,含真机烧录+串口)跑 dsh 原生 vs 现内核 | 表格:一次成功率 / 轮数 / token / 是否胡说(诚实性) |
| 0.4 | 检查 dsh 的 skill / MCP / 计划模式 / 审批模型,与我们现有概念对齐 | 概念映射表 |

**闸门 G0**:0.2 可行 **且** 0.3 明显更强(至少一次成功率或诚实性有肉眼可见提升)。否则停止,不迁,把结论写进记忆。

### Phase 1 — Sidecar 打通(2–3 周)

目的:桌面端一个 tab 用 dsh 跑通"聊天 + 工具 + 审批 + 取消",前端零改动。

| # | 事项 | 涉及文件 |
|---|---|---|
| 1.1 | 新包 `internal/engine/dsh`:拉起/守护 dsh 进程(JSON-RPC over stdio 或 ACP),生命周期与 tab 绑定 | 新增 |
| 1.2 | 实现 Controller 最小子集:`Submit/Send/Cancel/Running/Approve/PendingApprovals/SetPlanMode/History/NewSession/Resume/SessionPath` | `internal/control` 加接口抽取或第二实现 |
| 1.3 | 事件映射:dsh 事件流 → `event.Event`(text delta / tool start-end / approval / usage / error) | `internal/event`,`desktop/wire.go`,`internal/serve/wire.go` 保持一致 |
| 1.4 | 配置:`engine = "native" | "dsh"`(默认 native),`[dsh]` 段(二进制路径/版本/端口),render.go 渲染 + round-trip 测试扩展 | `internal/config`,`render.go`,`boot.Build` |
| 1.5 | 打包:仿 Tauri 版 sidecar 做法——内置 Node 运行时 + **锁死 dsh 精确版本**;`scripts/desktop-build.sh` 增加 sidecar 装配;macOS 先通,Windows 后置 | `scripts/desktop-build.sh`,`desktop/` |
| 1.6 | 会话文件归属:明确 dsh 自己的 session store 与我们的 session 文件谁是真源,避免双写(生态里已经出过 dual-writer 损坏) | 设计决策 |

**闸门 G1** ✅ **已过(2026-08-19,烧录/串口那一步除外)**:`engine="dsh"` 下 `reasonix run` 与 Web 模式
都跑通了 ESP32 **编译**(296114 字节固件),取消/审批正常,三套 build·vet·test + 前端 tsc + `dsh/` typecheck 全绿。
**未实测**:真机烧录 + 串口验证(本次没插板子)。

> 实施与规划的差异(重要):最终接缝**不是**"在 control 里抽 engineBackend 全量接口",而是既有的
> `agent.Runner`(一轮对话)+ 一个只有 6 个方法的可选 `EngineBackend`(引擎自有状态)。改动面小得多,
> 且 native 路径可证不变(`c.engine == nil`)。详见实施报告 §1。

### Phase 2 — 护城河搬迁(3–4 周)

| # | 能力 | 做法 | 备注 |
|---|---|---|---|
| 2.1 | 硬件 MCP | 直接把 `reasonix-hardware-mcp` 注册给 dsh | 零改动,最先做 |
| 2.2 | 证据引擎 | **先不重写**:Go 驱动层消费 dsh 工具事件流喂现有 `internal/evidence`;后期视需要再做 TS 插件 | 保住 `complete_step` ↔ `todo_write` 匹配语义 |
| 2.3 | 网关 + 模型隐私 | dsh provider 配 base URL/token;Go 驱动层对错误体/事件做模型名兜底过滤;网关路径下禁用任何 dsh 侧 planner/model 切换 UI | 对应现 `config.ModelPrivacyPolicy` |
| 2.4 | 账号/档位/点数 | 全在 Go + 前端,不动 | — |
| 2.5 | Skills / 板卡事实注入 / 诚实收尾护栏 | 迁到 dsh skill 与系统提示扩展点;能删的提示词工程就删(官方 harness 该吸收的让它吸收) | 逐条对照 `internal/skill/builtins.go` |
| 2.6 | 会话回退 / 分支 / 摘要 | 映射到 dsh 的 fork/summary;文件级 checkpoint(`internal/checkpoint`)保留 Go 实现或明确放弃 | 列"保留/映射/放弃"清单 |
| 2.7 | serve / acp / cli | 因走同一 Controller 自动受益,各跑一遍冒烟 | — |

**闸门 G2** ⚠️ **部分过(2026-08-19)**:功能对照表已逐行给出"已迁 / 复用 / 降级 / 暂不支持 + 理由",
没有"待定"(见实施报告 §4);自动化测试全绿。**未过**:三个 ESP32 **真机**用例(没插板子,只到编译)。

Phase 2 各行的实际结果:2.1 硬件 MCP ✅ / 2.2 证据引擎 ✅(含"谎报烧录被判未完成"的单测)/
2.3 网关+模型隐私 ✅(**多做了一件规划没写的事**:dsh 默认往系统提示塞 harness 身份句,已关掉)/
2.4 账号档位 未动 ✅ / 2.5 skills+注入 ✅(零 TS 改动)/ 2.6 checkpoint ✅ 文件级保留 Go 实现并实测
rewind 成功,fork/摘要/对话回退明确"暂不支持" / 2.7 serve·acp·cli **只冒烟了 cli 的 run**。

### Phase 3 — 灰度与切换(2 周)

1. 默认仍 native,自己日常改用 dsh 一周,记 bug。
2. 默认切 dsh,native 保留一个发布版本作为回退开关。
3. 下一版本删 native 内核与死代码;更新 `CLAUDE.md`、`docs/开发工作流.md`、记忆库。

## 3. 风险清单

| 风险 | 应对 |
|---|---|
| dsh 预览期破坏性变更 | 锁精确版本;升级当独立任务做,过 G1 冒烟才换版本 |
| provider 只有 DeepSeek,网关兼容性未知 | Phase 0.2 前置验证,不通直接停 |
| 模型名泄漏(错误体/日志/UI) | Go 驱动层兜底过滤 + 前端不渲染;是 SaaS 硬红线 |
| 桌面端体积/启动:Go 单二进制 → 捆 Node | 参考 Tauri 版 5MB 方案(按需自举下载运行时);Windows 后置 |
| session 双写损坏 | 1.6 单一真源决策 |
| 时间投入(兼职开发) | 总计约 2.5–3 个月;每闸门可停,前两阶段沉没成本最小 |

## 4. 立即可做的下一步

Phase 0.1–0.3:装 dsh、接网关、跑对比。**不需要任何代码改动**,一到两周出 G0 结论。
