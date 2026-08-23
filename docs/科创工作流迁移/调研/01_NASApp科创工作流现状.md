> 归档自 2026-08-23 调研会话,只读事实快照,以当日代码为准
> 勘误:§2.1④ 材料表实为 25 种(v17 核实),非 24。

# NASApp「科创工作流」现状分析（迁移到 OneCreat 的前置调研）

- 调研对象：`/Users/localwork/04_project/NASApp`（git 仓，分支 `main`，HEAD `4d3e5544`，2026-08-23）
- 调研方式：只读。未修改、未提交任何文件。
- 关联但**不在本仓**的三处真源（也读了，因为工作流跨这三处才完整）：
  - Mac 文件真源 `/Users/localwork/课题/`（`00_产题评题/`、`01_在研项目/`）
  - Mac 受控脚本仓 `/Users/localwork/课题/99_工作区/scripts/`（`project_sync.py` / `establish_project.py` / `topic_index.py` / `refresh_board.py` …）
  - Skill 仓 `/Users/zunwei/code/oneup-skills/`（`competition-toolkit` / `hardware-toolkit` 等 5 个插件、40+ skill）
- 教师平台仓 `/Users/zunwei/system/teacher` **未读**（本次范围外，只按 NASApp 侧文档描述其契约）。

> 一句话结论：NASApp 里的「科创工作流」不是一个模块，而是一条**跨 4 个物理位置（Mac / NAS / Mac mini / 教师平台）、由一份 JSON 注册表定义、由 40+ 个 Claude-Code skill 执行、以文件位置为状态机**的流水线。它的"方法论"资产集中在三处：`brain/registry/workflow_registry.json`（阶段与材料的机器契约）、oneup-skills 各 SKILL.md（每个环节怎么做的知识）、以及两份人读正本 README（`00_产题评题/README.md`、`01_在研项目/README.md`）。剩下的绝大部分代码是**把这三处粘到 NAS/iOS/Mac 上的胶水**。

---

## 0. 系统全景（先看这张图，后面所有细节都挂在它上面）

```
┌───────────────────────────────────────────────────────────────────────────┐
│  老板的 Mac(工作机)  ── 文件真源                                            │
│  /Users/localwork/课题/                                                    │
│    00_产题评题/{产题,评题,准则,_存量文件夹}   ← 题的来源正本                  │
│    01_在研项目/P26X-NNN_短名/{01..07 七目录}+project.json  ← 项目实体正本     │
│    01_在研项目/_未立项/<题名>_<组别>_<日期>/                                  │
│    99_工作区/scripts/  establish_project.py / project_sync.py /            │
│                        assign_project.py / reline_project.py /            │
│                        refresh_board.py → topic_index.py                  │
│  常驻:mac-agent/macfs_agent.py :8787(动作白名单,argv 数组,shell=False)      │
└───────────┬───────────────────────────────────────────────────────────────┘
            │ HTTP + 共享 token(~/.macfs_token) ；只跑 ACTIONS 白名单
            ▼
┌───────────────────────────────────────────────────────────────────────────┐
│  QNAP NAS 192.168.6.131 ── 仓库 + 柜台                                     │
│  Docker 容器 photo-uploader = web/main.py(FastAPI, 16939 行, 单文件)        │
│    :8080  内网 / https://nas.weizunxy.com 公网(Cloudflare→VPS→反向隧道)     │
│  数据:                                                                     │
│    /share/项目管理/00_项目总库/<编号>_<短名>/  七目录镜像(NAS 侧项目实体)      │
│    /share/CACHEDEV1_DATA/.appdata/topic_board/                            │
│        topic_index.json(v16 题索引) / board_data.json / pack_state.json / │
│        project_codes.json(编号登记簿,发号唯一权威)                          │
│    /share/CACHEDEV1_DATA/.appconfig/.kanban.json  ← P/C/B 三类看板卡        │
│    /share/项目管理/_未立项_入库/  ← 未立项包回流暂存                          │
└───────────┬───────────────────────────────┬───────────────────────────────┘
            │ Bearer CLUSTER_API_TOKEN      │ 会话 cookie
            ▼                               ▼
┌──────────────────────────────┐   ┌──────────────────────────────────────┐
│ M2 Mac mini 192.168.6.158    │   │ 客户端                                │
│ ── 大脑 / 车间                │   │  iOS OneUpTasks(SwiftUI, 主力)        │
│ launchd com.oneup.agent-     │   │  Web /desktop(desktop-more.js)        │
│   cluster  → brain/src/      │   │  Web /(index-v2.html) 手机网页         │
│   server.ts :8799            │   │  Android(复刻,滞后)                    │
│ launchd com.oneup.pi-agent   │   └──────────────────────────────────────┘
│   → pi-daemon/daemon.ts:8798 │
│ 产物:~/agent-cluster/app/     │   ┌──────────────────────────────────────┐
│   产物/<owner>/<项目>/<环节>/  │   │ 教师平台 t.weizunxy.com               │
│   runtime/{jobs,threads,      │   │  Next.js + Supabase(独立仓)          │
│     topic-rules,topic-        │   │  写凭证只在 iOS Keychain              │
│     feedback}.json            │   │  只读 token 在 mini .env(大脑用)      │
│ skill:~/.claude/skills ←      │   └──────────────────────────────────────┘
│   oneup-skills 仓             │
└──────────────────────────────┘
```

关键事实：
- **web/main.py 不生成任何材料**，它是柜台 + 反向代理（`_cluster_call`，`web/main.py:2681`）。
- **brain(agent-cluster) 不认识"竞赛"**，运行层只会"用某个 skill 跑到某个目录"（`brain/src/engine.ts:293` `runSkill`）。业务语义在 `pipeline.ts`，阶段/材料定义在 `registry/workflow_registry.json`。
- **Mac 是唯一能发号建档的地方**，NAS/大脑只能通过 macfs 白名单发起（`mac-agent/macfs_agent.py:317` `ACTIONS`）。

---

## 1. 方法论本身

### 1.1 三套并存的"阶段划分"（这是最容易搞混的地方，迁移必须先厘清）

NASApp 里同时活着 **三套阶段体系**，它们不是同一个东西，各有各的权威源：

#### (A) 项目全流程八段 —— 项目**生命周期**，权威在 P 卡

定义：`web/main.py:6366`
```python
PROJECT_STAGE_NAMES = ["孵化", "立项", "生产", "发布同步", "项目协作", "授课交付", "结题冲刺", "回流归档"]
```
- 值域 1–8，容错在 `_project_stage_int()`（`web/main.py:6369`，3.9 这种非整数直接判 None，不截断）。
- P 板（看板 type=`p`）的八个列 id 固定为 `col_flow_1..8`，**列与 stage 互为镜像**：拖卡改列 → 服务端回写 stage；卡里拨 stage → 服务端换列（`_apply_flow_binding`，`web/main.py:6423`）。
- 铁律：`/api/internal/project-card/upsert`（`web/main.py:6152`）**只给新卡写 stage 初值，绝不覆盖已有值**——手机上老板拨过的段位是权威（`CLAUDE.md:187-188`）。
- 每段的进入/退出条件、产物、涉及的端，被完整写死在 iOS 的一张"八段登高图"数据区里，**这是全仓最接近"方法论正文"的东西**：`ios/OneUpTasks/WorkflowMapView.swift:61-153`。逐段摘录（含它自己标注的"铁律/原则"）：

| LV | 段名 | 一句话 | 涉及端 | 关键铁律（原文） |
|---|---|---|---|---|
| 1 | 孵化 | 选题在课题库里按决策状态逐级晋级 | Mac, NAS | （v16 已废三层晋级，此卡文案是**存量未更新**，见 1.5） |
| 2 | 立项 | 发号+建档一条龙，项目从此有三端通用身份证 | Mac, NAS | 「立项 ≠ 分配」：业务线必选，分配对象可空后补；严禁以学生真名建项目文件夹（`:83-84`） |
| 3 | 生产 | 项目内容在七目录里长出来 | Mac | 「以科创为体、以 AI 为手」：AI 可辅助生成，但**学生必须能逐行解释；数据必须学生亲手采集**（`:95-96`） |
| 4 | 发布同步 | 一条命令把 Mac 真源、NAS 总库、手机 P 卡拉齐 | Mac, NAS | stage 权威在 P 卡，同步只写初值（`:103-104`）；双向不回环——`App产物/` 由 Mac 拉回，Mac→NAS 排除它（`:105-106`） |
| 5 | 项目协作 | 人围着项目转：分配只改元数据 | Mac, NAS | 项目实体永不搬家；换线只能走 `reline_project`，保留编号后三位（`:113-116`） |
| 6 | 授课交付 | 教师平台开课授课，产物按类型回流七目录 | NAS, 教师平台 | 平台 token 只留 iOS Keychain，NAS 后端不持有（`:123-124`） |
| 7 | 结题冲刺 | 材料收口进 07 目录，按赛事规则打包 | Mac, 教师平台 | 金鹏：严格匿名化（无姓名/校名/师名）· 3 分钟视频 · 6 内容点全覆盖（`:138-139`） |
| 8 | 回流归档 | 成果原位沉淀，经验滋养下一轮孵化 | Mac, NAS | 项目文件夹永不搬家、永不手改名（`:146-147`）；山顶注释：「回流归档后 ↺ 循环回 LV.1」（`:186`） |

#### (B) 科创一条链五段 —— **材料生产**流水线，权威在注册表

定义：`brain/registry/workflow_registry.json:13-19`

| id | label | order | kind | App tab | 落点 |
|---|---|---|---|---|---|
| `source` | 产题 | 1 | conversational | 产题 | Mac `课题/00_产题评题`，NAS `/share/项目管理/00_产题评题` |
| `establish` | 立项 | 2 | conversational | 立项 | `00_产题评题` + `01_在研项目/_未立项` |
| `collab` | 协作投放 | 3 | oneclick | 研发 | 桶 `06_项目协作/投放` |
| `platform` | 平台材料 | 4 | oneclick | 研发 | 桶 `02_平台材料` |
| `sprint` | 竞赛提交 | 5 | oneclick | 研发 | 桶 `07_竞赛提交` |

