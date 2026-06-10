// bridge is the single seam between the React app and the Go kernel. In the Wails
// shell it calls the bound App methods (window.go.main.App.*) and subscribes to
// the runtime event stream (window.runtime.EventsOn). In a plain browser (`pnpm
// dev` outside the shell) those globals are absent, so it falls back to a mock
// that streams a canned turn through the same contract — letting the whole UI be
// developed and laid out without rebuilding the Go side.

import type {
  BalanceInfo,
  CapabilitiesView,
  CheckpointMeta,
  CommandInfo,
  ContextInfo,
  DirEntry,
  EffortInfo,
  FilePreview,
  HistoryMessage,
  HardwareBoardFactsView,
  HardwareDetectView,
  HardwareEvidenceStatusView,
  HardwareMCPView,
  HardwareRunInput,
  HardwareRunResult,
  ReferenceFileResult,
  JobView,
  KnowledgeBaseView,
  KnowledgeImportResult,
  KnowledgePromptView,
  KnowledgeSearchResult,
  KnowledgeView,
  MCPServerInput,
  MemoryView,
  Meta,
  ModelInfo,
  NetworkView,
  ProviderView,
  QuestionAnswer,
  ServerView,
  SessionMeta,
  SettingsView,
  SkillRootView,
  SkillView,
  SlashArgsResult,
  TabMeta,
  UpdateInfo,
  UpdateProgress,
  WireEvent,
  WorkspaceView,
} from "./types";

// AppBindings mirrors desktop/app.go's exported method set. Keep in sync by hand
// (or regenerate with `wails generate module` and import wailsjs instead).
export interface AppBindings {
  Submit(input: string): Promise<void>;
  SubmitDisplay(display: string, input: string): Promise<void>;
  Cancel(): Promise<void>;
  Approve(id: string, allow: boolean, session: boolean): Promise<void>;
  AnswerQuestion(id: string, answers: QuestionAnswer[]): Promise<void>;
  SetPlanMode(on: boolean): Promise<void>;
  // 设置当前会话的「协作模式」persona(空串=默认):随每个 turn 注入,不进缓存系统前缀。
  SetCoachMode(preamble: string): Promise<void>;
  Compact(): Promise<void>;
  NewSession(): Promise<void>;
  // 多标签多任务(像 Codex / Claude Code):每个标签一个独立 controller + session,
  // 后台标签的 controller 照常在自己的 goroutine 里跑。CreateTab 新建并设为活动;
  // SetActiveTab 在切换标签时把后端「活动镜像」重指到目标标签,既有会话类方法随之作用
  // 到该标签;事件按 agent:event:<tabId> 独立通道走。
  CreateTab(kind: string): Promise<TabMeta>;
  CloseTab(id: string): Promise<void>;
  ListTabs(): Promise<TabMeta[]>;
  SetActiveTab(id: string): Promise<void>;
  History(): Promise<HistoryMessage[]>;
  // Checkpoints lists the session's rewind points; Rewind restores one (scope
  // "code" | "conversation" | "both"), after which the caller re-reads History.
  Checkpoints(): Promise<CheckpointMeta[]>;
  Rewind(turn: number, scope: string): Promise<void>;
  Fork(turn: number): Promise<void>;
  SummarizeFrom(turn: number): Promise<void>;
  SummarizeUpTo(turn: number): Promise<void>;
  // Session history: list saved sessions, resume one (returns its transcript),
  // preview one read-only, delete one, or give one a custom display name ("" clears it).
  ListSessions(): Promise<SessionMeta[]>;
  ResumeSession(path: string): Promise<HistoryMessage[]>;
  PreviewSession(path: string): Promise<HistoryMessage[]>;
  DeleteSession(path: string): Promise<void>;
  RenameSession(path: string, title: string): Promise<void>;
  // Workspace: open a folder chooser and switch to that project (fresh session);
  // returns the chosen path, or "" if cancelled.
  ListWorkspaces(): Promise<WorkspaceView[]>;
  PickWorkspace(): Promise<string>;
  SwitchWorkspace(path: string): Promise<string>;
  ContextUsage(): Promise<ContextInfo>;
  // Balance queries the active provider's wallet balance (a network call);
  // returns an unavailable readout when no balance_url is configured or it fails.
  Balance(): Promise<BalanceInfo>;
  // Jobs lists the running background jobs (bash/task started in the background)
  // for the status-bar indicator.
  Jobs(): Promise<JobView[]>;
  Meta(): Promise<Meta>;
  Commands(): Promise<CommandInfo[]>;
  // Capabilities feeds the MCP & Skills drawer: connected/failed servers + skills.
  // Add connects + persists a server; Remove disconnects + drops it from config;
  // Retry reconnects a configured server that failed (config untouched).
  Capabilities(): Promise<CapabilitiesView>;
  AddMCPServer(input: MCPServerInput): Promise<number>;
  HardwareMCP(): Promise<HardwareMCPView>;
  HardwareDetect(): Promise<HardwareDetectView>;
  HardwareEvidenceStatus(): Promise<HardwareEvidenceStatusView>;
  // 把 tests/hardware_evidence.jsonl 的真机验证记录汇总成可粘进竞赛材料的 Markdown（无记录返回空串）。
  HardwareEvidenceExport(projectDir: string): Promise<string>;
  // 写代码前确定性取出已选板卡的校验事实（电平/引脚/平台 API），供前端注入 prompt。
  HardwareBoardFacts(board: string, platform: string): Promise<HardwareBoardFactsView>;
  PickReferenceFile(): Promise<string>;
  ImportReferenceFile(pathOrURL: string): Promise<ReferenceFileResult>;
  HardwareValidate(input: HardwareRunInput): Promise<HardwareRunResult>;
  HardwareUpload(input: HardwareRunInput): Promise<HardwareRunResult>;
  HardwareMonitor(input: HardwareRunInput): Promise<HardwareRunResult>;
  AddHardwareMCPServer(): Promise<number>;
  KnowledgeView(): Promise<KnowledgeView>;
  KnowledgeCreate(name: string): Promise<KnowledgeBaseView>;
  KnowledgeDelete(id: string): Promise<void>;
  KnowledgeImportFiles(baseID: string): Promise<KnowledgeImportResult>;
  KnowledgeSearch(baseIDs: string[], query: string, limit: number): Promise<KnowledgeSearchResult>;
  KnowledgeBuildPrompt(baseIDs: string[], question: string, limit: number): Promise<KnowledgePromptView>;
  RemoveMCPServer(name: string): Promise<void>;
  RetryMCPServer(name: string): Promise<void>;
  PickSkillFolder(): Promise<string>;
  AddSkillPath(path: string): Promise<void>;
  RemoveSkillPath(path: string): Promise<void>;
  RefreshSkills(): Promise<void>;
  // SetMCPServerEnabled is the per-session connector toggle (on reconnects, off
  // disconnects; config untouched).
  SetMCPServerEnabled(name: string, enabled: boolean): Promise<void>;
  SlashArgs(input: string): Promise<SlashArgsResult>;
  ListDir(rel: string): Promise<DirEntry[]>;
  ReadFile(rel: string): Promise<FilePreview>;
  OpenWorkspacePath(rel: string): Promise<void>;
  RevealWorkspacePath(rel: string): Promise<void>;
  // 在系统文件管理器打开任意绝对路径的文件夹(侧栏「在文件夹中打开」)。
  OpenFolder(path: string): Promise<void>;
  SavePastedImage(dataUrl: string): Promise<string>;
  SavePastedFile(name: string, dataUrl: string): Promise<string>;
  AttachmentDataURL(path: string): Promise<string>;
  Models(): Promise<ModelInfo[]>;
  SetModel(name: string): Promise<void>;
  Effort(): Promise<EffortInfo>;
  SetEffort(level: string): Promise<void>;
  // Memory panel: read the loaded ONECREAT.md hierarchy + saved auto-memories,
  // quick-add a note to a scope's ONECREAT.md (≡ "#<note>"), and overwrite a doc
  // from the in-place editor.
  Memory(): Promise<MemoryView>;
  Remember(scope: string, note: string): Promise<string>;
  Forget(name: string): Promise<void>;
  SaveDoc(path: string, body: string): Promise<string>;
  // Settings panel: read the resolved config and apply edits (each writes config
  // and rebuilds the controller live). Secrets go through SetProviderKey (→ .env).
  Settings(): Promise<SettingsView>;
  SetDefaultModel(ref: string): Promise<void>;
  SetPlannerModel(ref: string): Promise<void>;
  SaveProvider(p: ProviderView): Promise<void>;
  DeleteProvider(name: string): Promise<void>;
  SetProviderKey(apiKeyEnv: string, value: string): Promise<void>;
  SetPermissionMode(mode: string): Promise<void>;
  AddPermissionRule(list: string, rule: string): Promise<void>;
  RemovePermissionRule(list: string, rule: string): Promise<void>;
  SetSandbox(bash: string, network: boolean, workspaceRoot: string, allowWrite: string[]): Promise<void>;
  SetNetwork(n: NetworkView): Promise<void>;
  SetAgentParams(temperature: number, maxSteps: number, systemPrompt: string): Promise<void>;
  // SetBypass toggles YOLO mode (auto-approve every tool call this session; deny
  // rules still apply). Runtime-only — not written to config.
  SetBypass(on: boolean): Promise<void>;
  // Auto-updater (desktop/updater_app.go): the injected build version, a manifest
  // check, applying an update (win/linux self-update; macOS opens the download
  // page), and opening that page directly. Progress streams on "updater:progress".
  Version(): Promise<string>;
  CheckUpdate(): Promise<UpdateInfo | null>;
  ApplyUpdate(): Promise<void>;
  OpenDownloadPage(): Promise<void>;
}

