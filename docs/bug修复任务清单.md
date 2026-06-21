# OneCreat Bug 修复任务清单（交付 Opus 4.8 执行）

> 生成日期：2026-06-10
> 范围：DeepSeek-Reasonix 全项目审查（5 路并行 + 人工复核调用链）
> **已排除**：网络连接质量问题、SSH 密码泄露、scp/ssh 凭据注入（这三类不在本次修复范围）
> 工作目录：`/Users/localwork/06_System/reasonix_source/DeepSeek-Reasonix`

---

## 0. 给执行者（Opus 4.8）的总则

**全局约束，每一项都必须遵守：**

1. **每改一处，先读上下文确认根因仍成立**——本文档写于审查时，若代码已变动，以实际代码为准。
2. **改组件前先 grep 确认谁在 import 它**（本仓库有死代码风险）。前端改 `_components/*.tsx` / `lib/*.ts` 前确认 `App.tsx` 实际引用的是哪个。
3. **每个 cluster 改完立即验证**：
   ```bash
   cd /Users/localwork/06_System/reasonix_source/DeepSeek-Reasonix
   go build ./...                      # 必须通过
   go vet ./...                        # 必须通过
   go test ./internal/... ./cmd/...    # 相关包必须绿
   ```
   前端改动：`cd desktop/frontend && pnpm tsc --noEmit && pnpm build`
4. **每个 cluster 完成是一个 commit checkpoint**（不要 push）。commit message 写清楚改了哪几条。
5. **诚实汇报**：每条改完记录「实际动了哪些文件 / 有没有跳过 / 哪里不确定」。
6. **不要顺手"优化"无关代码**。每行改动都要能追溯到本文档某一条。
7. **风格对齐**：注释用中文，匹配文件现有风格；并发改动遵循"单写者 + Snapshot 跨线程"的既有约定。
8. **已知不要碰**：`cmd/reasonix-hardware-mcp/main.go` 工作区里已有一处未提交的旧改动（非本文档引入），别把它卷进你的 commit。
9. **A 区（多标签）必须整簇一起做**——多条 bug 同根（"按 tabID 寻址 + 统一加锁"），分开改会互相打架。其余 cluster 相互独立，可任意顺序。

**建议执行顺序**：A（多标签，最高优先且同根）→ B（压缩/checkpoint）→ C（工具）→ D（配置）→ F（硬件）→ E（provider/其他 low）。

---

## Cluster A — 多标签真并行（最高优先，必须整簇一起改）

> 背景：这是 2026-06 新上的 A 档功能（多个会话标签同时跑 agent）。当前一串 bug 同根：**很多 `App` 方法只认 `a.ctrl`（活动标签镜像），不按 `tabID` 寻址；且对 `a.ctrl` 的读写一半加锁一半不加。**
> 涉及文件：`desktop/app.go`、`desktop/settings_app.go`、`desktop/sessions.go`、`desktop/frontend/src/lib/useController.ts`、`desktop/frontend/src/lib/bridge.ts`、`desktop/frontend/src/App.tsx`
>
> **先做一件事**：通读 `desktop/app.go` 顶部的 `App` 结构体、`tabRuntime` 结构、`a.mu` 的加锁约定、`tabs map`、`activeTab`，以及 `SetModel`(app.go:~2491) 和 `SwitchWorkspace`(app.go:~770) 这两个**正确**的范例——它们都「加 `a.mu.Lock()` + 回写 `rt.ctrl`」。本簇的修复就是把这个正确范例推广到所有漏掉的地方。

### A1【high】`rebuild()` 不回写 tabRuntime 且全程无锁

- **位置**：`desktop/settings_app.go:182`（`rebuild()`），调用方 `applyConfigChange`(settings_app.go:164)
- **现象**：改任意设置（provider / key / 权限 / sandbox / effort / skill 路径 / RefreshSkills）后——自动存盘存的是旧的已 `Close()` 的 controller → 新对话不再落盘；切走再切回该标签 → `SetActiveTab` 把活动镜像重指回 `rt.ctrl`（旧的已关闭 controller）→ 提交打到已关闭 controller，标签报废。
- **根因**：`rebuild()` 换了 `a.ctrl = ctrl` 却**不写 `a.tabs[a.activeTab].ctrl`**，也**全程不持 `a.mu`**。对照 `SetModel`(app.go:2513) 同样的换 controller 操作，那里既 `a.mu.Lock()` 又回写了 `rt.ctrl / rt.model / rt.label`。
- **修复**：
  1. 在 `rebuild()` 写 `a.ctrl / a.model / a.label / a.startupErr` 的那段加 `a.mu.Lock()/Unlock()`（注意：`boot.Build` 是秒级耗时操作，**锁不能包住 `boot.Build`**——参照 `SetModel`：先在锁外 build，再进锁写字段）。
  2. 进锁后，同步回写活动标签：`if rt := a.tabs[a.activeTab]; rt != nil { rt.ctrl = ctrl; rt.model = model; rt.label = ctrl.Label() }`。
  3. 检查 `rebuild()` 里 `a.ctrl.Snapshot()/History()/Close()` 那段读旧 controller 的代码——读 `a.ctrl` 也要在锁的保护下取一次快照指针再用（参照 `SetModel` 的 `ctrl := ...; RUnlock` 模式）。
- **验证**：改一项设置（如 `/effort`）后，① 继续对话能正常落盘（检查 session `.jsonl` 有新轮次）；② 切走再切回该标签仍能提交。补一个 Go 测试或手动验证清单。

### A2【high】后台标签的审批/ask 弹窗永久丢失 → 任务永久卡死

- **位置**：发射端 `desktop/app.go:3083`（`eventSink.Emit` → `agent:event:<tabID>`）；订阅端 `desktop/frontend/src/lib/useController.ts:466`（只 `onEvent(tabId,…)` 当前活动标签）；应答端 `desktop/app.go:393`（`App.Approve` 只打到 `a.ctrl`）；阻塞点 `internal/control/controller.go:1675`（`requestApproval` emit 一次后 `select` 阻塞等 `Approve`）。
- **现象**：标签 A 跑一个需审批的任务（非 YOLO 默认会审批写操作 / bash），切到标签 B，A 到达审批点 → 审批事件发到无人订阅的通道被丢弃，**且** 即便看到也无法应答（`App.Approve` 路由到活动标签 B 的 controller）。A 的 turn 永久阻塞，只能 Cancel。这正是"后台并行"的核心场景。
- **根因**：审批是 per-controller 的阻塞调用，但前端只订阅活动标签、`App.Approve` 只认活动 controller。后台标签的审批既**看不到**也**答不了**。
- **修复**（这是一个设计决策，按推荐方案做，若你判断有更优解先说明再改）：
  - **应答端必须按标签路由**：把 `App.Approve` 改成 `App.Approve(tabID string, id string, allow, session bool)`，内部 `rt := a.tabs[tabID]`（加锁取）→ `rt.ctrl.Approve(id, allow, session)`。前端调用处带上事件来源的 tabID。同步改 `SetPlanMode/SetCoachMode` 等所有"只打活动 ctrl"的运行时控制方法是否也需按标签路由（见 A8）。
  - **展示端让后台审批可见**：推荐做法——前端在创建标签时就为**每个**标签挂一个轻量订阅，把非活动标签的 `approval_request` / `ask_request` 攒进一个「跨标签待处理审批」状态（带 tabID + 标签名），在 UI 上以角标/通知形式提示「标签 X 有待审批」，点击切过去再处理。
  - 备选（改动更小）：切回标签时由后端补发该标签当前 pending 的审批事件——需要 `Controller` 暴露「当前未应答的审批列表」查询接口（`requestApproval` 里 `c.approvals` map 已有 id，可加一个 `PendingApprovals()` 返回 `[]event.Approval`），`loadSessionData` 时拉取并重放。