- `kind: conversational` = 有人机对话线程、可 `awaiting_input`、有"建档/未立项"决断；`kind: oneclick` = 一次性生成材料，靠依赖 DAG 编排。这是**整套系统里最重要的一个二分**。
- iOS 首页只展示「产题 → 立项 → 项目看板」三段（`CLAUDE.md:106-107`）；发现页入口卡文案是「产题 → 立项 → 平台材料 → 竞赛提交」（`ios/OneUpTasks/WorkflowHomeView.swift:60`）。

#### (C) 教学法八阶段 / 平台十流程 —— **课堂教学**方法论，权威在 skill

在 `/Users/zunwei/code/oneup-skills/competition-toolkit/skills/lesson-plan-generator/SKILL.md`：
- 教学内容 8 阶段（`:32-40`）：启发→调研→本质→可视→制作→测试→迭代→成果
  1 科技启发与问题定义 / 2 需求调研与验证 / 3 本质分析与方案设计 / 4 方案可视化 / 5 原型制作 / 6 测试验证 / 7 迭代优化 / 8 成果展示
- **教师平台进度轴权威十流程**（`:48-61`，每节教案必须打 `对应流程` 单值标签）：
  1 问题分析 / 2 课题定义 / 3 研究调研 / 4 方案设计 / 5 原型构建 / 6 程序调试 / 7 实验测试 / 8 数据总结 / 9 材料整理 / 10 报告与答辩
- 8→10 的迁移对照表在 `:63-74`。硬要求在 `:76-80`：节标题必须含 `第N节课教案`、N 连续、节数必须等于总课节数（平台靠这个切节）。
- 同一套十一阶段也**硬编码在 NAS 的 C 类看板默认列**里（学生项目看板）：`web/main.py:15398-15399`
  ```python
  "c": ["发现问题","课题定义","通识基础","研究调研","方案设计","原型制作",
        "程序调试","实验测试","数据总结","材料整理","报告答辩"],
  ```
  注意它比 skill 的十流程多一个「通识基础」——**两处不同源，已经漂移**。

### 1.2 题的状态机（v16 拍板：文件位置就是状态）

正本文档：`/Users/localwork/课题/00_产题评题/README.md`（v16，2026-08-17 生效）

```
候选 candidate
  └─ 老板点「立项」 → 立项中 establishing
       ├─ 确认建档   → 已立项 established   → 01_在研项目/P26X-NNN_短名/
       └─ 确认未立项 → 未立项 unestablished → 01_在研项目/_未立项/<题名>_<组别>_<日期>/
```
（`00_产题评题/README.md:53-63`）

进入/退出条件（原文要点）：
- 「进行中的同一题禁止重复发起」——实现在 `brain/src/conversation-threads.ts:160` `findActive()` + `normalizeThreadKey()`，服务端返 409（`brain/src/server.ts:2203-2207`）。
- 「**只有老板在结果页明确点击确认后才能发号建档**。业务线 C/B/G 必填，负责人可后补；不得手工编 P 号或用移动文件代替立项」（`README.md:61`）。代码强制：`brain/src/server.ts:2289` 只允许 `kind==='立项' && status==='succeeded'` 的线程执行 `建档/未立项`。
- 「建档时产题、评题和立项依据**复制**进项目的 `01_立项定题/`，来源区原始 Markdown 保留，不移动、不删」（`:62`）。
- 「未立项需要填写 reason」——`brain/src/server.ts:2331-2334` 强制，并写 `未立项原因.md`（`:2353-2356`）后整包 ingest 到 NAS `_未立项_入库`（`:2361`）。

线程自身的状态机（`brain/src/conversation-threads.ts:7-13`）：
`queued → running → awaiting_input → succeeded / failed / cancelled`
- 深度模式且没带 note 时**先进 `awaiting_input`**（`server.ts:2212, 2224`），收到补充消息才起 job（`:2262-2273`）——"不能把硬停伪装成完成"（`brain/DEVELOPMENT.md:777`）。

### 1.3 角色

| 角色 | 在流程里干什么 | 代码上的体现 |
|---|---|---|
| **创始人/老板（owner）** | 唯一的决策点持有者：立项建档确认、未立项判定、产题准则 confirm、P 卡拨段位、引擎档位选择 | `_is_owner` 判定（大脑完整模式，`brain/src/full-mode.ts:59-60`）；`topic_rules confirm` 只在老板明确说确认后调（`tools.ts:2188-2191`）；系统提示词多处「确认权只在老板」「绝不在老板未确认时替他 confirm」 |
| **老师 / 运营（agent 权限）** | 发起材料生成、上传替代生成、定点修改、上课记录、拍照 | `_require_agent`（`web/main.py:2624`，需 `admin` 或 `agent` 位）；确认卡 `requires_confirmation: true` |
| **学生** | 只是 C 卡档案 + NAS 文件登录账号；**没有工作流权限** | C 卡 `studentUser` 绑 NAS 登录账号（`docs/教务融合规划.md:73-74`）；学生视角只有 NAS `2026上课/<真名>/项目文件/` 软链（`project_sync.py` 第 5 步） |
| **外部协作者（设计师/外聘老师）** | `06_项目协作/投放` 拿件、`交稿` 交件 | `web/main.py` `/api/ext/*` + `external.html`（`web/CLAUDE.md` N 节） |
| **家长** | **系统里完全没有** | 无任何代码落点 |
| **AI** | 见第 4 节。定位是"手"不是"作者" | `WorkflowMapView.swift:95-96`；系统提示词的"轻重路由"与"探讨豁免"（`system-prompt.ts:125,142`） |

### 1.4 教学理念怎么体现在流程里

不是口号，有三处硬约束：

1. **"以科创为体，以 AI 为手"** → 生产段的原则条（`WorkflowMapView.swift:95-96`）："AI 可辅助生成，但学生必须能逐行解释；数据必须学生亲手采集"。
2. **题必须来自学生真实生活** → 写进 v16 正本的"人与 AI 共守边界"第 1 条（`00_产题评题/README.md:82`）："AI 可以辅助发散、评估和整理，但题目必须来自学生真实生活，学生必须能解释选题与验证方案。"
3. **证据链而非说辞** → 见 1.6 的"证据台账"。论文骨架期就把"这一章需要哪些证据"挖成槽位，课堂上采集，结题时装配；**缺素材如实标注，"不许用编造内容填坑"**（`competition-paper-generator/SKILL.md:110`）。
4. **人工把关点被刻意保留** → "论文图不自动嵌、要人工组装"，理由写在 `brain/DEVELOPMENT.md:112`："论文图多、图文编号要严格对应，自动放会错位。这是模型能力天花板，**刻意保留人工把关（也是竞赛该有的审核）**"。实现是生成一份《图表组装说明.txt》（`brain/src/pipeline.ts:200-222`）。
5. **匿名化是金鹏赛制要求，做成了流水线的一道工序**（`匿名化检查` 材料 → `jinpeng-anonymization-checker`，且"只检查不改源文件"，`pipeline.ts:401-407`）。

### 1.5 已知的"文档 vs 代码"漂移（迁移时别照抄错的那份）

- `ios/OneUpTasks/WorkflowMapView.swift:62-73` 的 LV.1 孵化卡仍写「课题库三级晋升 10头脑风暴→20已定待方案→30成熟方案」和 `02_课题库/看板.html`——**v16 已全部退役**（`NASApp/CLAUDE.md:105-111`：「旧 `02_课题库` 三层、晋级动作和四台入口均已退役」）。同段 LV.8 的"选题回流 02_课题库/10_头脑风暴/App产题/"同样过期。
- `brain/README.md` 整体停留在更早的版本（10 种材料、NAS 容器部署、`/console` 页面）；现役事实在 `NASApp/CLAUDE.md` 第五节 v14–v20 与 `brain/DEVELOPMENT.md` §6/§8/§9/§10。
- `web/main.py:15398` 的 C 板十一列 vs skill 的十流程，两处不同源。

---

## 2. 领域模型

### 2.1 实体清单

#### ① 项目 Project（P26 实体）

- **编号** `P26[CBG]?-\d{3}`（`web/main.py:4517` `PROJECT_CODE_RE`；mac 端更严 `^P26[CBG]-\d{3}$`，`macfs_agent.py:93`）。
  - `C`=个人学生 / `B`=机构 / `G`=学校，**三线共用一个后三位序号池**；存量 `P26-NNN` 不迁移（`NASApp/CLAUDE.md:189-191`）。
  - 发号唯一权威：NAS `project_codes.json`（`web/main.py:118`），`/api/internal/project-code/preview` 只读预告、`establish_project` 带 `expected_code` 回验，号变了就拒绝（`tools.ts:1290, 1330-1332`）。
- **身份文件** `project.json`（Mac 项目根目录唯一允许的散文件）。实测样本 `/Users/localwork/课题/01_在研项目/P26C-020_飞鸟志/project.json`：
  ```json
  { "code":"P26C-020", "short_name":"飞鸟志", "full_name":"校园鸟类声纹多样性监测站",
    "student":"常思诚", "line":"C",
    "assignee":{"type":"student","id":"card_7dd63fa21567","name":"常思诚"},
    "needs_outsourcing":false, "stage":3 }
  ```
- **七目录**（Mac 与 NAS `00_项目总库` 同名同构，`01_在研项目/README.md:29-41`）：
  `01_立项定题 / 02_平台材料 / 03_研发工作区 / 04_课堂记录 / 05_上课照片 / 06_项目协作 / 07_竞赛提交`
  - 每个目录"装什么/应放/不应放"的**判例表**在 `01_在研项目/README.md:47-63`——这是一份真正的领域知识，迁移必须带走。
  - 机制目录白名单：`App产物/`（NASApp/大脑生成物隔离，防同步回环）、`平台/`（教师平台回流，只允许在 02/04/05 下）（`README.md:77-80`）。NAS 侧常量在 `web/main.py:136,150-177`。