interface WailsRuntime {
  EventsOn(name: string, cb: (...data: unknown[]) => void): () => void;
  BrowserOpenURL(url: string): void;
}

declare global {
  interface Window {
    runtime?: WailsRuntime;
    go?: { main?: { App?: AppBindings } };
  }
}

// Must match desktop/app.go's eventChannel constant.
const EVENT_CHANNEL = "agent:event";

// Resolve the Wails binding at CALL time, not module-load time: in dev the Wails
// runtime can inject window.go AFTER this module first evaluates, so snapshotting
// once would pin the browser mock for the whole session (and show fake data — the
// dev mock's model list leaking into the real app was exactly this bug).
function realApp(): AppBindings | undefined {
  return typeof window !== "undefined" ? window.go?.main?.App : undefined;
}

let mockSingleton: AppBindings | null = null;
function getMock(): AppBindings {
  if (!mockSingleton) mockSingleton = makeMockApp();
  return mockSingleton;
}

// onEvent subscribes to one tab's typed event stream (agent:event:<tabId>);
// returns an unsubscribe. 每个标签独立通道,所以后台标签的事件互不串扰。
export function onEvent(tabId: string, cb: (e: WireEvent) => void): () => void {
  if (realApp() && typeof window !== "undefined" && window.runtime) {
    return window.runtime.EventsOn(`${EVENT_CHANNEL}:${tabId}`, (payload) => cb(payload as WireEvent));
  }
  return mockSubscribe(cb);
}

