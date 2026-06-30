// Wire contract — mirrors desktop/wire.go (itself mirroring internal/serve/wire.go).
// One event channel carries every kind; `kind` discriminates the payload.

export type EventKind =
  | "turn_started"
  | "reasoning"
  | "text"
  | "message"
  | "tool_dispatch"
  | "tool_result"
  | "tool_progress"
  | "usage"
  | "notice"
  | "phase"
  | "approval_request"
  | "ask_request"
  | "turn_done"
  | "compaction_started"
  | "compaction_done";

export interface WireCompaction {
  trigger?: string; // "auto" | "manual"
  messages?: number; // done: how many messages were folded into the summary
  summary?: string; // done: the briefing (empty on an aborted pass)
  archive?: string; // done: archive path, if any
}

export interface WireTool {
  id?: string;
  name: string;
  args?: string;
  output?: string;
  err?: string;
  readOnly: boolean;
  truncated?: boolean;
  partial?: boolean; // an early dispatch (name only) — a full one with args follows
  parentId?: string; // set on a sub-agent's calls — the parent `task` call's id
}

export interface WireUsage {
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
  cacheHitTokens: number;
  cacheMissTokens: number;
  reasoningTokens?: number;
  // Session-cumulative cache tokens — the status bar shows the aggregate
  // hit-rate (Σhit/Σ(hit+miss)), steadier than the single-turn cacheHitTokens.
  sessionCacheHitTokens: number;
  sessionCacheMissTokens: number;
  costUsd?: number;
}

export interface WireApproval {
  id: string;
  tool: string;
  subject: string;
}

export interface WireAskOption {
  label: string;
  description?: string;
}

export interface WireAskQuestion {
  id: string;
  header?: string;
  prompt: string;
  options: WireAskOption[];
  multi?: boolean;
}

export interface WireAsk {
  id: string;
  questions: WireAskQuestion[];
}

// QuestionAnswer is the reply for one question, sent back via AnswerQuestion.
export interface QuestionAnswer {
  questionId: string;
  selected: string[];
}

export interface WireEvent {
  kind: EventKind;
  text?: string;
  reasoning?: string;
  level?: "info" | "warn";
  tool?: WireTool;
  usage?: WireUsage;
  approval?: WireApproval;
  ask?: WireAsk;
  compaction?: WireCompaction;
  err?: string;
}

// Bound-method payloads (desktop/app.go).
export interface HistoryMessage {
  role: string;
  content: string;
  reasoning?: string;
}

// CheckpointMeta is one rewind point (a user turn) for the rewind UI.
export interface CheckpointMeta {
  turn: number;
  prompt: string;
  files: string[];
  time: number; // unix ms
}

// SessionMeta is one saved session for the history panel.
export interface SessionMeta {
  path: string;
  preview: string;
  title?: string; // user-chosen name; falls back to preview when empty
  turns: number;
  createdAt?: number; // unix milliseconds
  lastActivityAt?: number; // unix milliseconds
  modTime: number; // compatibility alias for lastActivityAt
  current: boolean;
  cwd?: string; // workspace path at session creation (for sidebar folder grouping)
  kind?: string; // 会话类型(如 "hardware");空=普通对话。历史侧栏据此区分垂直
}

export interface WorkspaceView {
  path: string;
  name: string;
  current: boolean;
}

export interface ContextInfo {
  used: number;
  window: number;
}

export interface Meta {
  label: string;
  ready: boolean;
  startupErr?: string;
  eventChannel: string;
  cwd: string;
  bypass?: boolean; // YOLO mode on (auto-approve every tool call)
  planMode?: boolean; // 该标签 controller 的真实 plan(只读)门控状态(A8)
  running?: boolean; // 该标签是否有 turn 正在跑(切回时恢复 spinner/守卫)(A3)
}

// PendingPrompts 是某标签当前未应答的审批 / ask(切回标签时补显弹窗用)(A2)。
export interface PendingPrompts {
  approvals: WireApproval[];
  asks: WireAsk[];
}

// TabMeta 是一个任务标签的快照(对应 desktop/app.go 的 TabMeta)。每个标签是一个独立
// 任务:自己的 controller + session,后台标签也并行跑。kind 决定显示对话还是硬件视图。
export interface TabMeta {
  id: string;
  kind: string; // "chat" | "hardware"
  label: string;
  ready: boolean;
  startupErr?: string;
  active: boolean;
}