- **验证**：标签 A 发一个会触发写文件审批的任务 → 立刻切到 B → 确认 UI 能提示 A 有待审批 → 切回 A（或直接在提示上）批准 → A 的 turn 继续完成。
- **风险**：这条改动面最大，涉及前后端接口签名变更。务必先把 `App.Approve` 的所有调用点 grep 出来（前端 `bridge.ts` / `App.tsx` / `ApprovalModal`）一并改。

### A3【medium】切回正在跑的标签：History 回填覆盖已流入事件 + running 状态丢失

- **位置**：`desktop/frontend/src/lib/useController.ts:466`（切标签 effect：先 `dispatch reset`，订阅，再异步 `loadSessionData`/`History()`）；history reducer 在同文件 `:402` 附近。
- **现象**：标签 A 任务进行中 → 切到 B 再切回 A → `History()` 异步返回后**整体替换 items**，把订阅后已流入的工具卡片/文本段清掉；且 `reset` 后 `running=false`，但 A 实际有 turn 在跑 → UI 显示空闲：无 spinner、Composer 可再次提交（打进正在跑的 controller）、"新任务/设置"按钮的 `disabled={state.running}` 守卫失效。
- **根因**：① history dispatch 是「整体替换」而非「与已流入事件对账合并」；② 切标签时没有任何途径恢复目标标签的 `running` 真值（Meta 不含 running，无 `turn_started` 重放）。
- **修复**：
  1. **running 真值**：让后端 `Meta()`（或新增 `TabState(tabID)`）返回该标签 controller 的运行态（`Controller` 需暴露 `IsRunning() bool`——内部 `c.running` 已有，加只读 getter）。`loadSessionData` 拿到后 `dispatch({type:"running", running})`。
  2. **history 合并**：history reducer 改为——若当前已有同轮的 live 内容，则以 history 为基线、保留订阅后已到达且 history 里还没有的尾部事件（按消息/工具 id 去重）。最稳妥的做法：history 回填只在 `items` 为空时整体填充；非空时只补 history 里有而当前没有的前缀，不清尾部 live 块。
- **验证**：A 跑一个多步任务 → 切到 B 等几秒 → 切回 A → spinner 仍转、流式内容不丢、Composer 在 running 时禁用。

### A4【medium】ResumeSession / DeleteSession 只防活动标签 → 跨标签双写同一 session 文件

- **位置**：`desktop/app.go:607`（`DeleteSession`）、`desktop/app.go:627`（`ResumeSession`）
- **现象**：`DeleteSession` 只比活动 ctrl 的 `SessionPath()`，后台标签正在写的 session 可被删除；`ResumeSession` 不查目标 session 是否已被别的标签占用 → 标签 A 开着 session X，标签 B 从历史 resume X → 两个 controller 各自整文件快照同一个 `.jsonl`，后写覆盖先写，对话轮次互丢。
- **根因**：守卫逻辑只读 `a.ctrl`，没遍历所有标签的 `SessionPath()`。
- **修复**：在两个方法里改为遍历 `a.tabs`（加锁），收集**所有**标签 controller 当前的 `SessionPath()`：
  - `DeleteSession`：若目标路径被任一标签占用 → 拒绝并返回明确错误（"该会话正在标签 X 使用中"）。
  - `ResumeSession`：若目标已被其他标签占用 → 拒绝，或直接切到那个已打开的标签（推荐后者，体验更好）。
- **验证**：标签 A 开 session X，标签 B 尝试 resume X → 被拒绝或切到 A；尝试 delete X → 被拒绝。

### A5【medium】SetModel 期间切标签 → 新 controller 装进错误的标签

- **位置**：`desktop/app.go:2491`（`SetModel`）
- **现象**：`SetModel` 在锁外做秒级 `boot.Build`，完成后写 `a.tabs[a.activeTab]`——此时 `activeTab` 可能已被用户切走：A 的旧 controller 被 Close、携带 A 对话的新 controller 写进 B 的 runtime；A 留下已关闭 ctrl。另外 `boot.Build` 传的 `Sink: a.sink`（app.go:~2510）若无锁读，事件可能串到错误标签通道。
- **根因**：把"哪个标签发起的切换"丢了——build 完成后用的是当下的 `activeTab` 而非发起时的 tabID。
- **修复**：在 `SetModel` 入口（锁内）先把发起标签 `targetTab := a.activeTab` 固定下来，并为新 controller 绑定**该标签的** sink（`eventSink{tabID: targetTab, …}`，参照 `buildTab` 怎么给每个标签建独立 sink）。build 完成后写回 `a.tabs[targetTab]` 而非 `a.tabs[a.activeTab]`。
- **验证**：切模型后立刻切标签，确认新模型装在发起标签上、事件不串台。
- **关联**：A2 若把 `SetPlanMode` 等改成按 tabID 路由，这里一并对齐。

### A6【medium】CloseTab 在 buildTab 未完成时关闭 → controller 泄漏

- **位置**：`desktop/app.go:286`（`CloseTab`）、`desktop/app.go:184`（`buildTab`）
- **现象**：关标签时若 `boot.Build` 还在跑（`rt.ctrl == nil`），CloseTab 什么都不 Close；buildTab 随后完成，把 controller 写进已移出注册表的 `rt`，并对死标签 emit `agent:ready`。这个 controller（含 MCP 子进程、session 文件、goroutine）永远没人 Close。
- **修复**：给 `tabRuntime` 加一个 `closed bool`（或 `context.CancelFunc`）。`CloseTab` 标记 `rt.closed = true`（并 cancel buildTab 的 ctx）；`buildTab` 完成后在写 `rt.ctrl` 前检查 `if rt.closed { ctrl.Close(); return }`，不 emit ready。两处都在 `a.mu` 保护下读写该标志。
- **验证**：新建标签后立刻（ready 前）关闭 → 确认 controller 被 Close（无孤儿 MCP 子进程；可 `ps` 查或加日志）。

### A7【medium-low】shutdown / ApplyUpdate 只收尾活动标签

- **位置**：`desktop/app.go:334`（`shutdown`）、`desktop/updater_app.go:108`（`ApplyUpdate`）
- **现象**：退出/自更新时只对活动镜像 `a.ctrl` 做 Snapshot+Close；后台标签 controller 不快照、不关闭 → 后台标签进行中的最后一轮丢失，MCP stdio 子进程成孤儿。
- **修复**：两处都改为遍历 `a.tabs`（加锁取快照列表），对每个非 nil `rt.ctrl` 依次 `Snapshot()` + `Close()`。
- **验证**：多标签开着退出 → 确认每个标签都落盘、无孤儿子进程。

### A8【medium，安全相关】plan/YOLO/persona 模式不随新标签生效，pill 与实际门控不一致