- **状态**：`stage` 1–8，权威在 P 卡（`.kanban.json` 的 p 类卡），`project.json` 里那份是同步初值。
- **不变量**：项目实体目录**永不搬家、永不手改名**；换业务线只能 `reline_project.py`（保留后三位、五处联动）。

#### ② 课题 Topic（题卡）

- **题卡不是文件，是索引产物**：`topic_index.py:715` `merge_topic_records()` 按归一化题名 key 合并多个来源文件成一张卡。卡结构（`topic_index.py:720-758`）：
  ```
  key(归一化题名) / title / brief / sources[] / group / dates[] / score / star? /
  state: candidate|establishing|established|unestablished /
  files[] / project_code?(established) / unestablished_reason?(unestablished)
  ```
  加上 NAS 侧叠加的 `thread_id / thread_status / thread_mode`（`web/main.py:932-938`）。
- **归一化规则**（三处必须一字不差同源）：NFKC → 小写 → 去空白与全部标点符号类
  - Mac：`topic_index.py:25-31`
  - NAS：`web/main.py:893-899`
  - brain：`conversation-threads.ts:74-79`
- **来源枚举** `SOURCE_TYPES = ("快速产题","深度产题","快速评题","深度评题","存量")`（`topic_index.py:13`）。注意 `单题发散` 的产物 frontmatter 必须写 `深度产题`，否则会被降级成"存量"（`server.ts:1020-1022`）。
- **组别枚举** `小学低/小学高/初中/高中/混合`（`workflow_registry.json:6`，`topic_index.py:12`，`unestablished-path.ts:4`——三处各写一份）。
- **文件命名契约**：`<简要说明>_<组别>_<YYYYMMDD>.md` + 必填 YAML frontmatter `kind/source/group/date/subject/topics`（`workflow_registry.json:5-12`，`00_产题评题/README.md:20-49`）。
- **未立项目录名**：`<题名>_<组别>_<YYYYMMDD>`，字符集收口在 `brain/src/unestablished-path.ts:7-23`（NFKC + 只留 `A-Za-z0-9㐀-鿿_-`，≤100 字），必须与 NAS 的 `UNESTABLISHED_FOLDER_RE` 同口径。

#### ③ 对话线程 ConversationThread（v16 的核心新实体）

`brain/src/conversation-threads.ts:36-60`：
```
id / owner / kind(材料类型) / mode(快速|深度) / subject / group / org? / student? /
topic? / note? / source_files[] / status / job_id? / messages[] /
context_preview[] / outputs[] / decision? / created_at / updated_at
decision = { value: 建档|未立项, status, reason?, job_id?, project_code?, error? }
```
- 存储：单文件 `runtime/conversation-threads.json`，原子写（tmp + rename，`:104-110`），全内存 Map。
- 运行指标（耗时/token）**不写回线程**，响应时从 job 派生（`conversation-metrics.ts:24`）。

#### ④ 材料 Material（工作流节点）

注册表 `workflow_registry.json:20-45`，共 **24 种**。字段：`type / stage / kind / skill / modes / default_mode / default_engine / deps / soft_deps / outputs{naming,dir,app_dir,exts,frontmatter_kind} / platform{file_type,upload_via} / status / app{icon,subtitle}`。

| 阶段 | 材料 | skill | 硬依赖 deps | 平台上传 file_type |
|---|---|---|---|---|
| source | 快速产题 | topic-express | — | — |
| source | 深度产题 | topic-mining | — | — |
| source | 单题发散 | topic-mining(A3) | — | — |
| source | 快速评题 | topic-scoring | — | — |
| source | 深度评题 | topic-scoring | — | — |
| establish | 立项 | idea-to-build | — | — |
| collab | 项目简要说明 | project-brief | 技术方案 | — |
| collab | 3D交互视图 | project-3d-viewer | 技术方案 | — |
| platform | 技术方案 | tech-proposal-generator | — | technical_proposal |
| platform | 辅导手册 | project-tutorial-manual | 技术方案 | tutorial_manual |
| platform | 教案 | lesson-plan-generator | 技术方案 | lesson_plan |
| platform | 老师采集手册 | lesson-plan-generator(第7步) | 教案 | teacher_guide |
| platform | 研究日志框架 | project-research-log | 技术方案 | research_log_scaffold\|evidence_ledger |
| platform | 论文骨架 | competition-paper-generator(骨架) | 技术方案 | paper_skeleton_md\|evidence_ledger\|paper_preview_docx |
| platform | 材料清单 | idea-to-build | — | bom |
| platform | 图表 | project-drawio-generator | — | design |
| platform | 课件 | courseware-generator | 教案 | （走 courseware 上传，写 project_lessons） |
| sprint | 研究方案 | research-proposal-generator | 技术方案 | — |
| sprint | 论文 | competition-paper-generator(装配) | 论文骨架 | paper_final_docx |
| sprint | 原始资料 | research-material-templates | 技术方案 | — |
| sprint | 照片清单 | photo-checklist-generator | — | — |
| sprint | 答辩PPT | presentation-generator | 论文 | — |
| sprint | 金鹏视频 | jinpeng-video-ppt | 论文 | — |
| sprint | 匿名化检查 | jinpeng-anonymization-checker | — | — |
| sprint | 提交整理打包 | jinpeng-submission-organizer | 论文 | — |

- 兼容别名（旧 build 仍会发）：`快速选题包→快速产题`、`选题→深度产题`、`评题→深度评题`、`研究日志→研究日志框架`（`workflow-registry.ts:103-108`）。
- 未注册类型 **fail-closed**：`resolveSkill()` 返 null → pipeline 记 `未知材料类型`（`materials.ts:10`，`pipeline.ts:263-277`）；注册表本身启动即校验（`workflow-registry.ts:67-83`，重复/未知阶段直接抛）。

#### ⑤ 看板卡（P/C/B）

- 存储：NAS `/share/CACHEDEV1_DATA/.appconfig/.kanban.json`，单文件、原子写 + 进程内 RLock（`web/main.py:15393-15394`）。**卡是 schemaless 的**，前端可直写字段。
- 类型白名单（2026-08-01 三板收口）：`c` 学生 / `b` 机构（含学校，用 clientType 区分）/ `p` 项目；`g`/`o` 只读兼容（`web/main.py:15412-15414`）。
- P 卡关键字段：`projectCode / stage / line / assignee / ledger / designerName / price / outsourceStage / platformProjectId / folder_path`。upsert 语义"缺字段就不写不清空"（`web/CLAUDE.md` Q 节）。
- C 卡承载教务：`class_log[]`（上课流水）、`competitions[]`（历年比赛）、`class_count/class_hours` 汇总、`teacher`、`studentUser`（`docs/教务融合规划.md:42-65`）。

#### ⑥ 选题包 Pack + 包题状态（B 端快车道）

- `pack_state.json`（NAS topic_board 目录），状态枚举 `sent/picked/scored/queued/established/backup`（`web/main.py:4453`），写入 `_set_topic_pack_state`（`:4487`）。
- 有意思的耦合：`/api/mac/exec` 发起立项时记住 `job_id→(pack_id, topic_no)`，`/api/mac/job` 轮到 done 时从输出里正则抓 P 号回写包题状态为 `established`（`web/main.py:6628-6650`）。

#### ⑦ 产题偏好准则 TopicRules + 反馈库（RLHF 闭环）

- 状态机：mini `runtime/topic-rules.json`（版本/正文/草稿/历史/补充条款）+ `runtime/topic-feedback.json`（反馈库）（`brain/src/topic-feedback.ts:20-21`）。
- **正本是 Markdown 文件、版本化、全系统唯一真源**（`workflow_registry.json:55-67`）：
  ```
  mini 产物/weizun/选题工坊/准则/{产题偏好准则.md, 历史/, 草稿/, 补充/}
    → project_sync 每 30 分钟拉回 Mac 课题/00_产题评题/准则/
    → 镜像到 NAS /share/项目管理/00_产题评题/准则/
  ```
- 反馈条目结构 `TopicFeedback`：`id/ts/updatedAt/owner/packId/topicNo/topicTitle/stars(1-5)/comment`（`topic-feedback.ts:33+`）。
- 操作只有一套实现、两个入口：App RLHF 页（确定性按钮，`/api/agent/topic-rules`）与大脑 `topic_rules` 工具（`show/distill/confirm/rollback/discard/add_principle`）。**确认权只在老板。**
- 注入点：`pipeline.ts:352-366`——产题类当"对照产出"，评题类当"评分透镜"，version 0 时一个字都不加。

#### ⑧ 证据台账 evidence_ledger（最接近 OneCreat `internal/evidence` 的东西）

不是 NASApp 的代码，是 **skill 之间约定的 JSON 契约**，三站接力：
- 站 1 `competition-paper-generator` 骨架模式：出论文骨架 + `<短名>_evidence_ledger.json` v1 + 效果预览 docx（`competition-paper-generator/SKILL.md:30, 54, 92`）。
- 站 2 `lesson-plan-generator`：把 ledger items 排到具体课次，产 `lesson_cards` 指标卡 + 《老师采集手册》（`lesson-plan-generator/SKILL.md:4-7`）。
- 站 3 `project-research-log`：写 `phases[]/entries[]/layout`，补日志专属采集物入账，规则"先对账再设计 / 补账 / 课次对齐"（`project-research-log/SKILL.md:125-128`）。
- 结题时站 1 的装配模式按台账把真实素材装进槽位；`perishable: false` 的缺失标"可现补"，`true` 的如实标缺失（`competition-paper-generator/SKILL.md:110`）。
- P 卡上有 `ledger:{done,total}` 摘要（大脑 `project_status` 会读，`tools.ts:1180`）。

### 2.2 存储总表