// Mode is the input mode cycled by Shift+Tab: normal → plan (read-only) → yolo
// (auto-approve every tool call; deny rules still apply).
export type Mode = "normal" | "plan" | "yolo";

export interface CommandInfo {
  name: string; // without the leading slash
  description: string;
  hint?: string;
  kind: "builtin" | "custom" | "mcp" | "skill";
}

export interface DirEntry {
  name: string;
  isDir: boolean;
}

export interface FilePreview {
  path: string;
  body: string;
  size: number;
  truncated: boolean;
  binary: boolean;
  err?: string;
}

// MCP & Skills drawer (desktop/app.go Capabilities) — the GUI counterpart to
// /mcp + /skill: connected/failed servers and discoverable skills.
export interface ServerView {
  name: string;
  transport: string;
  status: "connected" | "failed" | "disabled";
  tools: number;
  prompts: number;
  resources: number;
  error?: string;
  toolList?: MCPToolView[];
}
export interface MCPToolView {
  name: string;
  description: string;
}
export interface SkillView {
  name: string;
  description: string;
  scope: string;
  runAs: string;
}
export interface SkillRootView {
  dir: string;
  scope: string;
  priority: number;
  status: string;
  configured: boolean;
  skills: number;
  warning?: string;
}
export interface CapabilitiesView {
  servers: ServerView[];
  skills: SkillView[];
  skillRoots: SkillRootView[];
}
export interface HardwareMCPView {
  name: string;
  available: boolean;
  command: string;
  source: string;
  configured: boolean;
  connected: boolean;
  error?: string;
}
// HardwareBoardSummary 是板卡选择器的一项,来自共享数据驱动注册表(boards.json);
// 加一块板=改 JSON,UI 下拉自动多一项。
export interface HardwareBoardSummary {
  value: string;
  label: string;
  framework: string;
  platform: string;
}
export interface HardwareToolchainView {
  name: string;
  command: string;
  available: boolean;
  path?: string;
  version?: string;
  hint?: string;
}
export interface HardwareBoardView {
  port: string;
  protocol?: string;
  boardName?: string;
  fqbn?: string;
  core?: string;
  properties?: string;
}
export interface HardwareDeviceView {
  port: string;
  description?: string;
  hwid?: string;
}
export interface HardwareProjectCandidateView {
  dir: string;
  kind: string;
  entry?: string;
}
export interface HardwareDetectView {
  available: boolean;
  workspace?: string;
  projectDir?: string;
  projectTypes: string[];
  candidateProjects: HardwareProjectCandidateView[];
  serialPorts: string[];
  boards: HardwareBoardView[];
  devices: HardwareDeviceView[];
  toolchains: HardwareToolchainView[];
  recommendations: string[];
  espIdfOfficialMcp?: Record<string, string>;
  error?: string;
}
// 一键安装核心工具链的单步与整体结果（对应 Go 的 HardwareInstall*View）。
export interface HardwareInstallStepView {
  tool: string;
  action: string; // already_present | installed | failed | skipped
  ok: boolean;
  path?: string;
  message: string;
}
export interface HardwareInstallToolchainView {
  available: boolean;
  steps: HardwareInstallStepView[];
  allOK: boolean;
  managedDir?: string;
  nextStep?: string;
  error?: string;
}
// 串口监视器（常驻双向串口）打开/写入的统一返回。
export interface SerialResult {
  ok: boolean;
  error?: string;
}
export interface HardwareEvidenceStatusView {
  available: boolean;
  projectDir?: string;
  platform?: string;
  board?: string;
  evidenceFile?: string;
  recordCount: number;
  currentRecordCount: number;
  staleRecordCount: number;
  status: string;
  summary: string;
  missingGroups: string[];
  recommendations: string[];
  error?: string;
}

// 写代码前硬注入 prompt 的板卡事实串（来自 hardware MCP 的 board_profile + 平台 API）。
export interface HardwareBoardFactsView {
  found: boolean;
  facts: string;
}