// onUpdaterProgress subscribes to the auto-updater's progress events (a separate
// channel from the agent stream); returns an unsubscribe. Must match the event
// name emitted in desktop/updater_app.go.
export function onUpdaterProgress(cb: (p: UpdateProgress) => void): () => void {
  if (realApp() && typeof window !== "undefined" && window.runtime) {
    return window.runtime.EventsOn("updater:progress", (p) => cb(p as UpdateProgress));
  }
  updaterListeners.add(cb);
  return () => {
    updaterListeners.delete(cb);
  };
}

// onReady subscribes to one tab's agent:ready:<tabId> event, fired when that
// tab's boot.Build completes. The frontend re-fetches Meta/Context/History then.
export function onReady(tabId: string, cb: () => void): () => void {
  if (realApp() && typeof window !== "undefined" && window.runtime) {
    return window.runtime.EventsOn(`agent:ready:${tabId}`, () => cb());
  }
  // In dev mock, fire immediately since there's no real boot sequence.
  cb();
  return () => {};
}

// app proxies each call to the live binding (or the dev mock only when truly
// outside the shell), so a late-injected window.go is picked up transparently.
export const app: AppBindings = new Proxy({} as AppBindings, {
  get(_t, prop) {
    const target = realApp() ?? getMock();
    const v = (target as unknown as Record<string, unknown>)[String(prop)];
    return typeof v === "function" ? (v as (...a: unknown[]) => unknown).bind(target) : v;
  },
});

// openExternal opens a URL in the system browser (so links in rendered markdown
// don't navigate the webview away from the app). Falls back to window.open in the
// browser dev mock.
export function openExternal(url: string): void {
  if (typeof window !== "undefined" && window.runtime?.BrowserOpenURL) {
    window.runtime.BrowserOpenURL(url);
  } else if (typeof window !== "undefined") {
    window.open(url, "_blank", "noopener");
  }
}

// --- browser dev mock --------------------------------------------------------

const listeners = new Set<(e: WireEvent) => void>();

function mockSubscribe(cb: (e: WireEvent) => void): () => void {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
}

function emit(e: WireEvent) {
  listeners.forEach((l) => l(e));
}

// Updater progress has its own listener set so the browser dev mock's ApplyUpdate
// can stream a fake download through onUpdaterProgress.
const updaterListeners = new Set<(p: UpdateProgress) => void>();