| 数据 | 位置 | 形态 | 谁写 |
|---|---|---|---|
| 题来源正本 md | Mac `课题/00_产题评题/{产题,评题}` | 平铺 Markdown + frontmatter | skill 产出后 project_sync 拉回；人工也可写 |
| 产题准则正本 | 同上 `/准则/` | Markdown 树 | brain 导出 → project_sync 拉回 → 镜像 NAS |
| 项目实体 | Mac `课题/01_在研项目/P26X-NNN_短名/` | 七目录 + project.json | establish_project.py |
| 未立项包 | Mac `课题/01_在研项目/_未立项/` | 目录 + 未立项原因.md | brain decide → NAS `_未立项_入库` → project_sync 拉回 |
| 项目镜像 | NAS `/share/项目管理/00_项目总库/<编号>_<短名>/` | 同构七目录 | project_sync rsync（追加式，无 --delete） |
| 题索引 | NAS `.appdata/topic_board/topic_index.json` | JSON 快照 | refresh_board.py（scp 原子上传） |
| 看板数据（兼容） | 同上 `board_data.json` | JSON | project_sync 第 6 步 |
| 包题状态 | 同上 `pack_state.json` | JSON | main.py 原子写 |
| 编号登记簿 | 同上 `project_codes.json` | JSON | main.py（RLock 守临界区） |
| 看板卡 | NAS `.appconfig/.kanban.json` | JSON 单文件 | main.py |
| 任务/台账/成员 | NAS `.appconfig/.tasks.json` / `.ledger.json` / `.config.json` | JSON | main.py |
| 材料 job | mini `runtime/jobs.json` | JSON | brain server.ts |
| 对话线程 | mini `runtime/conversation-threads.json` | JSON | brain |
| 准则/反馈状态机 | mini `runtime/topic-rules.json` / `topic-feedback.json` | JSON | brain |
| 成本台账 | mini `cost-log.jsonl` + `runtime/budget-events.jsonl` | JSONL | brain cost.ts/budget.ts |
| App 生成产物 | mini `产物/<owner>/<项目>/<环节>/` | 文件 | pipeline |
| 教务/课程/项目包 | 教师平台 Supabase | 关系库 | 平台自身（NASApp 只经 iOS 编排） |

**没有关系型数据库。** 除了教师平台的 Supabase，NASApp 侧全部是 JSON 文件 + 文件系统。并发靠"进程内锁 + 原子 rename"。

### 2.3 与 `/Users/localwork/课题/` 的关系（最关键的一条）

- **Mac 是真源（M4 是真源，`project_sync.py:19` 原文硬约束）**，NAS 是镜像 + 柜台，mini 是车间。
- 数据流方向：
  - Mac → NAS：`project_sync.py` 分类投放（rsync 追加式，**排除 `App产物/`** 防回环）。
  - mini → NAS：`ingestProjectArtifact` 逐文件 base64 上传到 `<桶>/App产物/`（`server.ts:885` → `web/main.py:5984`）。
  - NAS → Mac：`project_sync.py` 增量拉回 `App产物/`、`_未立项_入库/`、`07 桶平台素材包 zip`。
  - mini → Mac：选题工坊产物（`产题/评题/准则`）单向回流（`project_sync.py:57-70`，主机是 `mini`）。
- **选题工坊是伪项目**：没有 P 编号，产物不走 `project-artifact/ingest`（`server.ts:865, 881`；`web/CLAUDE.md` Q 节"选题工坊没有编号，不走 ingest"）。

---

## 3. 代码落点

### 3.1 模块依赖图（文字版）

```
                        ┌────────────────────────────────────┐
                        │ brain/registry/workflow_registry.  │
                        │   json (69 行, v16.0)  ★唯一真源     │
                        └───────────────┬────────────────────┘
                                        │ 只读 + 启动即校验
                        ┌───────────────▼────────────────────┐
                        │ brain/src/workflow-registry.ts(112)│
                        └───────┬───────────────┬────────────┘
                                │               │
              ┌─────────────────▼──┐      ┌─────▼───────────────┐
              │ materials.ts (21)  │      │ workflow.ts (106)   │
              │ 类型→skill         │      │ DAG 节点/依赖/拓扑   │
              └─────────┬──────────┘      └─────┬───────────────┘
                        └────────┬───────────────┘
                                 ▼
              ┌──────────────────────────────────────────────┐
              │ pipeline.ts (522)  ★业务语义层                 │
              │  拓扑排序 → 依赖检查 → 依赖注入(读上游全文)     │
              │  → 逐材料的"本环境说明"prompt 注入             │
              │  → 产题准则注入 → 会话记忆注入                 │
              │  → runOneMaterial(重试1次 / 假成功检测 /       │
              │     PNG 看图质检 / 图表组装说明)                │
              │  → recordRun 记账 + guardJobSpend 预算         │
              └──────────────────┬───────────────────────────┘
                                 ▼
              ┌──────────────────────────────────────────────┐
              │ engine.ts (~1100)  ★运行层(不认识"竞赛")        │
              │  runSkill  → claude -p (--allowedTools,       │
              │              stream-json, 空 CLAUDE_CONFIG_DIR)│
              │  runPiSkill→ pi -p --mode json --skill …      │◄─ 默认壳
              │  runGrokSkill → grok                          │
              │  并发闸 / detached 进程组 / killTree / 超时     │
              └──────────────────┬───────────────────────────┘
                                 ▼  ANTHROPIC_BASE_URL=api.deepseek.com/anthropic
                     ┌───────────────────────────────┐
                     │ DeepSeek V4 + oneup-skills 仓  │
                     └───────────────────────────────┘

  server.ts (191KB, ~5000+ 行) ★接口层
    ├ /api/threads[/:id/{message,decide}]  ← 对话型材料
    │   startConversationThreadJob(:960) / syncConversationThread(:1111)
    │   normalizeSourceThreadOutputs(:1036) 归一文件名+补 frontmatter
    │   deliverLinkedProjectArtifacts(:864) → NAS ingest
    ├ /api/generate|revise|upload|project-state|project-info|workflow…
    ├ /api/brain-chat  → agent-core/agent-domain(工具/系统提示词/UI 上下文)
    └ /api/full-mode/* → pi-daemon(代理模式)

  agent-domain/
    tools.ts (132KB, 61 个工具) ─┬─ photo-uploader-client.ts (24KB) → NAS 内部 API
    system-prompt.ts (40KB)     └─ context.ts / ui-context.ts
```

```
  web/main.py (16939 行, FastAPI 单文件) ★NAS 柜台
    ├ 认证:_require_login/_require_agent(:2624)/_require_admin(:554)/_require_internal(:2766)
    ├ 大脑代理:_cluster_call(:2681) / _cluster_raw(:2713) / SSE 透传
    ├ 题板:/api/topic-board(:902) 读 topic_index.json + 叠加活跃立项线程
    ├ 工作流代理:/api/workflow-registry(:7471) /api/agent/threads*(:7489-7524)
    │            /api/agent/project-state(:7943) /api/agent/projects-summary(:7968)
    ├ 发号/P卡:/api/project-code/preview(:4678) /api/internal/project-card/upsert(:6152)
    │          八段列换算 _apply_flow_binding(:6423) / flow-columns-migrate(:6446)
    ├ 产物落盘:/api/internal/project-artifact/ingest(:5984)
    │          /api/internal/unestablished/ingest(:6073)
    ├ 平台项目包:/api/project-package/{files,file,lesson-plan-merged,courseware}(:5465-5623)
    ├ Mac 中转:/api/mac/{actions,exec,job}(:6610-6651) + /api/internal/mac-*(:6654-6675)
    ├ 看板:/api/kanban/*(:15779+) 存 .kanban.json(:15393)
    └ 大脑内部端点:/api/internal/*(project-overview / topic-board-data / topic-packs …)
          ▲
          │ HTTP + 共享 token(~/.macfs_token)
  mac-agent/macfs_agent.py (725 行) ★Mac 受控执行
    ROOTS 白名单(keti/project/code) + BLOCKED_NAMES/SUFFIXES/DIRS
    ACTIONS(:317) = sync_all / sync_one / refresh_board / git_status /
      establish_project / quick_establish_project / establish_bound /
      assign_project / reline_project / inspect_lesson_plan
    argv 数组 + shell=False；15min 超时；审计日志 macfs-exec-audit.log
          │
          ▼ 只调 /Users/localwork/课题/99_工作区/scripts/ 下的固定脚本
  establish_project.py(27KB) / project_sync.py(79KB) / assign_project.py(7KB) /
  reline_project.py(6KB) / refresh_board.py(5.5KB) → topic_index.py(35KB) /
  migrate_v16.py(20KB, 一次性)
```

```
  客户端
  ios/OneUpTasks/  (SwiftUI，主力端)
    WorkflowHomeView.swift(94)      科创首页壳 = ProjectListView + 活动悬浮球
    ProjectListView.swift(446)      P26 项目列表
    ProjectWorkbenchView.swift(1178) 项目工作台(五段展开)
    TopicBoardView.swift(1118)      v16 课题看板(四来源+存量筛选、立项入口)
    TopicDetailView.swift(467) / TopicCard.swift(251)
    TopicRLHFView.swift(577) / TopicRating.swift(129)  产题评价 + 确定性控制台
    SprintView.swift(665)           竞赛冲刺段
    PackDetailView.swift(386)       选题包详情
    WorkflowMapView.swift(369)      八段登高图(纯静态方法论展示)
    WorkflowChatBar.swift(356) / WorkflowActivityStore.swift(145) /
      WorkflowActivityBubble.swift(498)   对话线程 UI + 全局活动悬浮球
    AlignFlow.swift(366)            技术方案「简报→确认→生成」闸门
    ExecutionComponents.swift(564) / AgentExecutionViews.swift(165)  执行全景
    Agent.swift(1426)               材料/注册表/job 数据模型 + API
    KanbanView.swift(9113)          看板(含 P 卡全部操作)  ← 单文件巨兽
    TeacherPlatformView.swift(1825) / TeacherAPI.swift(1211) / 
      TeacherLessonGenerationView.swift(229)  教师平台随身端
    Brain.swift(1466) / BrainChatView.swift  大脑对话

  web/static/desktop-more.js (160KB)  桌面端，IIFE 分家
    DWorkflow(:1952) 科创工作流三段+环节页+执行卡 / DSprint(:2658) 竞赛冲刺
    DTopicBoard(:1657) 课题库看板 / DProjects(:1139) 项目总览
  web/index-v2.html  手机网页版(发现 tab 下的竞赛 Agent)
  android/OneUpTasks  Kotlin+Compose 复刻，明确"备用端，平时少动"，功能滞后
```