export interface KnowledgeBaseView {
  id: string;
  name: string;
  createdAt: number;
  updatedAt: number;
  documents: number;
  chunks: number;
}
export interface KnowledgeDocumentView {
  id: string;
  baseId: string;
  name: string;
  originalPath: string;
  storedPath?: string;
  size: number;
  importedAt: number;
  status: string;
  chunks: number;
  error?: string;
}
export interface KnowledgeView {
  storeDir: string;
  mode: string;
  supportedExtensions: string[];
  bases: KnowledgeBaseView[];
  documents: KnowledgeDocumentView[];
}
export interface KnowledgeImportIssue {
  path: string;
  error: string;
}
export interface KnowledgeImportResult {
  imported: KnowledgeDocumentView[];
  skipped: KnowledgeImportIssue[];
}
export interface KnowledgeMatchView {
  baseId: string;
  baseName: string;
  documentId: string;
  documentName: string;
  chunkId: string;
  chunkIndex: number;
  text: string;
  score: number;
}
export interface KnowledgeSearchResult {
  query: string;
  matches: KnowledgeMatchView[];
}
export interface KnowledgePromptView {
  prompt: string;
  sources: KnowledgeMatchView[];
}
export interface MCPServerInput {
  name: string;
  transport: string; // stdio | http | sse
  command: string;
  args: string[];
  url: string;
  env: Record<string, string>;
}

export interface ModelInfo {
  ref: string; // "provider/model" — pass to SetModel
  provider: string;
  model: string;
  current: boolean;
}

export interface EffortInfo {
  supported: boolean;
  current: string; // "auto" | "low" | "medium" | "high" | "xhigh" | "max"
  default: string;
  levels: string[];
}

// Slash sub-command / argument completion (desktop/app.go SlashArgs). Mirrors the
// CLI's arg hints so the composer can suggest e.g. /skill → list/show/new/paths.
export interface SlashArgItem {
  label: string;
  insert: string; // token to place at the current position
  hint: string;
  descend: boolean; // re-open the menu one level deeper after accepting
}
export interface SlashArgsResult {
  items: SlashArgItem[];
  from: number; // byte offset where the current token begins
}

// Memory panel payloads (desktop/app.go MemoryView).
export interface MemoryDoc {
  path: string;
  scope: string; // "user" | "ancestor" | "project" | "local"
  body: string;
}

export interface MemoryFact {
  name: string;
  title?: string;
  description: string;
  type: string; // "user" | "feedback" | "project" | "reference"
  body: string;
}

export interface MemoryScope {
  scope: string; // "user" | "project" | "local"
  path: string;
}

export interface MemoryView {
  docs: MemoryDoc[];
  facts: MemoryFact[];
  scopes: MemoryScope[];
  storeDir: string;
  available: boolean;
}

// Settings panel payloads (desktop/settings_app.go).
export interface ProviderView {
  name: string;
  kind: string;
  baseUrl: string;
  models: string[];
  default: string;
  apiKeyEnv: string;
  keySet: boolean; // the env var currently resolves to a value
  balanceUrl: string; // optional wallet-balance endpoint; "" disables the readout
  contextWindow: number;
}

// BalanceInfo is the wallet-balance readout (desktop/app.go Balance). available
// is false when the provider declares no balanceUrl or a fetch failed; display is
// the formatted amount (e.g. "¥110.00").
export interface BalanceInfo {
  available: boolean;
  display: string;
  err?: string;
}

// JobView is one running background job (desktop/app.go Jobs) for the status bar.
export interface JobView {
  id: string;
  kind: string; // "bash" | "task"
  label: string;
  status: string; // "running"
  startedAt: number; // unix milliseconds
}

export interface PermissionsView {
  mode: string; // "ask" | "allow" | "deny"
  allow: string[];
  ask: string[];
  deny: string[];
}

export interface SandboxView {
  bash: string; // "enforce" | "off"
  network: boolean;
  workspaceRoot: string;
  allowWrite: string[];
}

export interface NetworkProxyView {
  type: string;
  server: string;
  port: number;
  username: string;
  password: string;
}

export interface NetworkView {
  proxyMode: string; // "auto" | "custom" | "off" (backend may still return legacy "env")
  proxyUrl: string;
  noProxy: string;
  proxy: NetworkProxyView;
}