- **位置**：`desktop/frontend/src/App.tsx:362`（coach effect）、`:426`（applyMode/mode 全局态）、`:665`（createTab）；后端 `desktop/app.go:403`（`SetPlanMode`/`SetCoachMode` 在 `ctrl==nil` 时静默 no-op）。
- **现象**：`mode`(plan/yolo) 是 App 级全局 state，但 plan/bypass 是 per-controller 运行时状态。新建标签（controller 异步装配、初始 nil）时：plan/yolo 不重放、coach effect 打到 nil ctrl 被静默吞。**后果方向危险**：底部 pill 显示 "plan（只读）"，而新标签 controller 实际是 normal 模式（会真改文件）。
- **根因**：模式是全局 UI 态，但门控是 per-controller，二者在「新标签 / 切标签 / build 完成」三个时机没有同步。
- **修复**：
  1. 让 `mode` 成为「每标签」状态，或在每次 `agent:ready`（onReady）和切标签时，把当前 UI `mode` 重新下发到该标签 controller（`SetPlanMode/SetBypass/SetCoachMode` 按 tabID 路由——与 A2 同一套路由改造）。
  2. `SetPlanMode` 等在 `ctrl==nil` 时不能静默 no-op：要么排队等 ready 后应用，要么前端在 ready 后重发。
  3. pill 显示改为读「该标签 controller 的真实门控状态」（后端 Meta 暴露 planMode/bypass 真值），而非全局 UI 猜测。
- **验证**：开 plan mode → 新建标签 → 在新标签让模型尝试写文件 → **必须被只读门拦截**，且 pill 与实际一致。
- **优先级说明**：这条虽标 medium，但因为是「显示只读、实际可写」的安全错觉，建议和 A2 一起优先做。

### A9【medium】SubmitDisplay 及多个 MCP 方法无锁读 `a.ctrl`（数据竞争）

- **位置**：`desktop/app.go:366`（`SubmitDisplay` 读 `a.ctrl` 3 次）、`:1411`（AddMCPServer）、`:2174`（RemoveMCPServer）、`:2190`（RetryMCPServer）、`:2201`（SetMCPServerEnabled）、`settings_app.go:135 / :254`（读 `a.ctrl` / 写 `a.model`）。
- **现象**：这些方法直接读 `a.ctrl`，与 `Submit/Cancel` 的 RLock 模式不一致 → 与 buildTab/SetActiveTab/SetModel/rebuild 的写并发即数据竞争。`SubmitDisplay` 还有 TOCTOU：检查与 `a.ctrl.Submit` 之间 controller 可能被换成 nil/Close。`SubmitDisplay` 是知识库增强消息和硬件面板 prompt 的主提交路径，调用频繁。
- **修复**：统一改为 `a.mu.RLock(); ctrl := a.ctrl; a.mu.RUnlock(); if ctrl != nil { … }`（与 `Submit` 一致）。`SubmitDisplay` 取一次 `ctrl` 局部变量后全程用它，消除 TOCTOU。
- **验证**：`go test -race ./desktop/...`（若有桌面测试）；至少 `go vet`；人工压测：发消息同时切模型/开关 MCP 不崩。

### A10【low】session 侧车文件读改写无同步 → 并发丢更新

- **位置**：`desktop/sessions.go:71`（`rememberSessionCwd`）、`:202`（`recordSessionDisplay`）、`:122`（`setSessionTitle`）
- **现象**：`.display.json / .cwds.json / .titles.json` 都是「读全量→改→原子写回」，无互斥。两标签几乎同时 `SubmitDisplay` 时后写覆盖先写，丢一条 display/cwd 映射 → 历史面板某条消息显示原始长 prompt 而非短显示文本，或会话归错文件夹。原子 rename 保证文件不损坏，但不防丢更新。
- **修复**：给这三个侧车文件的读改写加一把进程内互斥锁（`sessions.go` 包级 `var sidecarMu sync.Mutex`，每个读改写整段持锁）。
- **验证**：两标签同时高频 SubmitDisplay，确认映射不丢。

---

## Cluster B — 压缩（compaction）与 checkpoint / 会话状态

> 涉及文件：`internal/control/controller.go`、`internal/agent/compact.go`、`internal/cli/rewind.go`、`internal/cli/chat_tui.go`

### B1【high】压缩后 cpBound 失效不清除 → rewind / fork 切错位置，静默破坏会话

- **位置**：`internal/control/controller.go`：`cpBound` 定义/重建在 `:256`，唯一清空点在 `summarizeAt`(`:1037`)；Rewind 在 `:763`、Fork 在 `:820` 用 `cpBound[turn]` 当截断下标。漏网路径：手动 `/compact`(`Controller.Compact` → `executor.CompactNow`) 和 agent **每轮自动压缩** `maybeCompact`（agent/compact.go）都用 `session.Replace()` 重写日志、改变消息数量与下标，却不通知 controller、不清 `cpBound`。
- **现象**：任意一次压缩（自动或手动）后做 Esc-Esc / `/rewind` 的「仅对话」或「分叉」→ 用陈旧下标 `s.Messages[:boundary]` 截断到错误位置（可能切掉摘要、保留错误尾部）；只要 `boundary <= len` 就静默切错；Fork 同理。
- **根因**：`cpBound`（turn→消息下标映射）是 controller 维护的，但压缩在 agent 层重写日志，两层没有失效通知机制。
- **修复**：
  1. 给 `Session.Replace()`（或 agent 的 compact 路径）增加一个回调/版本号，让 controller 能感知「日志被重写过」。最简方案：`Controller` 暴露 `InvalidateCheckpoints()`，在三个压缩入口（`summarizeAt` 已做、`Controller.Compact`、以及 agent 自动压缩完成后通过已有的事件/回调通知 controller）统一调用，清空 `cpBound` 和相关 `cpTurn` 映射。
  2. agent 自动压缩（`maybeCompact`）→ controller 的通知通道：检查是否已有 compact 事件/Notice；若有，controller 监听该事件清 `cpBound`；若无，加一个。
  3. **保护性兜底**（即使上面做了也要加）：Rewind/Fork 取到的 `boundary` 在使用前校验其合理性，越界或映射缺失时**拒绝并明确报错**，而不是静默截断（见 B4）。
- **验证**：跑几轮对话 → 触发一次压缩（手动 `/compact` 或喂长输入触发自动压缩）→ Esc-Esc 回退 → 确认回退到正确位置或明确拒绝，绝不静默切错。补一个回归测试覆盖「压缩后 rewind」。

### B2【high】`/compact` 与 `/new` 无 running 守卫 → 与正在跑的 turn 并发

- **位置**：`internal/control/controller.go:425`（Submit 的 `/compact` 分支 `go func(){ c.Compact(…) }()`）、`:437`（`/new` 分支 `go func(){ c.NewSession() }()`）；`Compact` 本身 `:680` 无 running 检查。
- **现象**：turn 进行中也能触发。`agent.compact()` 先**无锁读** `a.session.Messages`，做几秒 summarize，然后 `Replace()` 整个日志；期间 turn 用 `Session.Add` 追加的消息被整体覆盖丢弃，同时无锁读+带锁写构成数据竞争。`/new` 的 `NewSession` 会 `SetSession` 换掉会话指针，turn 运行中写入混合状态。desktop/HTTP 走 `Submit` 的前端直接暴露（TUI 因 Enter 在 running 时被拦稍轻，但 `/compact` 经 tea.Cmd 异步也有窗口）。
- **修复**：
  1. `Controller.Compact` 和 `NewSession` 入口加 running 守卫：`c.mu.Lock(); if c.running { c.mu.Unlock(); return errBusy }`（或排队到 turn 结束后执行）。明确给用户 Notice："正在运行中，请等当前轮结束再压缩/新建"。
  2. `agent.compact()` 读 `session.Messages` 必须走 `Snapshot()`（持锁），不能无锁读。