### 3.2 各落点职责与规模

| 落点 | 行数/大小 | 技术栈 | 职责 | 对外接口 |
|---|---|---|---|---|
| `brain/registry/workflow_registry.json` | 69 行 / 14.6KB | JSON | **阶段/材料/skill/依赖/输出路径/平台上传的唯一真源** | 经 `/api/workflow-registry` 暴露 |
| `brain/src/workflow-registry.ts` | 112 | TS | 读+校验+缓存+别名 | 内部 |
| `brain/src/workflow.ts` | 106 | TS | DAG 节点生成、`getDeps/getSoftDeps/topoSort` | `/api/workflow` |
| `brain/src/materials.ts` | 21 | TS | 类型→skill | 内部 |
| `brain/src/pipeline.ts` | 522 | TS | 依赖注入 / 重试 / 质检 / prompt 定制 / 记账 | 内部 |
| `brain/src/engine.ts` | ~1100 / 40KB | TS | 三种壳(claude/pi/grok)、进程组治理 | 内部 |
| `brain/src/server.ts` | 191KB | TS(原生 Node HTTP) | 全部 HTTP API + job 持久化 + 线程编排 + NAS 投放 | `:8799` REST，Bearer |
| `brain/src/conversation-threads.ts` | 170 | TS | 线程持久层 | 内部 |
| `brain/src/topic-feedback.ts` | 22KB | TS | RLHF 反馈库 + 准则状态机 + 正本导出 | `/api/topic-rules`、`/api/topic-feedback` |
| `brain/src/topic-candidates.ts` | 11.6KB | TS | 扫盘解析选题工坊候选题 | 内部（`topic_workshop` 工具用） |
| `brain/src/agent-domain/tools.ts` | 132KB | TS | 61 个大脑工具 | 经 brain-chat |
| `brain/src/agent-domain/system-prompt.ts` | 40KB | TS | 系统提示词（含"系统模块地图"三档） | — |
| `brain/src/subagent.ts` + `registry/subagents.json` | 18.5KB + 110 行 | TS/JSON | 6 种子代理角色（ops/ops_ro/research/coder/analyst/writer） | `spawn_subagent` 工具 |
| `brain/src/full-mode.ts` + `pi-daemon/` | 58KB + 9 文件 | TS | 代理模式：常驻 Pi rpc 会话 | `:8798` 本机 |
| `brain/src/budget.ts` / `pricing.ts` / `cost.ts` | 31.5+7+1.7KB | TS | 记账（**默认只记账不拦截**） | `budget_status` 工具 |
| `web/main.py` | 16939 | FastAPI | NAS 柜台 + 代理 + 落盘 + 看板 + 发号 | `:8080` REST，会话/内部 token |
| `web/static/desktop-more.js` | 160KB | 原生 JS IIFE | 桌面端科创工作流/看板/冲刺 | — |
| `mac-agent/macfs_agent.py` | 725 | Python3 stdlib | Mac 受控执行 + 只读浏览 | `:8787` REST，共享 token |
| `课题/99_工作区/scripts/*.py` | ~200KB 总计 | Python3 | 发号建档/同步/索引/分配/改线 | CLI（argv） |
| `ios/OneUpTasks/*` 科创相关 | ~24000 行 | SwiftUI | 全部工作流 UI | — |
| oneup-skills | 5 插件 / 40+ skill | Markdown + 少量脚本 | **方法论正文** | SKILL.md |

### 3.3 定时任务

- **Mac**：`launchd` `com.oneup.macfs-agent`（常驻）、`com.oneup.macfs-ipreport`、`com.oneup.proxy-relay`（plist 在 `mac-agent/`）；`project_sync_timer.sh` + `99_工作区/scripts/launchd/`（每 30 分钟拉准则/产物）。
- **mini**：`com.oneup.agent-cluster`、`com.oneup.pi-agent`（launchd）。
- **brain 内部**：`scheduler.ts`（28.8KB）——`create_schedule/list_schedules/delete_schedule/run_schedule_now` 工具支持的每日定时任务，包括"晨报蒸馏"（07:00 后 tick）与项目归档编排（05:20，经 main.py `/api/internal/project-platform-archive/run` 中转）。
- **NAS**：平台备份拉取 cron（05:00，老板自建，代码里明确"禁止改动"）。

---

## 4. AI 的角色

### 4.1 三层 AI（三个完全不同的东西，别混）

| 层 | 名字 | 壳 | 模型 | 干什么 |
|---|---|---|---|---|
| ① 材料流水线 | pipeline/engine | **Pi**（默认，2026-08-20 起全量切）/ claude -p / grok | DeepSeek V4（heavy 预设） | 跑一个 skill 产出一份材料。无人值守、不问问题 |
| ② 大脑对话 | brain-chat | 自研 agent-core 循环 | DeepSeek V4 flash/pro | 61 个业务工具、确认卡、反问、UI 上下文；**派单给 ①** |
| ③ 代理模式 | full-mode + pi-daemon | 常驻 `pi --mode rpc` | `deepseek-v4-flash-vision-exp`（默认）+ 思考档 off/low/high/max | mini 上真跑 bash/读写文件，运维与自由任务；有业务工具桥 `oneup_*`(50 工具) |

三档在 App 里叫「大脑 / 代理 / 纯聊」（`NASApp/CLAUDE.md:130`）。

### 4.2 ① 材料流水线里的 AI

- 核心命题（`brain/README.md:7`）：**"Claude Code 当壳（运行 skill 的 agent loop），DeepSeek 当脑（便宜的模型）"** —— 把 `ANTHROPIC_BASE_URL` 指到 DeepSeek 的 Anthropic 兼容端点，40+ skill 零改。这是"别推翻重来"的根本前提（`DEVELOPMENT.md:108`）。
- 2026-08-19 起壳换成 **Pi**（`@earendil-works/pi-coding-agent`，npm 依赖，`node_modules/.bin/pi`）：同一任务 71s/$0.005 vs claude -p 89–175s/$0.04–0.077。Pi 原生认 SKILL.md。回退开关 `HEAVY_SHELL=claude`。
- prompt 结构（`engine.ts:253-262` `buildPrompt` + `pipeline.ts` 的层层追加）：
  ```
  使用 <skill> 技能，为下面这个学生项目生成对应的竞赛材料，直接输出文件到当前目录：
  <项目.md 全文>
  + ## 本次生成的额外要求（老师就地填的一句话，仅本次有效，优先满足）
  + ## 本项目已有的「<上游材料>」(完整内容，请以它为主要依据)   ← 硬依赖 + 软依赖
  + ## 已确认的论文框架（老师逐条确认过，必须严格按此章节结构）  ← 论文专用
  + ## 本次要生成的是【论文骨架】(骨架模式 Mode A)…            ← 模式点名
  + ## v16 App 产物契约（必须遵守）                            ← 产题/评题专用
  + ## 产题偏好准则（老板已确认，必须遵守）                      ← RLHF 注入
  + ## 本环境说明(App 内运行，与 Mac 目录不同)                  ← 逐材料定制
  + <项目级会话记忆:老师之前的补充 + 之前生成过什么>            ← 放最后(越靠后越重视)
  + 【联网能力】…用 Bash 运行 node src/web-search-cli.ts "关键词"
  注意：不要问我任何问题，缺的信息你自己合理假设，直接生成文件。
  ```
- 质量控制四件套（`pipeline.ts:148-226`）：
  1. **"假成功"检测**：exit 0 但零交付物 → 判失败（`:171-174`）；
  2. **失败自动重试 1 次**，成本累加（`:175-185`）；
  3. **AI 看图质检**：产物含 PNG 时渲染发给视觉模型看，**只报告不自动改**（`:188-198`，`visual_check.ts`）；
  4. **图表组装说明**：有 docx+drawio 无 PNG 时写一份人工组装指引（`:200-222`）。
- "本环境说明"是很有意思的一类知识——因为 skill 的 SKILL.md 是按老板 Mac 的目录写的，App 环境没有那些路径，所以每种材料都注入一段"你现在在哪、别去找什么、别跑什么脚本"（`pipeline.ts:379-433`：立项/老师采集手册/匿名化检查/提交整理打包/项目简要说明/3D交互视图各一段）。

### 4.3 ② 大脑的 61 个工具（`agent-domain/tools.ts`）

科创专属的（行号为 tools.ts）：

| 工具 | 行 | 性质 | 作用 |
|---|---|---|---|
| `start_material_generation` | 507 | 写(确认卡) | 启动材料生成；单题作业必须在 note 里点名"只评审这一个课题：「X」" |
| `project_status` | 1156 | 只读 | 项目八段总览：编号/业务线/分配/证据 done|total/材料齐备度/协作次数 |
| `topic_library` | 1191 | 只读 | 按**精确题名**核对 v16 题索引；`exact_matches=0` 才允许进"缺课题三选一" |
| `project_code_preview` | 1289 | 只读 | 预告下一编号（必须原样带进 establish 确认卡） |
| `establish_project` | 1306 | 写(确认卡+is_danger) | 正式立项：发号+七目录+P 卡+可选分配 |
| `quick_establish_project` | 1379 | 写(确认卡) | 题索引没有该题时：先写最小 md 再发号 |
| `assign_project` | 1345 | 写(确认卡) | 只改 project.json assignee + 软链，实体不搬家 |
| `mac_action` | 1415 | 混合 | 列白名单/发起/轮询 job_id；**显式拒绝**从这里绕过三个确认卡工具（:1460-1466） |
| `topic_workshop` | 1897 | 只读 | 包→题视图 + 候选池视图（同进程扫盘，禁止打 127.0.0.1） |
| `list_material_files` / `read_material_file` | 2010 / 2041 | 只读 | 列/读项目材料（docx 走 markitdown 异步） |
| `topic_feedback_overview` | 2070 | 只读 | 反馈库统计（可按星级筛） |
| `recent_topics` | 2103 | 只读 | 最近题（题索引快照 + 已评星标注） |
| `rate_topics` | 2142 | 交互 | **对话内联评分卡**（ToolTarget kind=`rate-topic`，iOS 渲染成打星条，点星即存） |
| `topic_rules` | 2190 | 混合 | show/distill/confirm/rollback/discard/add_principle |
| `student_create` / `organization_create` | 1225 / 1259 | 写(确认卡) | 前置实体就地补全 |
| `search_awards` | 1863 | 只读 | 1.3 万条获奖名单撞车检索 |
| `dispatch_heavy_task` / `spawn_subagent` | 658 / 702 | 写 | 派重型自由任务 / 派子代理 |