function emitUpdater(p: UpdateProgress) {
  updaterListeners.forEach((l) => l(p));
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

function makeMockApp(): AppBindings {
  let cancelled = false;
  let cwd = "~/projects/reasonix"; // mutable so PickWorkspace is visible in dev
  let workspaces = [
    "~/projects/reasonix",
    "~/Documents/hardware/esp32_snake_web",
    "~/Documents/hardware/hardware_lessons",
    "~/projects/blade",
    "~/projects/deepseek-forge",
    "~/projects/cc-switch-light",
    "~/projects/SuperRig",
  ];
  let mockEffort = "auto";
  const day = 86_400_000;
  const t0 = Date.now();
  // Mutable so MCP add/remove/retry are observable in browser dev.
  let capServers: ServerView[] = [
    {
      name: "codegraph",
      transport: "stdio",
      status: "connected",
      tools: 4,
      prompts: 0,
      resources: 1,
      toolList: [
        { name: "search", description: "Search symbols, files, and text in the workspace." },
        { name: "context", description: "Fetch surrounding source context for a symbol or file." },
        { name: "trace", description: "Follow callers and callees across the code graph." },
        { name: "node", description: "Inspect a specific graph node." },
      ],
    },
    { name: "github", transport: "stdio", status: "connected", tools: 12, prompts: 2, resources: 0 },
    { name: "linear", transport: "http", status: "connected", tools: 8, prompts: 0, resources: 0 },
    { name: "figma", transport: "http", status: "failed", tools: 0, prompts: 0, resources: 0, error: "connect: 401 unauthorized" },
  ];
  const capSkills: SkillView[] = [
    { name: "explore", description: "Investigate the codebase in an isolated subagent", scope: "builtin", runAs: "subagent" },
    { name: "review", description: "Review the staged diff", scope: "project", runAs: "inline" },
    { name: "init", description: "Scaffold a ONECREAT.md for this repo", scope: "builtin", runAs: "inline" },
  ];
  let capSkillRoots: SkillRootView[] = [
    { dir: "~/projects/reasonix/.reasonix/skills", scope: "project", priority: 1, status: "missing", configured: false, skills: 0 },
    { dir: "~/my-skills", scope: "custom", priority: 5, status: "ok", configured: true, skills: 1 },
    { dir: "~/.reasonix/skills", scope: "global", priority: 6, status: "ok", configured: false, skills: 2 },
  ];
  const mockSwitchWorkspace = async (path: string) => {
    cwd = path || "~";
    workspaces = [cwd, ...workspaces.filter((p) => p !== cwd)].slice(0, 12);
    return cwd;
  };
  // Mutable so delete/rename are observable in browser dev.
  const sessions: SessionMeta[] = [
    { path: "/mock/sessions/a.jsonl", preview: "fix the login bug in auth.go", turns: 12, createdAt: t0 - 2 * day, lastActivityAt: t0 - 3_600_000, modTime: t0 - 3_600_000, current: true },
    { path: "/mock/sessions/b.jsonl", preview: "refactor the payment module", turns: 5, createdAt: t0 - 3 * day, lastActivityAt: t0 - 6 * 3_600_000, modTime: t0 - 6 * 3_600_000, current: false },
    { path: "/mock/sessions/c.jsonl", preview: "write the README and badges", turns: 8, createdAt: t0 - 4 * day, lastActivityAt: t0 - day - 3_600_000, modTime: t0 - day - 3_600_000, current: false },
    { path: "/mock/sessions/d.jsonl", preview: "explain the plugin host design", turns: 3, createdAt: t0 - 5 * day, lastActivityAt: t0 - 4 * day, modTime: t0 - 4 * day, current: false },
  ];
  // Mutable settings so the Settings panel's edits are observable in browser dev.
  const settings: SettingsView = {
    defaultModel: "deepseek-flash",
    plannerModel: "",
    providers: [
      { name: "deepseek-flash", kind: "openai", baseUrl: "https://api.deepseek.com", models: ["deepseek-v4-flash"], default: "deepseek-v4-flash", apiKeyEnv: "DEEPSEEK_API_KEY", keySet: true, balanceUrl: "https://api.deepseek.com/user/balance", contextWindow: 1_000_000 },
      { name: "mimo-pro", kind: "openai", baseUrl: "https://api.xiaomimimo.com/v1", models: ["mimo-v2.5-pro"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_API_KEY", keySet: false, balanceUrl: "", contextWindow: 1_000_000 },
    ],
    permissions: { mode: "ask", allow: ["ls", "read_file"], ask: [], deny: ["bash(rm *)"] },
    sandbox: { bash: "enforce", network: true, workspaceRoot: "", allowWrite: [] },
    network: {
      proxyMode: "auto",
      proxyUrl: "",
      noProxy: "",
      proxy: { type: "socks5", server: "127.0.0.1", port: 7890, username: "", password: "" },
    },
    agent: { temperature: 0.2, maxSteps: 0, systemPrompt: "You are onecreat, a coding agent." },
    configPath: "~/projects/reasonix/reasonix.toml",
    providerKinds: ["openai"],
    bypass: false,
  };
  const now = Date.now();
  const mockKnowledge: KnowledgeView = {
    storeDir: "~/.config/onecreat/knowledge",
    mode: "模式 A：本地存储 + 客户自填 API；当前检索在本机完成，只把命中片段加入本次请求。",
    supportedExtensions: [".txt", ".md", ".json", ".py", ".ino", ".cpp", ".ts", ".tsx"],
    bases: [
      {
        id: "kb_mock_hardware",
        name: "客户硬件资料",
        createdAt: now - day,
        updatedAt: now - 3600_000,
        documents: 2,
        chunks: 4,
      },
    ],
    documents: [
      {
        id: "doc_mock_uart",
        baseId: "kb_mock_hardware",
        name: "esp32_uart.md",
        originalPath: "~/Documents/esp32_uart.md",
        storedPath: "~/.config/onecreat/knowledge/files/kb_mock_hardware/doc_mock_uart.md",
        size: 2048,
        importedAt: now - 3600_000,
        status: "ready",
        chunks: 2,
      },
      {
        id: "doc_mock_wiring",
        baseId: "kb_mock_hardware",
        name: "wiring.md",
        originalPath: "~/Documents/esp32_snake_web/docs/wiring.md",
        storedPath: "~/.config/onecreat/knowledge/files/kb_mock_hardware/doc_mock_wiring.md",
        size: 1536,
        importedAt: now - 1800_000,
        status: "ready",
        chunks: 2,
      },
    ],
  };

  const cloneKnowledge = (): KnowledgeView => JSON.parse(JSON.stringify(mockKnowledge)) as KnowledgeView;
  const rebuildKnowledgeCounts = () => {
    mockKnowledge.bases = mockKnowledge.bases.map((base) => {
      const docs = mockKnowledge.documents.filter((doc) => doc.baseId === base.id && doc.status === "ready");
      return {
        ...base,
        documents: docs.length,
        chunks: docs.reduce((sum, doc) => sum + doc.chunks, 0),
        updatedAt: docs[0]?.importedAt ?? base.updatedAt,
      };
    });
  };
  return {
    async Submit(input) {
      cancelled = false;
      emit({ kind: "turn_started" });
      if (input.toLowerCase().includes("progress")) {
        emit({
          kind: "tool_dispatch",
          tool: {
            id: "todo-dev",
            name: "todo_write",
            args: JSON.stringify({
              todos: [
                { content: "Read docs/wiring.md and src/main.cpp", status: "completed" },
                {
                  content: "Audit docs/board_profile.md and PlatformIO layout",
                  activeForm: "Auditing ESP32 project context",
                  status: "in_progress",
                },
                { content: "Verify hardware workspace with platformio.ini and tests/hardware_checklist.md", status: "pending" },
              ],
            }),
            readOnly: false,
          },
        });
      }
      if (input.toLowerCase().includes("approval")) {
        emit({
          kind: "approval_request",
          approval: {
            id: "approval-dev",
            tool: "edit_file",
            subject: 'path: src/main.cpp\nold_string: "Serial.println(\\"hi\\");"\nnew_string: "Serial.println(\\"hello\\");"\nreference: docs/wiring.md',
          },
        });
      }
      if (input.toLowerCase().includes("ask")) {
        emit({
          kind: "ask_request",
          ask: {
            id: "ask-dev",
            questions: [
              {
                id: "board",
                header: "Board",
                prompt: "这次硬件项目使用哪块板卡？",
                options: [
                  { label: "ESP32", description: "WiFi 项目、网页控制和轻量边缘推理" },
                  { label: "Arduino Nano", description: "传感器采集、电机控制和简单执行层" },
                ],
              },
              {
                id: "port",
                header: "Port",
                prompt: "是否已经确认上传端口？",
                options: [
                  { label: "已确认", description: "可以进入接线图和代码生成" },
                  { label: "还没有", description: "先打开端口检测和驱动提示" },
                ],
              },
            ],
          },
        });
      }
      // Simulate the server's pre-first-token latency so the deferred user bubble
      // and the "un-send on Esc before any reply" path are observable in browser
      // dev. Bail if cancelled during the wait — nothing was streamed yet.
      await delay(700);
      if (cancelled) return;
      const reply =
        `You said: **${input}**\n\n` +
        "This is the browser dev mock — the real reply comes from the kernel " +
        "inside the Wails shell. Here's a fenced block to exercise the editor seam:\n\n" +
        "```go\nfunc main() {\n    println(\"hello from the mock\")\n}\n```\n";
      for (const ch of reply) {
        if (cancelled) break;
        emit({ kind: "text", text: ch });
        await delay(6);
      }
      emit({ kind: "message", text: reply });
      emit({
        kind: "tool_dispatch",
        tool: {
          id: "t1",
          name: "edit_file",
          args: '{"path":"src/main.cpp","old_string":"Serial.println(\\"hi\\");","new_string":"Serial.println(\\"hello\\");"}',
          readOnly: false,
        },
      });
      await delay(350);
      emit({
        kind: "tool_result",
        tool: { id: "t1", name: "edit_file", output: "edited src/main.cpp", readOnly: false },
      });
      emit({
        kind: "usage",
        usage: {
          promptTokens: 1280,
          completionTokens: 64,
          totalTokens: 1344,
          cacheHitTokens: 1024,
          cacheMissTokens: 256,
          sessionCacheHitTokens: 1024,
          sessionCacheMissTokens: 256,
        },
      });
      emit({ kind: "turn_done" });
    },
    async SubmitDisplay(_display, input) {
      await this.Submit(input);
    },
    async Cancel() {
      cancelled = true;
      emit({ kind: "turn_done" });
    },
    async Approve() {},
    async AnswerQuestion() {},
    async SetPlanMode() {},
    async SetCoachMode() {},
    async Compact() {},
    async NewSession() {},
    async CreateTab(kind: string) {
      return { id: `tab${Date.now()}`, kind: kind || "chat", label: "", ready: true, active: true };
    },
    async CloseTab() {},
    async ListTabs() {
      return [{ id: "main", kind: "chat", label: "", ready: true, active: true }];
    },
    async SetActiveTab() {},
    async Checkpoints() {
      return [];
    },
    async Rewind() {},
    async Fork() {},
    async SummarizeFrom() {},
    async SummarizeUpTo() {},
    async History() {
      return [];
    },
    async ListSessions() {
      return sessions.map((s) => ({ ...s }));
    },
    async ResumeSession(path: string) {
      sessions.forEach((s) => {
        s.current = s.path === path;
      });
      return [
        { role: "user", content: `(mock) resumed ${path}` },
        { role: "assistant", content: "This is a mock resumed transcript — the real one comes from the kernel." },
      ];
    },
    async PreviewSession(path: string) {
      const s = sessions.find((x) => x.path === path);
      return [
        { role: "user", content: s?.preview || `(mock) preview ${path}` },
        {
          role: "assistant",
          content: "This is a read-only mock preview. The active conversation is unchanged.",
          reasoning: "Preview reads the saved session without resuming it.",
        },
      ];
    },
    async DeleteSession(path: string) {
      const i = sessions.findIndex((s) => s.path === path);
      if (i >= 0) sessions.splice(i, 1);
    },
    async RenameSession(path: string, title: string) {
      const s = sessions.find((x) => x.path === path);
      if (s) s.title = title.trim() || undefined;
    },
    async ListWorkspaces() {
      return workspaces.map((path) => ({
        path,
        name: path.split("/").filter(Boolean).pop() ?? path,
        current: path === cwd,
      }));
    },
    async PickWorkspace() {
      // Browser dev has no native dialog; simulate picking a folder and re-root so
      // the topbar folder chip visibly changes.
      return mockSwitchWorkspace(cwd.endsWith("another-project") ? "~/projects/reasonix" : "~/projects/another-project");
    },
    async SwitchWorkspace(path: string) {
      return mockSwitchWorkspace(path);
    },
    async ContextUsage() {
      return { used: 1280, window: 1_000_000 };
    },
    async Balance() {
      // Mirror the active mock provider: deepseek-flash carries a balance_url.
      const p = settings.providers.find((x) => x.name === settings.defaultModel);
      if (!p?.balanceUrl) return { available: false, display: "" };
      return { available: true, display: "¥128.50" };
    },
    async Jobs() {
      return []; // browser dev mock has no background jobs
    },
    async Meta() {
      return {
        label: "mock model · browser dev",
        ready: true,
        eventChannel: EVENT_CHANNEL,
        cwd,
        bypass: settings.bypass,
      };
    },
    async Commands() {
      return [
        { name: "new", description: "Start a new session", kind: "builtin" as const },
        { name: "compact", description: "Summarize older history to free up context", kind: "builtin" as const },
        { name: "model", description: "Switch model", kind: "builtin" as const },
        { name: "effort", description: "Set reasoning effort", kind: "builtin" as const },
        { name: "skill", description: "List skills", kind: "builtin" as const },
        { name: "explore", description: "Investigate the codebase in an isolated subagent", kind: "skill" as const },
        { name: "review", description: "Review the staged diff", hint: "[focus]", kind: "custom" as const },
      ];
    },
    async Capabilities() {
      return {
        servers: capServers.map((s) => ({ ...s })),
        skills: capSkills.map((s) => ({ ...s })),
        skillRoots: capSkillRoots.map((s) => ({ ...s })),
      };
    },
    async AddMCPServer(input: MCPServerInput) {
      const tools = input.transport === "stdio" ? 3 : 5;
      capServers.push({
        name: input.name,
        transport: input.transport,
        status: "connected",
        tools,
        prompts: 0,
        resources: 0,
        toolList: Array.from({ length: tools }, (_, i) => ({
          name: `${input.name}_tool_${i + 1}`,
          description: `Mock tool ${i + 1} exposed by ${input.name}.`,
        })),
      });
      return tools;
    },
    async HardwareMCP() {
      const server = capServers.find((s) => s.name === "hardware");
      return {
        name: "hardware",
        available: true,
        command: "reasonix-hardware-mcp",
        source: "browser mock",
        configured: Boolean(server),
        connected: server?.status === "connected",
        error: server?.error,
      };
    },
    async HardwareDetect() {
      return {
        available: true,
        workspace: cwd,
        projectDir: cwd,
        projectTypes: ["platformio"],
        serialPorts: ["/dev/cu.usbserial-0001"],
        boards: [
          {
            port: "/dev/cu.usbserial-0001",
            protocol: "serial",
            boardName: "ESP32 Dev Module",
            fqbn: "esp32:esp32:esp32",
            core: "esp32:esp32",
          },
        ],
        devices: [
          {
            port: "/dev/cu.usbserial-0001",
            description: "USB Serial",
            hwid: "USB VID:PID=10C4:EA60",
          },
        ],
        toolchains: [
          {
            name: "arduino-cli",
            command: "arduino-cli version",
            available: true,
            path: "/usr/local/bin/arduino-cli",
            version: "arduino-cli Version: 1.3.1",
          },
          {
            name: "PlatformIO",
            command: "pio --version",
            available: true,
            path: "/usr/local/bin/pio",
            version: "PlatformIO Core, version 6.1.20",
          },
          {
            name: "ESP-IDF idf.py",
            command: "idf.py --version",
            available: false,
            hint: "Install ESP-IDF, then activate the environment before launching onecreat.",
          },
        ],
        recommendations: ["ESP-IDF 工程建议优先接入官方 Tools MCP。"],
        espIdfOfficialMcp: {},
      };
    },
    async HardwareBoardFacts() {
      return {
        found: true,
        facts:
          "板卡：ESP32 Dev Module（FQBN esp32:esp32:esp32）\n" +
          "逻辑电平：3.3V。5V 传感器输出接 ESP32 输入脚前必须分压/电平转换。\n" +
          "平台 API（ESP32 Arduino）——务必照此写：PWM 用 LEDC（core 3.x 用 ledcAttach），别用 analogWrite；舵机用 ESP32Servo。",
      };
    },
    async HardwareEvidenceStatus() {
      return {
        available: true,
        projectDir: cwd,
        platform: "platformio",
        board: "esp32dev",
        evidenceFile: `${cwd}/tests/hardware_evidence.jsonl`,
        recordCount: 1,
        currentRecordCount: 1,
        staleRecordCount: 0,
        status: "hardware_pending",
        summary: "Local compile passed; real-device upload and monitor evidence are still missing.",
        missingGroups: ["real_device"],
        recommendations: ["连接真实开发板后执行 upload/monitor，并用 hardware_evidence_record 保存输出。"],
      };
    },
    async HardwareEvidenceExport() {
      return [
        "# 真机验证证据（onecreat 自动导出）",
        "",
        "共 1 条验证记录。",
        "",
        "## 1. 【编译/语法】passed",
        "- 时间（UTC）：2026-06-06T04:30:00Z",
        "- 平台 / 板卡：platformio / esp32dev",
        "- 结果：1 passed, 0 failed",
      ].join("\n");
    },
    async PickReferenceFile() {
      return "";
    },
    async ImportReferenceFile(pathOrURL: string) {
      return {
        name: pathOrURL.split("/").pop() || pathOrURL,
        path: pathOrURL,
        text: "(dev mock — 这里会是文件解析后的文本)",
        charCount: 30,
        truncated: false,
        source: "file",
        formatHint: "txt",
      } as ReferenceFileResult;
    },
    async HardwareValidate(_input: HardwareRunInput) {
      return {
        status: "passed",
        summary: "1 passed, 0 failed, 0 skipped (dev mock)",
      } as HardwareRunResult;
    },
    async HardwareUpload(_input: HardwareRunInput) {
      return { status: "skipped", summary: "dev mock — no device attached" } as HardwareRunResult;
    },
    async HardwareMonitor(_input: HardwareRunInput) {
      return { status: "skipped", summary: "dev mock — no device attached" } as HardwareRunResult;
    },
    async AddHardwareMCPServer() {
      return this.AddMCPServer({
        name: "hardware",
        transport: "stdio",
        command: "reasonix-hardware-mcp",
        args: [],
        url: "",
        env: {},
      });
    },
    async KnowledgeView() {
      rebuildKnowledgeCounts();
      return cloneKnowledge();
    },
    async KnowledgeCreate(name: string) {
      const trimmed = name.trim() || "未命名知识库";
      const base: KnowledgeBaseView = {
        id: `kb_mock_${Date.now()}`,
        name: trimmed,
        createdAt: Date.now(),
        updatedAt: Date.now(),
        documents: 0,
        chunks: 0,
      };
      mockKnowledge.bases.unshift(base);
      return { ...base };
    },
    async KnowledgeDelete(id: string) {
      mockKnowledge.bases = mockKnowledge.bases.filter((base) => base.id !== id);
      mockKnowledge.documents = mockKnowledge.documents.filter((doc) => doc.baseId !== id);
    },
    async KnowledgeImportFiles(baseID: string) {
      const doc = {
        id: `doc_mock_${Date.now()}`,
        baseId: baseID,
        name: "mock_hardware_notes.md",
        originalPath: "~/Documents/mock_hardware_notes.md",
        storedPath: `${mockKnowledge.storeDir}/files/${baseID}/mock_hardware_notes.md`,
        size: 1536,
        importedAt: Date.now(),
        status: "ready",
        chunks: 1,
      };
      mockKnowledge.documents.unshift(doc);
      rebuildKnowledgeCounts();
      return { imported: [{ ...doc }], skipped: [] };
    },
    async KnowledgeSearch(baseIDs: string[], query: string, limit: number) {
      const selected = new Set(baseIDs.filter(Boolean));
      const docs = mockKnowledge.documents.filter((doc) => selected.size === 0 || selected.has(doc.baseId));
      const matches = docs.slice(0, Math.max(1, limit || 5)).map((doc, index) => {
        const base = mockKnowledge.bases.find((item) => item.id === doc.baseId);
        return {
          baseId: doc.baseId,
          baseName: base?.name || "知识库",
          documentId: doc.id,
          documentName: doc.name,
          chunkId: `${doc.id}:0`,
          chunkIndex: 0,
          text: `Mock 命中：${query || "ESP32 UART"}。ESP32 与 Unihiker 可以通过 UART 以 115200 波特率通信，资料只来自本机导入文件。`,
          score: 10 - index,
        };
      });
      return { query, matches };
    },
    async KnowledgeBuildPrompt(baseIDs: string[], question: string, limit: number) {
      const search = await this.KnowledgeSearch(baseIDs, question, limit);
      if (!search.matches.length) return { prompt: question, sources: [] };
      return {
        prompt:
          "你正在回答用户问题。下面是用户在 onecreat 知识库中显式选择的本地资料片段。\n\n" +
          search.matches.map((m, index) => `[${index + 1}] ${m.baseName} / ${m.documentName}\n${m.text}`).join("\n\n") +
          `\n\n# 用户问题\n${question}`,
        sources: search.matches,
      };
    },
    async RemoveMCPServer(name: string) {
      capServers = capServers.filter((s) => s.name !== name);
    },
    async RetryMCPServer(name: string) {
      capServers = capServers.map((s) =>
        s.name === name ? { ...s, status: "connected", tools: s.tools || 4, error: undefined } : s,
      );
    },
    async PickSkillFolder() {
      return "~/my-skills";
    },
    async AddSkillPath(path: string) {
      const dir = path.trim() || "~/my-skills";
      if (!capSkillRoots.some((r) => r.scope === "custom" && r.dir === dir)) {
        capSkillRoots.push({ dir, scope: "custom", priority: capSkillRoots.length + 1, status: "ok", configured: true, skills: 1 });
      }
      if (!capSkills.some((s) => s.name === "local-dev")) {
        capSkills.push({ name: "local-dev", description: "Local custom development workflow", scope: "custom", runAs: "inline" });
      }
    },
    async RemoveSkillPath(path: string) {
      capSkillRoots = capSkillRoots.filter((r) => !(r.scope === "custom" && r.dir === path));
      if (!capSkillRoots.some((r) => r.scope === "custom")) {
        const idx = capSkills.findIndex((s) => s.name === "local-dev");
        if (idx >= 0) capSkills.splice(idx, 1);
      }
    },
    async RefreshSkills() {},
    async SetMCPServerEnabled(name: string, enabled: boolean) {
      capServers = capServers.map((s) =>
        s.name === name
          ? { ...s, status: enabled ? "connected" : "disabled", tools: enabled ? s.tools || 4 : 0, error: undefined }
          : s,
      );
    },
    async SlashArgs(input: string) {
      // Mirror a slice of the real arg hints so the menu is exercisable in browser dev.
      const from = input.lastIndexOf(" ") + 1;
      const cur = input.slice(from);
      const cmd = input.slice(0, input.indexOf(" ") < 0 ? input.length : input.indexOf(" "));
      const subs: Record<string, { label: string; insert: string; hint: string; descend?: boolean }[]> = {
        "/skill": [
          { label: "list", insert: "list", hint: "list skills" },
          { label: "show", insert: "show ", hint: "show a skill's body", descend: true },
          { label: "new", insert: "new ", hint: "scaffold a new skill" },
          { label: "paths", insert: "paths", hint: "show discovery paths" },
        ],
        "/hooks": [
          { label: "list", insert: "list", hint: "list active hooks" },
          { label: "trust", insert: "trust", hint: "trust this project's hooks" },
        ],
        "/model": [
          { label: "deepseek/deepseek-v4-flash", insert: "deepseek/deepseek-v4-flash", hint: "current" },
          { label: "deepseek/deepseek-v4-pro", insert: "deepseek/deepseek-v4-pro", hint: "" },
        ],
        "/effort": [
          { label: "auto", insert: "auto", hint: "use the model default" },
          { label: "high", insert: "high", hint: "deeper reasoning" },
          { label: "max", insert: "max", hint: "maximum reasoning" },
        ],
      };
      const items = (subs[cmd] ?? [])
        .filter((it) => it.label.toLowerCase().startsWith(cur.toLowerCase()))
        .map((it) => ({ label: it.label, insert: it.insert, hint: it.hint, descend: it.descend ?? false }));
      return { items, from };
    },
    async ListDir(rel: string) {
      // A tiny fake tree so the @ menu is navigable in browser dev.
      if (rel === "" || rel === "./") {
        return [
          { name: "internal", isDir: true },
          { name: "desktop", isDir: true },
          { name: "README.md", isDir: false },
          { name: "go.mod", isDir: false },
        ];
      }
      if (rel === "internal/") {
        return [
          { name: "control", isDir: true },
          { name: "boot", isDir: true },
          { name: "event.go", isDir: false },
        ];
      }
      return [{ name: "file.go", isDir: false }];
    },
    async ReadFile(rel: string) {
      const samples: Record<string, string> = {
        "README.md": "# onecreat\n\nBrowser-dev workspace preview.\n\n- Chat in the center\n- Browse files on the right\n- Keep sessions on the left\n",
        "go.mod": "module reasonix\n\ngo 1.23\n",
        "desktop/file.go": "package desktop\n\nfunc main() {\n\tprintln(\"workspace preview\")\n}\n",
        "internal/event.go": "package internal\n\n// mock file used by the browser dev seam\n",
      };
      return {
        path: rel,
        body: samples[rel] ?? `// ${rel}\n\nMock file body from browser dev.`,
        size: samples[rel]?.length ?? 42,
        truncated: false,
        binary: false,
      };
    },
    async OpenWorkspacePath(rel: string) {
      console.info("mock OpenWorkspacePath", rel);
    },
    async OpenFolder(path: string) {
      console.info("mock OpenFolder", path);
    },
    async RevealWorkspacePath(rel: string) {
      console.info("mock RevealWorkspacePath", rel);
    },
    async SavePastedImage(_dataUrl: string) {
      return ".reasonix/attachments/mock.png";
    },
    async SavePastedFile(name: string, _dataUrl: string) {
      return `.reasonix/attachments/mock-${name}`;
    },
    async AttachmentDataURL(_path: string) {
      return "data:image/png;base64,iVBORw0KGgo=";
    },
    async Models() {
      return [
        { ref: "deepseek/deepseek-v4-flash", provider: "deepseek", model: "deepseek-v4-flash", current: true },
        { ref: "deepseek/deepseek-v4-pro", provider: "deepseek", model: "deepseek-v4-pro", current: false },
      ];
    },
    async SetModel() {},
    async Effort() {
      return { supported: true, current: mockEffort, default: "high", levels: ["auto", "high", "max"] };
    },
    async SetEffort(level: string) {
      mockEffort = level || "auto";
    },
    async Memory() {
      return {
        available: true,
        storeDir: "~/.config/reasonix/projects/-mock/memory",
        docs: [
          {
            path: "ONECREAT.md",
            scope: "project",
            body: "# onecreat project memory\n\nMock doc shown in the browser dev seam.\n\n## Notes\n\n- prefers concise replies",
          },
          {
            path: "~/.config/reasonix/ONECREAT.md",
            scope: "user",
            body: "# User memory\n\nAlways respond in 中文.",
          },
        ],
        facts: [
          {
            name: "prefers-tabs",
            description: "User prefers tabs",
            type: "user",
            body: "Indent with tabs.",
          },
        ],
        scopes: [
          { scope: "user", path: "~/.config/reasonix/ONECREAT.md" },
          { scope: "project", path: "ONECREAT.md" },
          { scope: "local", path: "ONECREAT.local.md" },
        ],
      };
    },
    async Remember(scope: string, note: string) {
      emit({ kind: "notice", level: "info", text: `remembered → ${scope}` });
      return `${scope} ONECREAT.md (mock): ${note}`;
    },
    async Forget(name: string) {
      emit({ kind: "notice", level: "info", text: `forgot → ${name}` });
    },
    async SaveDoc(path: string, _body: string) {
      emit({ kind: "notice", level: "info", text: `saved → ${path}` });
      return path;
    },
    async Settings() {
      return JSON.parse(JSON.stringify(settings)) as SettingsView;
    },
    async SetDefaultModel(ref: string) {
      settings.defaultModel = ref;
    },
    async SetPlannerModel(ref: string) {
      settings.plannerModel = ref;
    },
    async SaveProvider(p: ProviderView) {
      const i = settings.providers.findIndex((x) => x.name === p.name);
      if (i >= 0) settings.providers[i] = p;
      else settings.providers.push(p);
    },
    async DeleteProvider(name: string) {
      settings.providers = settings.providers.filter((p) => p.name !== name);
    },
    async SetProviderKey(apiKeyEnv: string) {
      settings.providers.forEach((p) => {
        if (p.apiKeyEnv === apiKeyEnv) p.keySet = true;
      });
    },
    async SetPermissionMode(mode: string) {
      settings.permissions.mode = mode;
    },
    async AddPermissionRule(list: string, rule: string) {
      const k = list as "allow" | "ask" | "deny";
      if (settings.permissions[k] && !settings.permissions[k].includes(rule)) settings.permissions[k].push(rule);
    },
    async RemovePermissionRule(list: string, rule: string) {
      const k = list as "allow" | "ask" | "deny";
      settings.permissions[k] = settings.permissions[k].filter((r) => r !== rule);
    },
	    async SetSandbox(bash: string, network: boolean, workspaceRoot: string, allowWrite: string[]) {
	      settings.sandbox = { bash, network, workspaceRoot, allowWrite };
	    },
	    async SetNetwork(n: NetworkView) {
	      settings.network = n;
	    },
    async SetAgentParams(temperature: number, maxSteps: number, systemPrompt: string) {
      settings.agent = { temperature, maxSteps, systemPrompt };
    },
    async SetBypass(on: boolean) {
      settings.bypass = on;
    },
    async Version() {
      return "v1.0.0 (browser dev)";
    },
    async CheckUpdate() {
      // Browser dev preview should not show fake update prompts in the product UI.
      return null;
    },
    async ApplyUpdate() {
      const total = 12_345_678;
      for (let r = 0; r <= total; r += 1_800_000) {
        emitUpdater({ phase: "downloading", received: Math.min(r, total), total });
        await delay(120);
      }
      emitUpdater({ phase: "verifying", received: total, total });
      await delay(500);
      emitUpdater({ phase: "applying", received: total, total });
      await delay(500);
      emitUpdater({ phase: "done", received: total, total });
      // The real shell relaunches here; the mock just stops.
    },
    async OpenDownloadPage() {
      if (typeof window !== "undefined") {
        window.open("https://github.com/esengine/reasonix/releases/latest", "_blank", "noopener");
      }
    },
  };
}