- **验证**：turn 运行中执行 `/compact` 和 `/new` → 被拒绝或排队，不丢消息、`-race` 干净。

### B3【medium-low】Rewind 直接改 `Session.Messages` 绕过会话锁

- **位置**：`internal/control/controller.go:781`（`s.Messages = s.Messages[:boundary]` 不持 `s.mu`）；对照 `internal/agent/session.go:16` 的约定「mu guards Messages，跨 goroutine 走 Snapshot」。
- **现象**：TUI 单事件循环下没事，但 `serve`(HTTP/SSE) 前端从多个请求 goroutine 调 `History()`(RLock) 与 `Rewind`，构成数据竞争。
- **修复**：给 `Session` 加一个持锁的截断方法（如 `Truncate(boundary int) bool`），Rewind/Fork 调它而非直接切片。`Truncate` 内部 `s.mu.Lock()`，并返回 boundary 是否合法（与 B4 合并实现）。
- **验证**：`go test -race ./internal/...`。

### B4【low-medium】Rewind 截断被跳过时仍报告成功

- **位置**：`internal/control/controller.go:782`（`if boundary <= len(s.Messages)` 不成立时，整段截断+cpTurn/cpBound 更新被跳过，但仍 emit "rewound conversation to turn N" 成功 Notice）。
- **现象**：用户以为回退了，模型上下文原封不动（结合 B1，压缩后完全可能发生）。
- **修复**：截断条件不成立时，emit 失败 Notice（"无法回退到该 turn：会话已被压缩/边界失效"）并 return，不发成功消息。与 B1.3 / B3 的 `Truncate` 返回值统一处理。
- **验证**：构造 boundary 越界场景 → 确认报失败而非假成功。

### B5【low】rewind 面板的 summarize 在 UI 事件循环上同步执行 → TUI 冻结

- **位置**：`internal/cli/rewind.go:128`（`m.ctrl.SummarizeFrom/UpTo(context.Background(), …)` 直接在 bubbletea `Update` 里同步跑一次完整 summarizer 调用）。
- **现象**：期间整个 TUI 冻结（无 spinner、无法取消）。同文件的 `/compact` 已特意改成异步（chat_tui.go:2390 注释），这两条漏了。
- **修复**：照 `/compact` 的范式，把 `SummarizeFrom/UpTo` 包进 `tea.Cmd` 异步执行，完成后用一个 msg 回到 Update 更新状态；执行期间显示 spinner。
- **注**：此条涉及 summarizer 网络调用，但 bug 本质是「UI 线程同步阻塞」而非网络质量——属于本次范围。
- **验证**：rewind 面板触发 summarize 期间 TUI 不冻结、有进度提示。

---

## Cluster C — 内置工具

> 涉及文件：`internal/tool/builtin/`（preview.go、editfile.go、multiedit.go、fuzzymatch.go、delete_range.go、grep.go、glob.go、workspace.go）、`internal/agent/agent.go`

### C1【high】edit_file / multi_edit 的 Preview 与 Execute 匹配逻辑已分叉

- **位置**：`internal/tool/builtin/preview.go:74`（editFile.Preview 用 `strings.Count` 精确匹配）、`:117`（multiEdit.Preview 同）；vs Execute 路径 `editfile.go:59` / `multiedit.go:107` 已改用 `findUniqueMatch`（fuzzymatch.go，精确失败后降级「忽略行尾空白/缩进」）。防漂移测试 `preview_test.go` 只覆盖精确用例，模糊路径无断言。
- **现象**：模型复述的 old_string 带行尾空格/缩进差异时（这正是 fuzzymatch 为 flash 加的容错场景）→ Preview 返回 "old_string not found" → agent.go:812 的 `perr == nil` 不成立 → **onPreEdit checkpoint 不快照该文件**，但 Execute 随后模糊匹配成功真改了文件 → 用户 rewind 该轮时这次改动还原不掉（静默数据不一致）；同时审批卡的 FileDiff 也缺失。次要：重叠匹配时反向分叉（`strings.Count` 判可行、`findUniqueMatch` 判 not-unique）。
- **根因**：Execute 升级了匹配算法，Preview 没跟上。preview.go 头部注释还声称「Preview mirrors Execute exactly」。
- **修复**：把 editFile.Preview 和 multiEdit.Preview 改为**复用 `findUniqueMatch`**（与 Execute 同一函数），返回相同的字节区间和 note。确保 Preview 算出的 diff 与 Execute 实际改动逐字一致。
- **验证**：**关键**——给 `preview_test.go` 补模糊路径的断言（带行尾空格差异的 old_string，Preview 与 Execute 都成功且 diff 一致；重叠匹配两者都失败）。这个测试是防止再次漂移的护栏，必须加。

### C2【medium】delete_range 删除全文所有裸 `\r` 并统一行尾

- **位置**：`internal/tool/builtin/delete_range.go:100`（`strings.ReplaceAll(original, "\r", "")`）、`:134`（`strings.Join(keep, lineSep)`）
- **现象**：为匹配锚点先把全文 `\r` 删掉再按 `\n` 切行，最后用单一 `lineSep` 重新 join 写回 → ① 行内裸 `\r`（测试 fixture、字符串字面量、旧 Mac 片段）被静默删除；② 混合行尾文件被整体归一，被删区间外每行都被改写。即使只删 2 行也触发。
- **修复**：不要全局 `ReplaceAll("\r","")`。改为：保留原始行尾，按 `\n` 切分时保留每行的 `\r`（或用能保留行尾的分割方式），只删除目标区间的行，其余行**原样**拼回。锚点匹配时做局部归一化比较，但写回用原始内容。
- **验证**：构造一个 CRLF 文件 + 一个行内含裸 `\r` 的文件，delete_range 删中间几行 → 确认未被删的行字节完全不变、行尾不被统一。补测试。

### C3【medium】grep 对 GB18030 等文件提前停止扫描时泄漏 goroutine + io.Pipe

- **位置**：`internal/tool/builtin/grep.go:119`（非 UTF-8 走 `io.Pipe`：`go func(){ pw.Write(peek); io.Copy(pw,f); pw.Close() }()`）；`searchFile` 两条提前返回：命中 200 上限 `return io.EOF`(`:144`)、扫描中遇 NUL `return nil`(`:139`)——都不读 reader、不关 `pr`，写端 goroutine 永久阻塞在 `pw.Write`（io.Pipe 无缓冲）。
- **现象**：对含 GB18030 文件的目录 grep 且命中上限或文件 8KB peek 后出现 NUL → 每次泄漏一个 goroutine + pipe，进程生命周期累积。中文教学项目环境 GB18030 不罕见。
- **修复**：提前返回前关闭读端，让写端 goroutine 的 `pw.Write`/`io.Copy` 拿到 `ErrClosedPipe` 而退出。最干净的做法：在创建 pipe 的作用域用 `defer pr.Close()`（`transform.NewReader(pr,…)` 包了 pr，但底层 pr 仍需关）；或把 `src` 在函数退出时统一 close（给 `searchFile` 末尾加 `defer`，对实现了 `io.Closer` 的 src 调用 Close）。确保**任何**返回路径都关 pr。
- **验证**：写一个测试：构造 GB18030 文件命中超 200 次，grep 后用 `runtime.NumGoroutine()` 前后对比确认无泄漏（留足调度时间）。