**行为规程写在系统提示词里，不在代码里**（`system-prompt.ts`）。最有方法论价值的几条：
- 【缺课题三选一】(`:73`)：`topic_library` 查不到 → 反问三选一（① 按新想法直接立项 ② 先走产题/评题 ③ 报准确题名）。选②"**只引导，不能偷偷发号**"。
- 【一句话新学生立项全链】(`:76`)：六步，"**绝不能把 student_create 和 establish_project 合成同一张确认卡**"。
- 【轻重路由】(`:125`)：科创一条链的任何成果物**必须**派 `start_material_generation`，"你凭空写出来的东西没有这些兜底，质量不达标还会误导老师——**这是最严重的失职**"。反例明写：老师说"给三年级学员出几个课题"你直接在对话里列 4 个题 = 错。
- 【探讨豁免】(`:142`)：老师说"只讨论/脑暴/先不生成"时允许直接推理，但两条底线不变（系统真实数据仍要查工具；探讨产出不落文件）。一说落地动作豁免即刻结束。
- 【引擎档位】(`:128`)："老师说了才换，你不许替他挑"。

### 4.4 ③ 代理模式与子代理

- 代理模式：mini 上常驻 `pi --mode rpc`，两条泳道（人工对话 `full-<owner>-daemon` 工具全开；只读诊断 `full-<owner>-ro` 去掉 edit/write + `ONEUP_FULL_MODE_READONLY=1`）（`pi-daemon/README.md:40-43`）。三条硬规矩：env 白名单、`guard.ts` 必须 `-e` 显式加载、cwd 只能是 ops 目录（`full-mode.ts:17-20`）。
- 子代理：6 角色注册表 `brain/registry/subagents.json`，每个角色定死 `tools/bridge_tools/cwd/read_only/timeout_ms/budget_usd/prompt_file`；正文提示词在 `ops-template/agents/*.md`。max_concurrent=3、max_depth=1。
  - 与科创最相关的是 `research`（bridge_tools 含 `topic_library / recent_topics / search_awards / read_material_file`，只读）。

### 4.5 模型与网关

- **没有网关**：直连 `https://api.deepseek.com/anthropic`（Anthropic 兼容端点）与 `/v1`（OpenAI 兼容）。key 在 mini `.env` 的 `DEEPSEEK_API_KEY`。
- 目录与牌价唯一真源 `brain/src/pricing.ts`（`DEEPSEEK_CATALOG` + 8/16 峰谷价，按北京时间取价，周末全谷）。三端下拉都从 `/api/model-config` 拉，**禁止硬编码模型 id**（`NASApp/CLAUDE.md:138-144`）。
- 现役模型：`deepseek-v4-flash` / `deepseek-v4-pro` / `deepseek-v4-flash-vision-exp`；`deepseek-chat`/`reasoner` 已退役（alias 兜底）。
- 联网搜索 5 源，`searchProvider=auto`：DeepSeek 内置 → 博查（中文）/ Tavily（英文）→ DuckDuckGo。重型壳**没有 WebSearch 工具**，靠 `node src/web-search-cli.ts` CLI（`engine.ts:266-280`），并明说"不许因为没有 WebSearch 就跳过那一步，更不许凭记忆编造检索结果"。
- 视觉：DeepSeek 主、MiniMax 备（失败回退一次）。
- 成本：`cost-log.jsonl` 一行一次运行（含 `owner/jobId/source/shell`）；`budget.ts` **默认只记账不拦截**（老板 2026-08-22 拍板"不要预算上限，只要知道花了多少"）；`BRAIN_BUDGET_ENFORCE=1` 才启用五档拦截。

---

## 5. 对外依赖与耦合

### 5.1 依赖 NAS（QNAP）的能力

| 能力 | 用在哪 | 可替代性 |
|---|---|---|
| Container Station / Docker | 跑 photo-uploader 容器 | 高（普通 Docker） |
| SMB 共享盘 `/share/项目管理` | 项目总库七目录、`_未立项_入库`、准则镜像 | 高（就是个目录） |
| `/share/CACHEDEV1_DATA/.appdata` `.appconfig` | topic_board 四个 JSON、`.kanban.json`、`.config.json` | 高 |
| 内网固定 IP + 免密 ssh 别名 `nas` | project_sync 的 rsync/scp 目标 | 中 |
| 自签证书 :8080 + Cloudflare→VPS→反向隧道 | 公网入口 | 中 |
| **发号权威（project_codes.json + RLock）** | 立项 | 逻辑可搬，但"谁是唯一发号方"是架构决策 |

⚠️ 大脑**已经不在 NAS 上**了（2026-08-15 迁到 mini），NAS 上的 agent-cluster 容器只停未删作回滚。

### 5.2 依赖教师平台（teacher / Supabase）

- 只在 **⑥ 授课交付 / ⑦ 结题冲刺** 两段。
- 账号体系**两棵树、零包含、不做单点登录**（`docs/教师平台集成.md:30-36`）。
- **写凭证只在 iOS Keychain**，NAS 后端绝不保存或代理（`NASApp/CLAUDE.md:198-199`）。唯一例外：服务端**只读** token（平台 `SERVICE_READONLY_TOKEN` ↔ mini `TEACHER_READONLY_TOKEN`），仅供大脑 `teacher_platform` 工具读三个 GET，token 本体只存 mini `.env`。
- 数据交换：
  - NAS → 平台：iOS 从 `02_平台材料/`（**收集范围锁死这一个桶**）挑候选文件 → `POST /api/m/projects/package-file`；课件走 `courseware` JSON 直传写 `project_lessons`。
  - 平台 → NAS：`material-export` 匿名素材包 ZIP → `05_授课产物/platform-materials-<sha16>.zip` → project_sync 拉回 `07_竞赛提交/平台素材包/`；日常回流按类型分投 `02/04/05` 的 `平台/` 子目录。
  - 隔日快照：`platform_archive.build_platform_snapshot` → `.appconfig/.teacher_snapshot.json`（供晨报与大脑降级兜底）。
- 平台侧 ID 映射锚点：B 卡 `orgId` ↔ platform organization id；P 卡 `platformProjectId` ↔ 平台项目。

### 5.3 依赖本机 Mac 的脚本

全部经 `macfs_agent.py:317` 的 `ACTIONS` 白名单调用，**永远不执行调用方传来的命令**（argv 数组 + `shell=False`）：

| action | 脚本 | 作用 |
|---|---|---|
| `establish_project` | `establish_project.py --topic --line --short [--assignee --assignee-id --expected-code]` | 发号+平铺建档+七目录+project.json+P 卡+单项目同步 |
| `quick_establish_project` | 同上 `+ --quick-description` | 先写最小 md 再发号 |
| `establish_bound` | 同上（号已发出，绝不再发号） | 手建 P 卡的项目补 Mac 实体 |
| `assign_project` | `assign_project.py --short --type --name` | 只改元数据 + NAS 软链 |
| `reline_project` | `reline_project.py --short --line` | 受控改线，保留后三位，五处联动 |
| `sync_all` / `sync_one` | `project_sync.py --all` / `<短名>` | 七步同步（见 `project_sync.py:4-14`） |
| `refresh_board` | `refresh_board.py` → `topic_index.py` | 重建题索引并原子 scp 上传 NAS |
| `inspect_lesson_plan` | `inspect_lesson_plan.py` | **只读**检查 02_平台材料 下的教案 md |
| `git_status` | `git -C <repo> status` | 仓库白名单 nasapp/skills |

加动作的规矩（`macfs_agent.py:314-316`）："会删数据、会对外发送（邮件/推送/发布）的动作**不要加进来**"。

### 5.4 平台无关 vs 平台专属

**平台无关（纯方法论/领域逻辑）**
- `workflow_registry.json` 全部内容（阶段、材料、依赖、命名、frontmatter 契约）
- `workflow.ts` DAG + 拓扑排序 + 依赖检查
- `pipeline.ts` 的依赖注入 / 重试 / 假成功检测 / 质检 / prompt 定制策略
- `conversation-threads.ts` 线程状态机 + 题名归一化 + 活跃去重
- `topic-feedback.ts` RLHF 状态机（反馈→蒸馏→草稿→确认→注入→回滚）
- `topic_index.py` 的合并算法与卡结构
- 七目录判例表、题状态机、命名与 frontmatter 契约（三份 README）
- oneup-skills 的全部 SKILL.md（方法论正文）
- 证据台账三站接力契约

**平台专属胶水**
- `web/main.py` 里 90%+ 的内容（QNAP 路径、会话/成员/权限、照片上传、看板 JSON、ESP32-P4 屏、录音笔、快传、公共站、技术库、监控…）
- `macfs_agent.py` 的 HTTP 服务与 token（受控执行**思想**可留，实现要重写）
- `project_sync.py` 的 rsync/scp/软链编排（79KB，几乎全是 NAS/Mac 拓扑）
- iOS/Android 的全部 UI
- `desktop-more.js` / `index-v2.html` 的全部 UI
- 教师平台集成（iOS Keychain 编排、package-file 上传、素材包 ZIP 回搬）
- pi-daemon / full-mode（运维代理，与科创工作流无关）
- 预算/记账/推送/晨报/网站中心/教务档案/台账/录音/互传…（同一后端里的邻居功能）

---

## 6. 迁移视角的判断

### 6.1 可以平移的（"方法论内核"，约占价值的 80%、代码量的 10%）