export interface AgentView {
  temperature: number;
  maxSteps: number;
  systemPrompt: string;
}

export interface SettingsView {
  defaultModel: string;
  plannerModel: string;
  providers: ProviderView[];
  permissions: PermissionsView;
  sandbox: SandboxView;
  network: NetworkView;
  agent: AgentView;
  configPath: string;
  providerKinds: string[]; // provider implementations the kernel registered (for the kind picker)
  bypass: boolean; // live YOLO state (runtime-only) — whether approvals are skipped this session
}

// ReferenceFileResult 是 Composer 「📎 上传参考资料」按钮的返回。
// 后端解析任意 Word/PDF/HTML/Markdown/代码 -> 一段可注入对话上下文的文本。
export interface ReferenceFileResult {
  name: string;
  path: string;
  text: string;
  charCount: number;
  truncated: boolean;
  source: "file" | "url" | string;
  formatHint: string;
}

// HardwareRunInput / Result drive the one-click 编译/烧录/看串口 buttons in HardwarePanel.
// The backend dispatches to the right MCP tool by Platform; rootCause + fixHint come
// from hardware_project_validate's error distillation (empty on success).
export interface HardwareRunInput {
  projectDir: string;
  platform: string;
  board?: string;
  port?: string;
  seconds?: number;
  address?: string; // OTA WiFi 烧录:板子地址(IP 或 mDNS 名)
  otaPassword?: string; // OTA WiFi 烧录:ArduinoOTA 口令
}

// 发布固件到远程服务器(③ 云端拉取)的入参,服务器配置留空用 NAS 默认。
export interface HardwarePublishInput {
  projectDir: string;
  board?: string;
  projectName: string;
  version: string;
  sshHost?: string;
  remoteDir?: string;
  baseURL?: string;
}

// 新建 OTA 项目脚手架(A 方案)入参/结果。
export interface OTAScaffoldInput {
  destDir?: string;
  projectName: string;
  mode: "lan" | "web" | "cloud";
  wifiSSID: string;
  wifiPassword: string;
  nasBaseURL?: string;
}
export interface OTAScaffoldResult {
  ok: boolean;
  path?: string;
  error?: string;
}

// 内置文件夹选择器的一页(BrowseDir 返回),绕开 macOS 原生对话框的"开到窗口后面"bug。
export interface FolderListing {
  path: string;
  parent: string;
  dirs: string[];
  home: string;
  desktop: string;
  error?: string;
}

// 账号会话(P1:登录 + 按权限门控)。permissions = 该账号开通的功能 key 列表;超管 isAdmin=true 拥有全部。
export interface AccountTier {
  index: number;
  name: string;
}
export interface AccountSession {
  loggedIn: boolean;
  account: string;
  isAdmin: boolean;
  permissions: string[];
  tiers: AccountTier[]; // 订阅制三档(模型对用户隐藏);超管 / 未配为空
  points: number | null; // 机构点数余额(登录快照);超管 = null 不限
  selectedTier: number; // 当前选中档位 1/2/3
}
export interface AccountLoginResult {
  ok: boolean;
  error?: string;
}

export interface HardwareRunResult {
  status: "passed" | "failed" | "skipped" | string;
  // 验证子类（如 python_syntax）：前端据此区分「真编译通过」与「仅 py_compile 语法通过」。
  kind?: string;
  summary: string;
  output?: string;
  rootCause?: string;
  fixHint?: string;
  nextStep?: string;
  error?: string;
  command?: string;
}

// Auto-updater payloads (desktop/updater.go). UpdateInfo drives the update banner;
// UpdateProgress streams on the "updater:progress" event during ApplyUpdate.
export interface UpdateInfo {
  available: boolean;
  current: string;
  latest: string;
  notes: string;
  canSelfUpdate: boolean; // win/linux true; macOS false (no cert → manual download)
  downloadUrl: string; // human-facing releases page (macOS path / fallback link)
  assetSize: number; // running platform's artifact size, for the progress bar
  err?: string; // set when the check itself failed (both endpoints down)
}

export interface UpdateProgress {
  phase: "downloading" | "verifying" | "applying" | "done" | "error";
  received: number;
  total: number;
  err?: string;
}