### C4【medium】工作区模式下 glob 的「裸文件名递归回退」永远失效

- **位置**：`internal/tool/builtin/glob.go:45` + `:62`，配合 `workspace.go:76`（`resolveIn`）
- **现象**：`p.Pattern = resolveIn(g.workDir, p.Pattern)` 把相对 pattern 变绝对路径（必含 `/`），随后回退条件 `len(matches)==0 && !strings.ContainsAny(p.Pattern, "/\\")` 永远 false → 文档承诺的「只知文件名时全树搜索」(`**/<pattern>`) 在所有 Workspace 绑定实例（桌面前端、ACP）一律不触发，直接返回 "(no matches)"。模型用裸文件名 glob（如 `main.py`）找不到非根目录文件 → 多烧轮次。
- **修复**：在 `resolveIn` 改写 pattern **之前**先判断原始 pattern 是否为裸文件名（不含路径分隔符），把这个判断结果保存下来，回退逻辑用「原始 pattern 是否裸文件名」而非改写后的绝对路径来决定是否触发 `**/` 递归搜索。
- **验证**：工作区会话里 glob 一个深层目录下的裸文件名 → 能命中。补测试。

### C5【low】truncateToolOutput 边界可重叠且 elided 字节数为负

- **位置**：`internal/agent/agent.go:904`（`keep = limit/2`，`snapToRuneBoundary` 把 head 右界外推、tail 左界外推各最多 +3 字节）
- **现象**：`len(s)` 略超 limit（如 limit+1）且边界落在多字节字符中间时，head 与 tail 区间重叠（中段重复），`omitted = len(s)-len(head)-len(tail)` 为负，通知显示 "truncated -5 of N bytes"。中文输出概率不低，影响仅几字节重复+提示数字错。
- **修复**：截断前先判断 `if len(s) <= limit { return s }`（已有的话检查阈值）；计算 head/tail 后加保护：`if headEnd >= tailStart { return s }`（重叠就不截断，直接返回原文）；`omitted` 用 `max(0, …)`。
- **验证**：构造长度在 `(limit, limit+6]` 且边界为中文的输出，确认不重复、提示数字非负。补测试。

---

## Cluster D — 配置持久化

> 涉及文件：`internal/config/render.go`、`internal/config/config.go`、`internal/control/controller.go`

### D1【medium】RenderTOML 丢失 section → 任何一次存配置静默重置 codegraph/lsp/tools.search/plugin tier

- **位置**：`internal/config/render.go`（全文）。已确认 `RenderTOML` 不渲染 `[codegraph]`、`[lsp]`、`[tools.search]`，也不写 `PluginEntry.Tier`（plugins 渲染段在 `:232`）。保存路径众多：`/mcp add`、`/mcp remove`(controller.go:1215/1343 → `cfg.Save()`)、`/effort`、desktop 设置面板。
- **现象**：任一次保存后——`codegraph.enabled=false` 丢失 → 下次启动 codegraph 又默认开启（触发 ~45MB 自动下载）；`[lsp]` enabled/servers 丢失；`tools.search.engine`/`rg_path` 丢失；插件 `tier="eager"` 丢失 → eager 插件降级 lazy。
- **修复**：
  1. 通读 `config.go` 的 `Config` 结构体，列出**所有**应持久化的字段。对照 `render.go` 当前输出，补齐 `[codegraph]`、`[lsp]`、`[tools.search]` 三个 section 的渲染（含各自的 enabled / servers / engine / rg_path 等字段，参照现有 section 的写法和注释风格）。
  2. plugins 渲染段补上每个 `PluginEntry.Tier` 的输出（仅当非默认值时写，参照其它字段的 `if 非默认 { Fprintf } else { 注释示例 }` 模式）。
  3. **加一个回归测试**：构造一个把这些字段都设为非默认的 `Config` → `RenderTOML` → 重新 `Load` → 断言字段一字不差地回来（round-trip 测试）。这是防止以后再漏字段的护栏。
- **验证**：设 `codegraph.enabled=false` → 跑 `/effort` → 重启 → codegraph 仍关闭、无下载。round-trip 测试绿。

### D2【medium】`.mcp.json` 的服务器条目被永久复制进 onecreat.toml

- **位置**：`internal/config/config.go:505`（Load 把 .mcp.json 条目合并进 `cfg.Plugins`，无来源标记）+ `internal/control/controller.go:1215`（AddMCPServer：`config.Load()` → `cfg.Save()`）、`:1343`（RemoveMCPServer 同）
- **现象**：`Load()` 把 `.mcp.json` 服务器合并进 `cfg.Plugins` 后，Add/Remove 调 `cfg.Save()` 把**所有** plugins（含来自 .mcp.json 的）写进 onecreat.toml。之后 TOML 副本在名字冲突时优先，用户改 `.mcp.json` 被静默遮蔽——与 controller.go:1333 注释（".mcp.json 不是我们能编辑的文件，下次启动会回来"）自相矛盾。
- **修复**：给 `PluginEntry` 加来源标记（如 `source` 字段：`"toml"` / `"mcp.json"`，**不序列化**到 TOML，仅运行时用）。`Load` 合并 .mcp.json 时标记来源。`RenderTOML`/`Save` 跳过 `source=="mcp.json"` 的条目，不写进 onecreat.toml。
- **验证**：项目根放 `.mcp.json` → 执行 `/mcp add` 加另一个 server → 检查 onecreat.toml **不含** .mcp.json 里的 server；改 .mcp.json 仍生效。

---

## Cluster F — 硬件 MCP（非密码类）

> 涉及文件：`cmd/reasonix-hardware-mcp/main.go`
> **注意**：本文件工作区有一处与本任务无关的旧未提交改动，别卷进 commit。

### F1【high，安全】ESP-IDF 本地回退路径命令注入（`bash -lc` + `strconv.Quote`）

- **位置**：`cmd/reasonix-hardware-mcp/main.go:3351`（`shellJoin` 用 `strconv.Quote` 生成**双引号**字符串），在 `:3624`（`ESPIDFShellScript`）拼进脚本，`:3310`（`exec.CommandContext(ctx,"bash","-lc",script)`）执行。`idf.py` 的 `target`/`port` 参数来自模型/工具输入（`runESPIDF` → `runLocalESPIDFCommand`），schema 未 enum 限制。
- **现象**：`strconv.Quote("esp32$(touch /tmp/PWNED)")` → `"esp32$(touch /tmp/PWNED)"`，bash 在双引号内仍执行 `$(...)`/反引号 → 任意命令执行。触发分支：`esp_idf_run`（或 `hardware_project_validate` 的 ESP-IDF 路径）在 `idf.py` 不在 PATH、配置了本地 ESP-IDF（`REASONIX_ESP_IDF_PATH`）时的回退路径，且 `target`/`port` 含 `$(...)`。
- **修复**：把进入 `bash -lc` 的所有参数改用 POSIX 单引号转义——**复用本文件已有的 `shellArg`**(`:3668`，单引号包裹 + `'\''` 转义)，而不是 `strconv.Quote`。即 `shellJoin` 用于「喂给 bash -lc」的场景时，每个 arg 走 `shellArg`。检查 `shellJoin` 的所有调用点，区分「展示用」（可保留可读形式）和「真喂给 shell 执行」（必须 `shellArg`）。
- **验证**：构造 `target="esp32$(touch /tmp/PWNED_TEST)"` 走本地回退 → 确认 `/tmp/PWNED_TEST` **未被创建**。补一个转义单元测试（`shellArg` 对 `$()`、反引号、`;`、`&` 的输出）。