| 资产 | 现在在哪 | 迁到 OneCreat 的形态建议 |
|---|---|---|
| 阶段/材料注册表 | `workflow_registry.json` | 直接搬 JSON（或转 TOML/Go struct），配一个启动即校验的 loader（对应 `internal/config` 的 round-trip 守卫思路） |
| 材料 DAG + 拓扑 + 依赖检查/注入 | `workflow.ts` / `pipeline.ts` | Go 重写约 300–500 行；语义一一对应 |
| 对话线程状态机 | `conversation-threads.ts` | OneCreat 已有 `internal/session` + `agent.Session`，线程≈带业务字段的 session；`awaiting_input` 对应 ask/approval |
| 题状态机 + 归一化 key | 三处各一份 | **合并成一份**，放领域包 |
| 七目录判例表 | `01_在研项目/README.md` | 作为 AGENTS.md 级的项目记忆 + 一个 `bucket.Classify(materialType, relpath)` 函数 |
| RLHF 准则闭环 | `topic-feedback.ts` + 两个入口 | 直接对应 OneCreat 的"记忆/证据"层；准则注入点 = Compose 时的运行时注入（**不要进系统前缀缓存**） |
| 证据台账三站接力 | skill 契约 | **这是与 OneCreat `internal/evidence` 最契合的一块**：evidence ledger 的 items/slots/perishable/consumers 语义可以直接变成 evidence 引擎的一等公民 |
| skill 全家桶 | oneup-skills | OneCreat 已有 `internal/skill`（`/skill-name` + 内建索引）。SKILL.md 格式相同，理论上零改挂载 |
| 质量四件套 | `pipeline.ts` | 假成功检测/重试/看图质检/人工组装说明——都是与平台无关的产品判断 |
| 一批"血泪规则" | 散落注释 | 例：论文图不自动嵌、单题作业必须点名、深度模式不能把硬停伪装成完成、未注册类型必须报错不许客户端兜底猜 |

### 6.2 要重写或舍弃的

- **`web/main.py` 整个**：16939 行里科创相关不到 20%，其余是 NAS 文件站的邻居功能。只该抽出：题板叠加逻辑、发号临界区、P 卡八段列换算、产物 ingest 的 fail-closed 校验（路径/桶/大小/软链）四段。
- **`macfs_agent.py` + 那套"手机→NAS→Mac 白名单执行"链路**：OneCreat 本身就跑在本机、有 `toolpolicy` 门禁与 permission gate，整条远程受控执行链**没有存在理由**。但它的两个设计要保留：动作白名单 + argv 数组 + 独立审计日志；写操作必须过确认卡且 `is_danger`。
- **`project_sync.py`（79KB）**：三端 rsync 编排。OneCreat 单机形态下大部分消失，只剩"分类投放"（哪个文件进哪个桶）这一段业务规则值得保留。
- **iOS / Android / desktop-more.js 全部 UI**：OneCreat 是 Web UI（单二进制 + 浏览器），要重做。
- **教师平台集成**：属于另一个产品的边界，迁移时应先明确 OneCreat 要不要接。
- **pi-daemon / full-mode / 子代理 / 预算 / 推送 / 晨报 / 网站中心**：与科创工作流无关，OneCreat 已有对应或不需要。
- **`brain/README.md`、`WorkflowMapView.swift` 的 LV.1/LV.8 文案**：过期，别照抄。

### 6.3 需要迁移 / 双写 / 适配层的数据

| 数据 | 量级 | 建议 |
|---|---|---|
| `01_在研项目/P26*` 项目实体 | 26 个项目 × 七目录 | **不迁移，原地接管**——OneCreat 的 workspace 直接指向 `/Users/localwork/课题/01_在研项目/<项目>`，七目录天然是项目结构 |
| `project.json` | 每项目一个 | 保留原格式，OneCreat 读它当项目身份（对应 `session.Record` 的 workspace/kind 写一次语义） |
| `00_产题评题/{产题,评题}` md | 若干 | 不迁移，直接读 |
| `topic_index.json` | 一份快照 | 迁：把 `topic_index.py` 的扫描器移植成 OneCreat 的一个内置命令/工具 |
| `.kanban.json` 的 P 卡 stage | 26 张卡 | **必须迁**：stage 是唯一不在文件系统里的项目状态。建议写进 `project.json`（加 `stage`/`stage_updated_at`），P 卡降级为镜像或废弃 |
| C/B 卡（学生/机构档案、class_log、competitions） | 数十张 | 教务数据，**建议不迁**，划出 OneCreat 边界；若要迁需要设计新实体 |
| `pack_state.json` 包题状态 | 若干 | B 端快车道特有；可迁可弃，取决于是否保留"选题包"概念 |
| `project_codes.json` 编号登记簿 | 一份 | 必须迁（发号权威） |
| `runtime/conversation-threads.json` | 活跃线程 | 可不迁（在途任务不多），冷启动即可 |
| `runtime/topic-rules.json` + `topic-feedback.json` | RLHF 资产 | **必须迁**——这是老板一条条打星攒出来的、不可再生 |
| 准则正本 md 树 | 三端同构 | 已在 Mac `00_产题评题/准则/`，原地接管 |
| `cost-log.jsonl` | 历史账 | 可归档不迁 |
| `产物/<owner>/<项目>/<环节>/` | mini 上的中间产物 | 已投放的在 NAS/Mac，mini 那份可弃 |

**需要双写/适配层的**：若迁移期 NASApp 与 OneCreat 并行，唯一必须双写的是 **P 卡 stage** 和 **编号登记簿**（两个都是"唯一权威"型数据，双写必冲突）。建议：迁移当天冻结发号，一次性切。

### 6.4 迁移最难的 5 个点（按难度排序）

**① "文件位置即状态"与 OneCreat "session/engine 即状态"的世界观冲突**
v16 拍板的核心是：题的状态由它在哪个目录决定（`00_产题评题/产题` / `_未立项` / `P26*`），`topic_index.json` 只是扫描产物。OneCreat 的世界是 session + engine + 消息日志 + checkpoint。硬迁会出现两个真源。
- 难点具体化：立项线程 `succeeded` 后必须**停下来等人**（`awaiting_decision`），这不是 OneCreat 现有 turn 模型里的状态——它只有 running/idle + approval。要么把"建档/未立项"做成一种特殊的 approval，要么在 OneCreat 之上新建一层"工作流线程"实体（那就等于把 NASApp 的 thread 层原样搬进来）。
- 另一个坑：题名归一化 key 现在有三份实现，迁移时必须收成一份，且**必须与历史文件产生的 key 逐字节一致**，否则历史题卡全部变成新题。

**② 24 种材料的 prompt 定制知识分散在三处，且互相打补丁**
一份材料的最终 prompt = SKILL.md（外部仓）+ pipeline 的"本环境说明"（因为 SKILL.md 是按 Mac 目录写的）+ 模式点名（因为一个 skill 承载多个模式）+ 准则注入 + 会话记忆。
- 也就是说 `pipeline.ts:308-451` 那一大段 if 链**本质是在纠正 SKILL.md 与运行环境的不匹配**。
- 迁到 OneCreat 后运行环境变了（就在项目目录里跑、有真的七目录、有 bash/文件工具），这些补丁**大部分应该删掉而不是搬过去**——但删哪些、留哪些需要逐个 skill 实测。这是最耗时的一项，24 个材料 × 快慢两档。
- 且 `idea-to-build`、`project-brief`、`project-3d-viewer` 三个 skill 在 `hardware-toolkit` 里，`courseware-generator` 等在 `competition-toolkit` 里，`package-sync` / `topic-class` / `video-compress` 未在注册表登记——skill 仓与注册表已经不完全对齐。

**③ 老板作为"唯一决策点"的交互模型，OneCreat 没有对应物**
系统里至少 5 个地方是"必须老板亲手点"：立项建档确认、未立项判定（必须填 reason）、产题准则 confirm、引擎档位选择、P 卡拨段位。
- 现在这些落在 iOS 的确认卡 / RLHF 控制台 / 评分卡（ToolTarget kind=`rate-topic`）上。
- OneCreat 有 `approvalBroker`（approval/ask 提示、会话授权、YOLO/bypass）——形状接近但语义不同：OneCreat 的 approval 是"要不要允许这个工具跑"，NASApp 的是"任务已经跑完了，你要不要接受这个业务结论"。**后者是产品级决策点，不是安全门**，硬套到 toolpolicy 上会把两件事混起来。
- 还要注意 OneCreat 有 YOLO/bypass 模式——业务决策点绝不能被 bypass 掉。

**④ 多端一致性与"注册表唯一真源"的落地纪律**
NASApp 反复踩这个坑，v14→v15→v16 三次重写都是为了收口。现在的纪律是："任何一端新增材料时，先改注册表并补 skill/测试，再由 brain/web/scripts/iOS 消费；未注册类型必须报错，不能客户端兜底猜测"（`NASApp/CLAUDE.md:110-111`）。
- 迁到 OneCreat 后端数变少（Web UI + CLI），但同样的漂移会以另一种形式出现：注册表 vs skill 目录 vs 项目七目录判例表 vs 教师平台 file_type 枚举。
- 现存漂移证据：组别枚举写了 3 份、十流程 vs C 板十一列不同源、`WorkflowMapView` 文案过期、`brain/README.md` 整体过期。**迁移时必须一次性收口并加机械守卫**（OneCreat 已有这个文化：`boundary_test.go` / `TestRenderTOMLRoundTrips` / `TestKindNamesCoversEveryDeclaredKind`）。

**⑤ 三端物理拓扑消失后，"谁是真源"要重新定义**
现在 Mac=真源、NAS=镜像+柜台+发号、mini=车间，靠 rsync/ingest/软链把三份拉齐，并有明确的防回环规则（`App产物/` 单向）。
- OneCreat 单机后这些全部塌缩成一个目录 —— 好事，但要重新回答：
  - 发号权威放哪？（现在是 NAS 的一个 JSON + 进程内锁）
  - NAS 项目总库还留不留？（学生视角软链、SMB 共享给老师看、上课照片落点都依赖它）
  - 教师平台的项目包上传由谁做？（现在必须是 iOS，因为写 token 只在 Keychain）
- 这不是技术难点，是**产品边界决策**，必须先拍板再动代码。否则很容易做成"OneCreat 里又造一个 project_sync"。