### F2【medium】monitor 超时不杀进程组 → 孤儿进程占住串口阻塞后续烧录

- **位置**：`cmd/reasonix-hardware-mcp/main.go:3249 / :3278 / :3310`（均 `exec.CommandContext`，无 `SysProcAttr.Setpgid`、无自定义 `Cancel`）。monitor 处理器设计上总是命中超时（`runESPIDF` monitor `:2918`、`runPlatformIO` monitor `:2873`）。
- **现象**：超时时 Go 只 SIGKILL 直接子进程，不杀进程组。`idf.py monitor` 会 fork `esp_idf_monitor` 子进程；杀 `idf.py` 留下 grandchild 占着串口，阻塞后续 flash。本地回退路径（`bash -lc … exec idf.py monitor`）同形。
- **修复**：参照本仓库 bash 工具已有的 `setKillTree`（`Setpgid: true` + 超时时对 `-pgid` 发 SIGKILL）。给这些 `exec.Cmd` 设 `SysProcAttr{Setpgid: true}`，并用自定义 `Cancel`（或 context 超时回调）杀整个进程组。`arduino-cli monitor` 是单进程不泄漏，可不改但加了也无害。
- **验证**：跑一次 `esp_idf_run action=monitor` 超时 → `ps` 确认无 `esp_idf_monitor` 孤儿 → 立刻再跑一次 flash 能拿到串口。

### F3【low】`arduino_monitor_sample` 传 seconds=0 → 阻塞满 90s

- **位置**：`cmd/reasonix-hardware-mcp/main.go:2835`（`runArduinoMonitor` 把 `time.Duration(seconds)*time.Second` 当 timeout）→ `:3240`（`runCommandText` 见 timeout `<=0` 重置为 `defaultTimeout` 90s）。`arduino-cli monitor` 不自退 → 阻塞满 90s。
- **修复**：`runArduinoMonitor` 对 `seconds <= 0` 给一个合理默认（如 3~5 秒采样），或显式拒绝非正值并返回错误提示。不要让它落进 90s 兜底。
- **验证**：`arduino_monitor_sample seconds=0` → 几秒内返回，不卡 90s。

---

## Cluster E — provider / plugin / serve / 其它（low，可最后做）

> 涉及多个包，彼此独立。

### E1【medium】Anthropic 零参数 tool call 产出空 Arguments → 下游 JSON 解析失败

- **位置**：`internal/provider/anthropic/anthropic.go:342`（`content_block_start` 建 `ToolCall{ID,Name}`，`Arguments=""`，只靠 `input_json_delta` 追加；空输入 tool_use 不发 delta）→ `content_block_stop` 发的 `Arguments==""` → 下游 `executeOne` 把空串交给 `json.Unmarshal` 报 `unexpected end of JSON input`。同文件 `buildRequest:241` 在**重放**时已把空 input 替换成 `{}`，说明作者知道约束，但发射路径漏了。
- **修复**：在 anthropic provider 发出 `ChunkToolCall` 前，若 `Arguments` 为空串则置为 `"{}"`（在 `content_block_stop` 组装完整 ToolCall 处归一化）。
- **验证**：走 anthropic provider 调一个无参工具 → 正常执行不报 JSON 错。

### E2【medium】stdio plugin 的 `wait()` 无超时 → 回合级死锁

- **位置**：`internal/plugin/transport_stdio.go:401`（`withStderr → wait() → cmd.Wait()` 无 timeout/ctx），结合 `call:347` / `write:390`。
- **现象**：MCP 子进程关闭自己的 stdin（write 得 EPIPE）或 stdout（readLoop 得 EOF）但进程不退出时，`Wait()` 永久阻塞，且 `call` 还持 `callMu` → 该 plugin 所有后续调用挂死，ESC 取消回合也救不回，只能杀整个会话。
- **修复**：`withStderr`/`wait` 包一个超时（如 `context.WithTimeout` 几秒）——超时后强制 `cmd.Process.Kill()` 再返回。确保任何失败路径不会无限期持 `callMu`。
- **验证**：模拟一个关 stdio 但不退出的 fake MCP server → 确认调用超时返回、不死锁、不卡 callMu。

### E3【low-medium】stdio readLoop 丢弃 server→client 请求（含 ping），从不应答

- **位置**：`internal/plugin/transport_stdio.go:311`（任何带 `method` 的行直接 `continue`）。
- **现象**：JSON-RPC/MCP 要求 client 响应 server 的 `ping`；不答的话部分 server 判定 client 死亡而自退，表现为 plugin「莫名」断开。
- **修复**：至少识别 `ping` 请求并回一个空结果响应（`{"jsonrpc":"2.0","id":<id>,"result":{}}`）。其它不支持的 server 请求（roots/sampling 按设计忽略）可回 method-not-found 错误响应而非静默吞。
- **验证**：连一个会发 ping 的 MCP server，长连接不被动断开。

### E4【low】Coordinator.plan 失败时残留孤立 user 消息

- **位置**：`internal/agent/coordinator.go:71`（`plannerSess.Add(user)` 在 Stream 前；流式中途 `ChunkError` 返回时 assistant 未写入，user 留下 → 下次 Run 再 Add user → planner 会话连续两条 user）。
- **修复**：plan 流式失败时回滚刚加的 user 消息（或改为成功拿到 assistant 回复后再一起提交）。
- **验证**：模拟 planner 流式中途出错 → 确认 planner 会话不残留孤立 user。

### E5【low】diff 仅「末尾换行增删」渲染成完全无变化

- **位置**：`internal/diff/diff.go:51` / `:112`（`splitLines` 剥离换行后做 Myers，`"a\nb\n"` vs `"a\nb"` 得全 equal → `Added/Removed=0`、`Diff=""`，但 `oldText!=newText` 文件确实变了）。
- **现象**：审批卡/changed-files 显示「无变化」，用户批了个看不见的修改。
- **修复**：在 diff 构建末尾，若 `oldText != newText` 但 ops 全 equal（纯 EOL 差异），显式标注一行「末尾换行符变化」（或在 `\ No newline at end of file` 逻辑覆盖到这种纯末尾换行场景）。
- **验证**：write_file 唯一差异是末尾换行 → 审批卡显示有变化。

### E6【low】skill frontmatter `name:` 覆盖文件名后，索引里的名字 `Read()` 解析不到

- **位置**：`internal/skill/skill.go:311`（parse 允许 `name:` 覆盖 stem，进系统提示索引）vs `:245`（`Read` 只按文件名找 `<name>/SKILL.md` 或 `<name>.md`）。
- **现象**：frontmatter `name` 与文件名 stem 不一致时，模型照索引调 `run_skill({name})` → `Read(name)` 按文件路径找不到 → "unknown skill"。
- **修复**：二选一——① `List()` 进索引时用「文件名 stem」作为可调用的 name（忽略 frontmatter name 或仅作显示）；② `Read()` 支持按 frontmatter name 反查（建一个 name→path 映射）。推荐 ②，保留 frontmatter 覆盖能力。
- **验证**：造一个 frontmatter name ≠ 文件名的 skill → 模型能成功 run_skill。

### E7【low】jobs.Kill 返回 true 后任务仍可能被标记 Done

- **位置**：`internal/jobs/jobs.go:202`（Kill 锁内置 `status=Killed`，**释放锁后**才 `j.cancel()`）vs `:125`（run goroutine 正常返回时覆写 `status=Done`）。窗口期竞态。
- **现象**：调用方拿到 `Kill()==true` 但 Wait/Output 报 Done（无数据损失，仅状态不一致）。
- **修复**：run goroutine 写最终 status 前检查是否已被置为 `Killed`（`if j.status == Killed { return }`，持锁判断），不要覆写。
- **验证**：构造 kill 与自然完成竞态，确认 Killed 不被覆写。

### E8【low】serve wire.go 漏 MCPSurfaceReady → SSE 发出 `"kind":""` 帧

- **位置**：`internal/serve/wire.go:84`（`kindNames` 映射止于 `ToolProgress`，缺 `MCPSurfaceReady`，定义在 `internal/event/event.go:75`）。
- **现象**：serve 模式下 MCP server 完成 phase B 时前端收到 `{"kind":"","text":"…prompts ready…"}` 无法识别，静默丢弃（/mcp 状态不刷新）。
- **修复**：`kindNames` 补上 `MCPSurfaceReady` 的映射字符串。
- **验证**：serve 模式连带 prompts 的 MCP server → 前端收到正确 kind。

### E9【low】serve /history 空会话返回 JSON `null` 而非 `[]`

- **位置**：`internal/serve/serve.go:272`（`var out []msg` 为 nil 时编码成 `null`；对照 `:608` sessions 端点已做 `nil→[]` 防护）。
- **现象**：JS 客户端 `for (const m of await res.json())` 对 `null` 抛 TypeError。
- **修复**：照 sessions 端点：`if out == nil { out = []msg{} }`（或 `make([]msg, 0)`）。
- **验证**：对空会话调 /history → 返回 `[]`。

### E10【low】OpenAI provider tool call ID 合成时机 → partial 卡片对不上

- **位置**：`internal/provider/openai/openai.go:343`（start 发射时 `cur.ID==""`）vs `:364`（流结束才合成 `call_%d`）。仅影响按 index 流式、不带 id 的兼容网关（DeepSeek 官方带 id，不触发）。
- **现象**：`ChunkToolCallStart` 带空 ID → agent 发 `ToolDispatch{Partial:true, ID:""}`，流结束后完整卡片 ID 变 `call_0/1` → 前端无法 merge，悬挂 partial 卡片；多并行 call 时空 ID 互不可分。
- **修复**：把 ID 合成提前到 `ChunkToolCallStart` 发射时（按 index 生成稳定 `call_<index>`），start 与最终 ToolCall 用同一 ID。
- **验证**：用不带 id 的兼容网关跑并行 tool call → 前端卡片正确合并（DeepSeek 主路径不受影响，回归即可）。

---

## 附：审查中确认「不是 bug」的可疑点（不要动）

避免执行者误判，以下已逐一核实**正确**，请勿"修复"：
- `executeBatch` 并行只对 ReadOnly 工具开放，`todo_write`/`complete_step` 已排除出并行批，顺序依赖成立。
- `evidence.Ledger` 全部带锁；storm breaker 状态机的重置逻辑完整；session「单写者 + Snapshot」自洽。
- bash 工具 `setKillTree`（Setpgid + 负 pid SIGKILL）+ 120s 超时 + WaitDelay，无进程泄漏（F2 是 hardware-mcp 没用这套，不是 bash 工具的问题）。
- `confine.go` 的 `realPath`/`within` 防 symlink 逃逸正确；`compact.go` 的 `planCompaction` 负索引边界有短路保护。
- `notebookedit.go:191` 的切片拼接无别名覆盖；jobs 的 Wait/Output 偏移加锁正确。
- 前端各处 `JSON.parse` 均有 try/catch；事件监听器 cleanup 到位；`requestApproval` 的 channel 是 buffered(1) + ctx.Done 清 map，无泄漏。
- `boot.Build` 的 eager 过滤、`mergeTOMLPlugins` later-wins 合并、`fixCJKEmphasis` 判定均正确。

---

## 完成标准（goal-driven 总验收）

全部 cluster 改完后，必须满足：
1. `go build ./...` + `go vet ./...` 通过。
2. `go test ./...` 全绿（含本文档要求新增的回归测试：C1 preview 模糊路径、C2 delete_range 行尾、C3 grep 无泄漏、C5 截断边界、D1 config round-trip、F1 shellArg 转义、B1 压缩后 rewind）。
3. 前端 `pnpm tsc --noEmit && pnpm build` 通过。
4. A 区手动验收：多标签并行下，改设置 / 后台审批 / 切回正在跑的标签 / 跨标签 resume 四个场景都不再丢数据或卡死。
5. 逐条在本文档对应项后标注「已修复 / 跳过（原因）/ 不确定（细节）」，交回人工复核。

---

## 执行结果（Opus 4.8 — 2026-06-11）

**总验收**：`go build ./...` + `go vet ./...` 通过；`go test ./...`（根模块）+ desktop 模块
`go build/vet/test` 全绿（含全部新增回归测试）；前端 `pnpm tsc --noEmit` + `pnpm build` 通过。
每个 cluster 一个 commit checkpoint（未 push）。下面逐条标注。

### Cluster A — 多标签真并行（整簇一起改，1 个 commit）

- **A1**【已修复】`rebuild()` 进锁前取旧 ctrl + 发起标签 targetTab/sink,锁外 build,回锁写回
  发起标签 tabRuntime +(若仍活动)活动镜像。`settings_app.go` Settings()/SetDefaultModel 的
  无锁读写也一并加锁。
- **A2**【已修复】应答端:`App.Approve/AnswerQuestion` 加 `tabID` 按标签路由(+前端 bridge/
  useController 带上 tabId)。展示端:`Controller.PendingApprovals/PendingAsks` + `App.PendingPrompts`,
  切回标签时 `loadSessionData` 补显弹窗;另加「非活动标签轻量订阅 + 侧栏待审批红点」。
- **A3**【已修复】`Meta` 暴露 `running/planMode`,切回标签从真值恢复 running(`restoreRunning`);
  history reducer 改为 items 非空时不整体覆盖,保留已流入 live 事件。
- **A4**【已修复】新增 `sessionPathsInUse`;`DeleteSession`/`ResumeSession` 遍历所有标签 SessionPath,
  跨标签占用则拒绝并明确报错。
- **A5**【已修复】`SetModel` 入口固定 `targetTab` + 该标签 sink,build 完成只回写发起标签,
  活动镜像仅在仍是发起标签时更新。