### 6.5 顺带值得注意的几点

- OneCreat 的 `internal/evidence`（complete_step 匹配 todo_write、积累证据链）与 NASApp 的 evidence_ledger 是**两套独立发明的同一个想法**。迁移是把两者合并的最好时机：ledger 提供"这一章需要什么证据"的领域 schema，evidence 引擎提供"实际发生了什么"的运行时证据，两边一对账就是完整闭环。
- OneCreat 的 `internal/runtime` 四层作用域（Process→Workspace→Session→Turn）刚好能表达"一个项目共享的服务 vs 一个材料任务私有的东西"——NASApp 现在靠 `产物/<owner>/<项目>/<环节>/` 目录约定 + 全局并发闸（默认 2）来做，粒度粗得多。
- NASApp 的"engine 三档（deepseek/claude/grok）+ 壳三种（claude -p / pi / grok）"这套复杂度，在 OneCreat 里已经被 `internal/engine` 的 TurnEngine seam + provider registry 覆盖了，不用搬。
- 成本记账：NASApp 的 `cost-log.jsonl`（含 owner/jobId/source/shell 四维）比 OneCreat `internal/billing` 的口径更细，值得参考。

---

## 7. 附录

### 7.1 读过的关键文件（绝对路径）

**仓库总览与部署**
- `/Users/localwork/04_project/NASApp/CLAUDE.md`（219 行，含 v14–v20 大脑接入评估与「六、科创项目三端全流程契约」）
- `/Users/localwork/04_project/NASApp/README.md`
- `/Users/localwork/04_project/NASApp/web/CLAUDE.md`（607 行，读了 Q 节 430-467、十一~十三节 586-607、目录 1-70）
- `/Users/localwork/04_project/NASApp/docs/教师平台集成.md`（前 60 行）
- `/Users/localwork/04_project/NASApp/docs/教务融合规划.md`（前 80 行）
- `/Users/localwork/04_project/NASApp/docs/reviews/scitech_fullflow_review_20260805.md`（前 40 行）

**brain（大脑 / agent-cluster）**
- `/Users/localwork/04_project/NASApp/brain/registry/workflow_registry.json`（全文，69 行）
- `/Users/localwork/04_project/NASApp/brain/registry/subagents.json`（全文，110 行）
- `/Users/localwork/04_project/NASApp/brain/src/workflow-registry.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/workflow.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/materials.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/pipeline.ts`（1-522，全文）
- `/Users/localwork/04_project/NASApp/brain/src/conversation-threads.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/conversation-metrics.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/base-dir.ts` / `unestablished-path.ts`（全文）
- `/Users/localwork/04_project/NASApp/brain/src/engine.ts`（253-400, 705-800 + 结构 grep）
- `/Users/localwork/04_project/NASApp/brain/src/server.ts`（842-1060, 2173-2400 + 路由 grep）
- `/Users/localwork/04_project/NASApp/brain/src/config.ts`（1-90）
- `/Users/localwork/04_project/NASApp/brain/src/topic-feedback.ts`（前 40 行 + 结构）
- `/Users/localwork/04_project/NASApp/brain/src/full-mode.ts`（1-60）
- `/Users/localwork/04_project/NASApp/brain/src/agent-domain/tools.ts`（507-560, 1156-1480, 1897-2241 + 全工具名清单）
- `/Users/localwork/04_project/NASApp/brain/src/agent-domain/system-prompt.ts`（科创相关行 grep，约 60 行）
- `/Users/localwork/04_project/NASApp/brain/pi-daemon/README.md`（1-70）
- `/Users/localwork/04_project/NASApp/brain/README.md`（全文 339 行，注意已过期）
- `/Users/localwork/04_project/NASApp/brain/DEVELOPMENT.md`（89-160, 753-781 + 目录）

**web（NAS 后端）**
- `/Users/localwork/04_project/NASApp/web/main.py`：860-960（题板）、2624-2720（鉴权+集群代理）、4444-4520（编号/包题状态）、5984-6072（产物 ingest）、6366-6470（八段列）、6617-6660（mac 中转）、15391-15420（看板存储），以及全量路由/常量 grep
- `/Users/localwork/04_project/NASApp/web/test_v16_workflow.py`（1-70）
- `/Users/localwork/04_project/NASApp/web/static/desktop-more.js`（模块定位 grep）

**mac-agent**
- `/Users/localwork/04_project/NASApp/mac-agent/macfs_agent.py`（30-120, 157-380, 405-470）

**iOS**
- `/Users/localwork/04_project/NASApp/ios/OneUpTasks/WorkflowHomeView.swift`（全文 94 行）
- `/Users/localwork/04_project/NASApp/ios/OneUpTasks/WorkflowMapView.swift`（1-190，含全部八段数据）
- `/Users/localwork/04_project/NASApp/ios/OneUpTasks/Agent.swift`（1-60）
- 其余科创相关 swift 文件只统计了行数与文件名，**未逐个读**

**Android**
- `/Users/localwork/04_project/NASApp/android/OneUpTasks/README.md`（1-30）

**仓外真源（只读）**
- `/Users/localwork/课题/00_产题评题/README.md`（全文 85 行）
- `/Users/localwork/课题/01_在研项目/README.md`（1-80）
- `/Users/localwork/课题/01_在研项目/P26C-020_飞鸟志/project.json` + 目录结构
- `/Users/localwork/课题/99_工作区/scripts/README.md`（全文）
- `/Users/localwork/课题/99_工作区/scripts/project_sync.py`（1-70）
- `/Users/localwork/课题/99_工作区/scripts/refresh_board.py`（1-70）
- `/Users/localwork/课题/99_工作区/scripts/topic_index.py`（1-80, 460-485, 715-786）
- `/Users/zunwei/code/oneup-skills/README.md`（1-60）
- `/Users/zunwei/code/oneup-skills/competition-toolkit/skills/lesson-plan-generator/SKILL.md`（1-80）
- `/Users/zunwei/code/oneup-skills/competition-toolkit/skills/topic-scoring/SKILL.md`（1-50）
- `/Users/zunwei/code/oneup-skills/competition-toolkit/skills/topic-express/SKILL.md`（1-40）
- `/Users/zunwei/code/oneup-skills/competition-toolkit/skills/{project-research-log,competition-paper-generator}/SKILL.md`（台账相关行 grep）

### 7.2 没读 / 不确定的地方（诚实标注）

**完全没读**
1. `web/main.py` 的 16939 行里我只精读了约 600 行。**未读**：看板 CRUD 全套、账号/权限、文件浏览、互传、录音、快传、技术库、监控、台账、网站中心、`sites_hub.py`、`platform_archive.py`、`photo_classify.py`。其中 `platform_archive.py` 与结题冲刺段有关，**可能有我没覆盖到的工作流逻辑**。
2. `brain/src/server.ts` 191KB 我只读了约 400 行 + 路由清单。**未读**：`/api/align*`（技术方案对齐流）、`/api/outline*`（论文框架闸门）、`/api/preflight`、`/api/import-folder`、job 调度与持久化细节、`startJob` 本体。**`align.ts`(20KB) 与 `AlignFlow.swift` 实现的"简报→确认→生成"闸门是一个我只看到描述、没看到实现的重要机制。**
3. `brain/src/agent-domain/tools.ts` 132KB 只读了科创相关的约 400 行；`system-prompt.ts` 40KB 只 grep 了关键词命中行。
4. iOS 侧除 `WorkflowHomeView` / `WorkflowMapView` / `Agent.swift` 头部外，**其余 ~23000 行 swift 未读**。`KanbanView.swift` 单文件 9113 行里 P 卡的全部业务操作我没看。
5. `establish_project.py`(27KB) / `project_sync.py`(79KB) / `assign_project.py` / `reline_project.py` 只读了文件头与 README 描述，**未读实现**。发号的具体并发处理、七目录建档细节、软链接维护逻辑我是按文档转述的。
6. 教师平台仓 `/Users/zunwei/system/teacher` 完全未读。平台侧的 10 流程进度轴、`project_lessons`、`project_packages` 表结构我只有 NASApp 侧的描述。
7. oneup-skills 40+ 个 SKILL.md 我只读了 4 个（lesson-plan / topic-scoring / topic-express 的头部，加 grep）。**`idea-to-build`（立项的核心 skill）我没读**——它是"快速立项 vs 深度立项"两档差异的定义者，迁移时必读。
8. `web/static/desktop-more.js` 的 `DWorkflow`/`DSprint` 实现未读（只定位了行号）。
9. `ai_dou/`、`m5-recorder/`、`trimui_monitor/`、`mac/` 四个目录完全没看（判断与科创工作流无关，但没验证）。

**不确定的判断**
- 我判断 `stage` 只在 P 卡（`.kanban.json`）和 `project.json` 两处，但 `project_sync.py` 是否还有第三处写入，**不确定**。
- `courseware-generator` 在 `workflow_registry.json` 里标 `status: available` 且注明"2026-08-17 晚交付"，但我在 `competition-toolkit/skills/` 下确实看到了这个目录——**已交付**。而注册表里没有的 `package-sync`、`topic-class`、`video-compress` 三个 skill 的用途，**不确定**。
- `WorkflowMapView.swift` 的八段文案我判断为"存量未更新"，依据是 `CLAUDE.md:105-111` 明确说三层与晋级已退役。但**不确定**这个视图现在是否还在 App 里可达（可能已经从导航里摘掉、只是文件还在）。
- 「组别」枚举我找到 3 处独立定义（registry / topic_index.py / unestablished-path.ts），**可能还有第 4、5 处**（iOS 与 desktop-more.js 里大概率也各有一份），没有穷举。
- 大脑 `topic_library` 工具读的是 `topicBoardData()`（`board_data.json` 的 `incubation.topics`），而 `/api/topic-board` 读的是 `topic_index.json` ——两者由 `refresh_board.py` 同源生成，但**是否总是同时刷新、会不会出现口径差**，我没验证。
- 项目总数：Mac 上 26 个 P 项目目录（含 `_未立项`），但 NAS 项目总库、P 板卡数我**没有实际核对**。