- **A6**【已修复】`tabRuntime.closed`;`CloseTab` 置位,`buildTab` 完成时若已关闭则 `ctrl.Close()`
  且不发 ready。
- **A7**【已修复】`shutdown` 遍历所有标签 Snapshot+Close;`ApplyUpdate` 经 shutdown 一并修复。
- **A8**【已修复】`SetPlanMode/SetBypass/SetCoachMode` 按 tabID 路由 + `tabRuntime` want 状态
  (nil ctrl 不再静默吞),`buildTab` 装配后施加;pill 读 Meta 真值,新标签继承当前门控。
- **A9**【已修复】`SubmitDisplay` 取一次 ctrl 局部变量消除 TOCTOU;`Add/Remove/Retry/SetMCPServerEnabled`
  统一 RLock 取 ctrl。
- **A10**【已修复】`sessions.go` 加包级 `sidecarMu`,三个侧车文件读改写整段持锁。
- 【不确定 / 待人工】完成标准 #4 的「A 区手动验收」需在真实 Wails 桌面 GUI 下点测,
  本环境无法启动 GUI;后端逻辑由 build/vet/test 覆盖,前端由 tsc/build 覆盖,**四个场景的
  实机点测请人工补做**。

### Cluster B — 压缩 / checkpoint（1 个 commit）

- **B1**【已修复】`Agent.SetOnCompact` 回调 + `Controller.InvalidateCheckpoints`,构造时连线;
  `compact()` 重写日志后触发清空 cpBound。补回归测试(压缩后 rewind 报失败)。
- **B2**【已修复】`Controller.Compact/NewSession` 加 running 守卫(busyNotice);
  `compact/SummarizeFrom/UpTo` 改用 `Session.Snapshot()` 持锁读。
- **B3**【已修复】新增 `Session.Truncate`(持锁截断 + 越界返回 false);Rewind 改用它。
- **B4**【已修复】Rewind 截断失败时报失败 Notice + 返回 error，不再假成功。补回归测试。
- **B5**【已修复（注）】`summ-from/summ-upto` 包进 tea.Cmd 异步执行 + 执行前发 `summarizing…`
  进度 notice。**注**:未为 rewind 面板 summarize 加完整 CompactionStarted/Done 占位卡(那需在
  agent 层为 SummarizeFrom/UpTo 新增事件),只做了异步化(修掉 UI 冻结这一本质问题)+ 文字进度提示。

### Cluster C — 内置工具（1 个 commit）

- **C1**【已修复】editFile/multiEdit 的 Preview 改用与 Execute 同一个 `findUniqueMatch`。
  补 preview_test 模糊成功路径 + 不唯一一致性断言。
- **C2**【已修复】delete_range 改「保留行尾的行记录」逐字节拼回,不再全局删 \r / 统一行尾。
  补 CRLF 外行内裸 \r + 混合行尾回归测试。
- **C3**【已修复】grep 解码 pipe 提前返回前 `defer pr.Close()`。补 GB18030 命中上限的 goroutine
  无泄漏测试(已验证去掉 fix 则 fail:before=2 after=22)。
- **C4**【已修复】glob 在 resolveIn 之前判定裸文件名,回退用原始判断 + dir/base 重建递归 pattern。
  补 Workspace 绑定 + cwd 两种裸名回退测试。
- **C5**【已修复】truncateToolOutput 用下标版 rune 边界 + `headEnd>=tailStart` 重叠保护。补边界测试。

### Cluster D — 配置持久化（1 个 commit）

- **D1**【已修复】RenderTOML 补 `[codegraph]`/`[lsp]`(含 `[lsp.servers.*]`)/`[tools.search]` +
  插件 `tier`。扩展 round-trip 测试覆盖这些字段。
- **D2**【已修复】`PluginEntry.Source` 运行时标记(`toml:"-"`),loadMCPJSON 标 "mcp.json",
  RenderTOML 跳过。补「.mcp.json 插件不写入 toml」测试。

### Cluster F — 硬件 MCP（1 个 commit；未碰本文件那处无关旧改动）

- **F1**【已修复，安全】喂给 `bash -lc` 的 exec 行从 `shellJoin`(strconv.Quote)改为 `shellJoinArgs`
  (单引号转义);`shellJoin` 加注释标为「仅展示」。补 shellArg 中和 `$()`/反引号/`;`/`&`/`|`
  + ESP-IDF 脚本恶意 arg 单引号化测试(已验证去掉 fix 则 fail:恶意 arg 进双引号)。
- **F2**【已修复】新增 `setKillGroup`(Setpgid + 负 pid SIGKILL + WaitDelay,按平台分文件),
  作用于三个 timeout 命令 runner。
- **F3**【已修复】`runArduinoMonitor` 对 `seconds<=0` 回退默认采样秒数,不再落进 90s 兜底。
- 【已遵守】`cmd/reasonix-hardware-mcp/main.go` 那处「网络板卡探测」旧未提交改动:开工前已 stash,
  提交本簇后再 pop,**未卷进 commit**(仍留在工作区未提交)。

### Cluster E — provider / plugin / serve / 其它（1 个 commit）

- **E1**【已修复】anthropic 零参 tool_use 空 Arguments 归一化 "{}"。
- **E2**【已修复】stdio `wait()` 加超时强杀,避免持 callMu 死锁。
- **E3**【已修复】stdio readLoop 应答 server 请求:ping 回空结果、其余 method-not-found。
- **E4**【已修复】Coordinator.plan 成功才提交 user+assistant,失败不留孤立 user。
- **E5**【已修复】diff 纯末尾换行差异显式标注。补回归测试。
- **E6**【已修复】skill Read 增加按 frontmatter name 反查的回退。
- **E7**【已修复】jobs run goroutine 写终态前检查 Killed 不覆写。
- **E8**【已修复】serve + desktop wire 补 `MCPSurfaceReady` 映射(desktop 那条是同源 bug,顺手补)。
- **E9**【已修复】serve /history 空会话返回 `[]`。
- **E10**【已修复】openai 在 ChunkToolCallStart 处合成稳定 `call_<index>` id。

### ⚠️ 需人工知悉的一处操作失误（已自行补救）

工作区开工前**另有一处与本任务无关的「turn 结束 todo 对账提醒」WIP**,横跨
`internal/agent/agent.go` + `internal/evidence/evidence.go` + `internal/agent/todo_reconcile_test.go`。
本文档只点名了 main.go,我对 agent.go **未先 grep 既存改动**,在 Cluster B `git add agent.go`
时把它一并提交了进去(疏忽)。这导致单独 checkout Cluster B 该 commit 无法编译
(`evidence.LatestTodos` 未定义)。补救:已用一个**单独标注的 commit**(`acc6dc5c`)补上
evidence.go + test 半边,使 **HEAD 可独立编译、全测试通过**。

代价:① 该 todo-reconcile WIP 现在是「已提交」状态(开工时是未提交);② Cluster B~F 的中间
commit 单独 checkout 仍不可编译(LatestTodos 只在 acc6dc5c 才出现),只有 HEAD 可编译。
如需把它从 Cluster B 干净剥离、恢复成未提交 WIP,可在本地 `git rebase -i f79577c0` 编辑 B
commit(本 harness 不支持交互式 rebase,故未自动处理)。已留安全分支 `backup-before-evidence-fix`。
