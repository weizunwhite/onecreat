import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, KeyboardEvent, PointerEvent as ReactPointerEvent } from "react";
import {
  SquarePen,
  BookOpen,
  Brain,
  Blocks,
  Cpu,
  Eye,
  FolderClosed,
  FolderOpen,
  ChevronDown,
  ChevronRight,
  History,
  MessageSquare,
  MoreHorizontal,
  Pin,
  PinOff,
  Pencil,
  Trash2,
  Settings as SettingsIcon,
  LogOut,
  PanelLeftClose,
  PanelLeftOpen,
  PanelRightClose,
  PanelRightOpen,
  Plus,
  X,
} from "lucide-react";
import logo from "./assets/onecreat-logo.png";
import { useT, t as i18nT } from "./lib/i18n";
import type { DictKey } from "./locales/en";
import { useController } from "./lib/useController";

// 协作模式 persona:label/desc 走 i18n;preamble 是发给模型的中文指令(同首页卡片
// prompt 的决策——产品中文优先,不入 i18n)。空 preamble = 默认无角色。
const COACH_MODES: { key: string; labelKey: DictKey; descKey: DictKey; preamble: string }[] = [
  { key: "", labelKey: "coach.default", descKey: "coach.defaultDesc", preamble: "" },
  {
    key: "student",
    labelKey: "coach.student",
    descKey: "coach.studentDesc",
    preamble:
      "你正在辅导一名 1-9 年级的学生做科技创新项目。用引导式教学:多用提问启发他自己思考,不要直接给出完整代码或现成答案;每讲一步都让学生理解并能复述这一步在做什么、为什么这么做;鼓励为主、允许试错。牢记:学生必须能在答辩时逐行解释自己的项目,AI 只是辅助的手,不能替他完成思考和采集数据。",
  },
  {
    key: "teacher",
    labelKey: "coach.teacher",
    descKey: "coach.teacherDesc",
    preamble:
      "你正在帮一线科技教育老师备课、准备项目材料。可以直接产出完整、可用的内容,但每个关键技术点都要附一句「为什么这么做」的教学解释,方便老师讲给学生;用词通俗,默认面向 1-9 年级。",
  },
];
import { Transcript } from "./components/Transcript";
import { SessionArtifacts } from "./components/SessionArtifacts";
import { Composer } from "./components/Composer";
import { TodoPanel } from "./components/TodoPanel";
import { ApprovalModal } from "./components/ApprovalModal";
import { AskCard } from "./components/AskCard";
import { StatusBar } from "./components/StatusBar";
import { MemoryPanel } from "./components/MemoryPanel";
import { HistoryPanel } from "./components/HistoryPanel";
import { SettingsPanel } from "./components/SettingsPanel";
import { HelpDrawer } from "./components/HelpDrawer";
import { CapabilitiesPanel } from "./components/CapabilitiesPanel";
import { HardwarePanel } from "./components/HardwarePanel";
import { KnowledgePanel } from "./components/KnowledgePanel";
import { LoginGate } from "./components/LoginGate";
import { UpdateBanner } from "./components/UpdateBanner";
import { WorkspacePanel, type WorkspaceOpenRequest } from "./components/WorkspacePanel";
import { FolderPicker } from "./components/FolderPicker";
import { useSession, setSessionStore } from "./lib/account";
import { ConfirmHost, confirmDialog } from "./lib/confirm";
import { openFolderPicker } from "./lib/folderPicker";
import { app, onEvent } from "./lib/bridge";
import { parseTodos } from "./lib/tools";
import { useDetailMode, toggleDetailMode } from "./lib/detailMode";
import { sessionActivityTime } from "./lib/session";
import type { MemoryView, Mode, SessionMeta, TabMeta } from "./lib/types";
import { loadLayoutSize, saveLayoutSize } from "./lib/layoutPreferences";
import { applyTheme, getTheme, getThemeStyle, isThemeStyle, themeForStyle, type Theme } from "./lib/theme";

const SIDEBAR_COLLAPSED_KEY = "reasonix.sidebar.collapsed";
const KNOWLEDGE_SELECTED_KEY = "onecreat.knowledge.selectedBases";
const SIDEBAR_COLLAPSED_WIDTH = 68;
const SIDEBAR_DEFAULT_WIDTH = 264;
const SIDEBAR_MIN_WIDTH = 228;
const SIDEBAR_MAX_WIDTH = 420;
const CHAT_MIN_WIDTH = 420;

function isThemeMode(value: string): value is Theme {
  return value === "auto" || value === "light" || value === "dark";
}
const WORKSPACE_PANEL_MIN_WIDTH = 640;
const WORKSPACE_PANEL_DEFAULT_WIDTH = WORKSPACE_PANEL_MIN_WIDTH;
const WORKSPACE_PANEL_MAX_WIDTH = 820;
const WORKSPACE_PANEL_MAX_RATIO = 0.54;
const WORKSPACE_FILE_TREE_PANEL_DEFAULT_WIDTH = 360;
const WORKSPACE_FILE_TREE_PANEL_MIN_WIDTH = 320;
const WORKSPACE_FILE_TREE_PANEL_MAX_WIDTH = 480;
const WORKSPACE_FILE_TREE_PANEL_MAX_RATIO = 0.32;

function clampSidebarWidth(width: number): number {
  return Math.min(SIDEBAR_MAX_WIDTH, Math.max(SIDEBAR_MIN_WIDTH, Math.round(width)));
}

function clampWorkspacePanelWidth(width: number, sidebarWidth = SIDEBAR_DEFAULT_WIDTH, viewportWidth = 1440): number {
  const maxByRatio = Math.floor(viewportWidth * WORKSPACE_PANEL_MAX_RATIO);
  const maxByChat = Math.floor(viewportWidth - sidebarWidth - CHAT_MIN_WIDTH);
  const max = Math.max(WORKSPACE_PANEL_MIN_WIDTH, Math.min(WORKSPACE_PANEL_MAX_WIDTH, maxByRatio, maxByChat));
  return Math.min(max, Math.max(WORKSPACE_PANEL_MIN_WIDTH, Math.round(width)));
}

function clampWorkspaceFileTreePanelWidth(width: number, sidebarWidth = SIDEBAR_DEFAULT_WIDTH, viewportWidth = 1440): number {
  const maxByRatio = Math.floor(viewportWidth * WORKSPACE_FILE_TREE_PANEL_MAX_RATIO);
  const maxByChat = Math.floor(viewportWidth - sidebarWidth - CHAT_MIN_WIDTH);
  const max = Math.max(
    WORKSPACE_FILE_TREE_PANEL_MIN_WIDTH,
    Math.min(WORKSPACE_FILE_TREE_PANEL_MAX_WIDTH, maxByRatio, maxByChat),
  );
  return Math.min(max, Math.max(WORKSPACE_FILE_TREE_PANEL_MIN_WIDTH, Math.round(width)));
}

function loadSidebarCollapsed(): boolean {
  if (typeof window === "undefined") return false;
  try {
    return window.localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1";
  } catch {
    return false;
  }
}

function saveSidebarCollapsed(collapsed: boolean): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? "1" : "0");
  } catch {
    /* ignore storage failures */
  }
}

function loadKnowledgeSelectedBaseIds(): string[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = window.localStorage.getItem(KNOWLEDGE_SELECTED_KEY);
    if (!raw) return [];
    const ids = JSON.parse(raw);
    return Array.isArray(ids) ? ids.filter((id): id is string => typeof id === "string" && id.length > 0) : [];
  } catch {
    return [];
  }
}

function saveKnowledgeSelectedBaseIds(ids: string[]): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(KNOWLEDGE_SELECTED_KEY, JSON.stringify(ids));
  } catch {
    /* ignore storage failures */
  }
}

// 知识库总开关:默认开(自动检索全部);用户可在输入框的知识库下拉里关掉。
const KNOWLEDGE_ENABLED_KEY = "onecreat.knowledge.enabled";
function loadKnowledgeEnabled(): boolean {
  try {
    return window.localStorage.getItem(KNOWLEDGE_ENABLED_KEY) !== "0";
  } catch {
    return true;
  }
}
function saveKnowledgeEnabled(on: boolean): void {
  try {
    window.localStorage.setItem(KNOWLEDGE_ENABLED_KEY, on ? "1" : "0");
  } catch {
    /* ignore */
  }
}

function shouldBypassAutoKnowledge(rawText: string): boolean {
  const compact = rawText
    .trim()
    .replace(/\s+/g, "")
    .replace(/[。！？!?，,；;：:.、]/g, "");

  if (!compact || compact.length > 18) return false;

  const exactFollowUps = new Set([
    "上传",
    "直接上传",
    "帮我上传",
    "你帮我上传",
    "你直接帮我上传",
    "烧录",
    "直接烧录",
    "帮我烧录",
    "运行",
    "直接运行",
    "编译",
    "验证",
    "继续",
    "可以",
    "好的",
    "重试",
    "再试一次",
  ]);
  if (exactFollowUps.has(compact)) return true;

  if (/^(你)?(直接)?(帮我)?(上传|烧录|运行|编译|验证|调试|继续|重试)(一下|下|吧)?$/.test(compact)) {
    return true;
  }

  return /(上传|烧录).*(板子|开发板|设备)/.test(compact);
}

function loadSidebarWidth(): number {
  return loadLayoutSize("sidebarWidth", SIDEBAR_DEFAULT_WIDTH, clampSidebarWidth);
}

function saveSidebarWidth(width: number): void {
  saveLayoutSize("sidebarWidth", width, clampSidebarWidth);
}

function loadWorkspacePanelWidth(): number {
  return loadLayoutSize("workspacePanelWidth", WORKSPACE_PANEL_DEFAULT_WIDTH, clampWorkspacePanelWidth);
}

function saveWorkspacePanelWidth(width: number): void {
  saveLayoutSize("workspacePanelWidth", width);
}

function loadWorkspaceFileTreePanelWidth(): number {
  return loadLayoutSize(
    "workspaceFileTreePanelWidth",
    WORKSPACE_FILE_TREE_PANEL_DEFAULT_WIDTH,
    clampWorkspaceFileTreePanelWidth,
  );
}

function saveWorkspaceFileTreePanelWidth(width: number): void {
  saveLayoutSize("workspaceFileTreePanelWidth", width);
}

// 把 preview 里的模板噪音(Plan mode 块、planner 提案、工具调用 tag)清掉,
// 只留学生真正问的那句话,免得侧栏全是 "Plan mode — read-only. Explore..."。
function cleanSessionPreview(raw: string): string {
  let text = raw;
  // 删除成对的 [Plan mode ...] / [... read-only ...] / [... Explore ...] 块(保留其后的真实问题)
  text = text.replace(/\[(?:Plan mode|[^\]]*?read-only|[^\]]*?Explore the codebase)[^\]]*\]/gi, "");
  // 删除「未闭合 / 被截断」的开头方括号块(如侧栏里看到的 "[Plan mode — read-only. Ex…")
  text = text.replace(/\[\s*(?:Plan mode|[^\]]*read-only)[\s\S]*$/i, "");
  // 删除 planner / agent 模板前缀
  text = text.replace(/A planner proposed this approach:?/gi, "");
  text = text.replace(/(?:Task|TASK):\s*/g, "");
  // 删除 <tool_name> / <path>...</path> 这类 XML 风格标签和它们的内容
  text = text.replace(/<[^>]+>[\s\S]*?<\/[^>]+>/g, "");
  text = text.replace(/<[^>]+>/g, "");
  // 折叠空白
  text = text.replace(/\s+/g, " ").trim();
  return text;
}

function sessionTitle(session: SessionMeta, fallback: string): string {
  if (session.title?.trim()) return session.title;
  const cleaned = cleanSessionPreview(session.preview || "");
  if (cleaned) return cleaned;
  return fallback;
}

// relativeTime 给会话一个紧凑的相对时间("刚刚 / 3 天 / 2 周 / 1 个月"),
// 像参考侧栏右侧那样,比绝对日期更易扫读。
function relativeTime(ms: number): string {
  if (!ms) return "";
  const sec = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (sec < 60) return "刚刚";
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} 分钟`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr} 小时`;
  const day = Math.floor(hr / 24);
  if (day < 7) return `${day} 天`;
  if (day < 30) return `${Math.floor(day / 7)} 周`;
  if (day < 365) return `${Math.floor(day / 30)} 个月`;
  return `${Math.floor(day / 365)} 年`;
}

// 取路径最后一段作为文件夹显示名;空 cwd 归到「未关联文件夹」组。
function cwdFolderLabel(cwd: string): string {
  if (!cwd) return i18nT("sidebar.unlinkedFolder");
  const parts = cwd.replace(/[\\/]+$/, "").split(/[\\/]/);
  return parts[parts.length - 1] || cwd;
}

// 把 sessions 按 cwd 分组,保留原有顺序(新→旧)。
// 返回 [{ cwd, label, sessions[] }, ...],"当前文件夹优先排,未关联归末尾"。
function groupSessionsByCwd(
  sessions: SessionMeta[],
  currentCwd: string,
): { cwd: string; label: string; sessions: SessionMeta[] }[] {
  const groups = new Map<string, SessionMeta[]>();
  const order: string[] = [];
  for (const s of sessions) {
    const key = s.cwd || "";
    if (!groups.has(key)) {
      groups.set(key, []);
      order.push(key);
    }
    groups.get(key)!.push(s);
  }
  // 当前文件夹组提到最前,空 cwd 组放最后
  order.sort((a, b) => {
    if (a === currentCwd) return -1;
    if (b === currentCwd) return 1;
    if (!a) return 1;
    if (!b) return -1;
    return 0;
  });
  return order.map((cwd) => ({ cwd, label: cwdFolderLabel(cwd), sessions: groups.get(cwd)! }));
}

// 文件夹备注名(如学生名)与置顶,先用 localStorage 持久化(键为文件夹绝对路径)。
const FOLDER_ALIASES_KEY = "onecreat.folder.aliases";
const FOLDER_PINNED_KEY = "onecreat.folder.pinned";
function loadFolderAliases(): Record<string, string> {
  try {
    const v = JSON.parse(window.localStorage.getItem(FOLDER_ALIASES_KEY) || "{}");
    return v && typeof v === "object" ? (v as Record<string, string>) : {};
  } catch {
    return {};
  }
}
function saveFolderAliases(m: Record<string, string>): void {
  try {
    window.localStorage.setItem(FOLDER_ALIASES_KEY, JSON.stringify(m));
  } catch {
    /* ignore */
  }
}
function loadFolderPinned(): string[] {
  try {
    const v = JSON.parse(window.localStorage.getItem(FOLDER_PINNED_KEY) || "[]");
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}
function saveFolderPinned(a: string[]): void {
  try {
    window.localStorage.setItem(FOLDER_PINNED_KEY, JSON.stringify(a));
  } catch {
    /* ignore */
  }
}

// 对话(会话)置顶:按 session 路径记一组,置顶的对话单独排到列表顶部「置顶」组。
const SESSION_PINNED_KEY = "onecreat.session.pinned";
function loadSessionPinned(): string[] {
  try {
    const v = JSON.parse(window.localStorage.getItem(SESSION_PINNED_KEY) || "[]");
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}
function saveSessionPinned(a: string[]): void {
  try {
    window.localStorage.setItem(SESSION_PINNED_KEY, JSON.stringify(a));
  } catch {
    /* ignore */
  }
}

export default function App() {
  // 多标签多任务:每个标签是一个独立任务(后端各有 controller + session,后台标签也并行
  // 跑)。useController 绑定当前活动标签;切换标签时它会重订阅该标签的事件并重载会话。
  const [tabs, setTabs] = useState<TabMeta[]>(() => [
    { id: "main", kind: "chat", label: "", ready: false, active: true },
  ]);
  const [activeTabId, setActiveTabId] = useState("main");
  const {
    state,
    send,
    notice,
    cancel,
    approve,
    answerQuestion,
    setPlan,
    setCoach,
    setBypass,
    listSessions,
    resumeSession,
    previewSession,
    deleteSession,
    renameSession,
    refreshMeta,
    switchWorkspace,
    rewind,
	setModel,
	setEffort,
    fetchMemory,
    remember,
    forget,
    saveDoc,
  } = useController(activeTabId);
  const t = useT();
  const detailMode = useDetailMode(); // 简洁(默认) vs 详细 显示开关
  const [mode, setMode] = useState<Mode>("normal");
  // 协作模式 persona key(""=默认)。setCoach 作用于「活动 tab」的 controller,
  // 所以换模式或切 tab 时都重新注入,让新 tab 也续上当前 persona。
  const [coachKey, setCoachKey] = useState<string>("");
  useEffect(() => {
    setCoach(COACH_MODES.find((m) => m.key === coachKey)?.preamble ?? "");
  }, [coachKey, activeTabId, setCoach]);
  // A8:切回标签后把底部 pill 同步到「该标签 controller 的真实门控」(Meta 的
  // planMode/bypass),而不是凭全局 UI 态猜测——否则会出现「pill 显示只读、实际可写」的
  // 安全错觉。只在切标签后第一次 meta 到达时同步,不覆盖用户随后的手动 Shift+Tab 切换。
  const pendingModeSyncRef = useRef(false);
  useEffect(() => {
    pendingModeSyncRef.current = true;
  }, [activeTabId]);
  useEffect(() => {
    if (!pendingModeSyncRef.current || !state.meta) return;
    pendingModeSyncRef.current = false;
    setMode(state.meta.planMode ? "plan" : state.meta.bypass ? "yolo" : "normal");
  }, [state.meta]);
  // A2:每个「非活动」标签挂一个轻量订阅,只为在标签栏上提示「该标签有待审批」。活动标签
  // 的弹窗由 useController 处理(并在切回时由 PendingPrompts 补显),这里跳过它避免重复。
  const [pendingTabs, setPendingTabs] = useState<Record<string, boolean>>({});
  // 已登录态镜像到 ref:后台 tab 的事件订阅(下面)据此决定是否刷新余额,但不能把 session
  // 放进该订阅 effect 的依赖里(否则每次刷新点数都会重订阅所有标签)。在下面 session 那个
  // effect 里更新它。
  const loggedInRef = useRef(false);
  useEffect(() => {
    const offs = tabs
      .filter((tab) => tab.id !== activeTabId)
      .map((tab) =>
        onEvent(tab.id, (e) => {
          if (e.kind === "approval_request" || e.kind === "ask_request") {
            setPendingTabs((p) => (p[tab.id] ? p : { ...p, [tab.id]: true }));
          } else if (e.kind === "turn_done") {
            setPendingTabs((p) => (p[tab.id] ? { ...p, [tab.id]: false } : p));
            // L1:后台 tab 烧点也要刷新余额,否则只在活动 tab 下次结束时才更新、余额显示偏高。
            if (loggedInRef.current) void app.RefreshAccountSession().then(setSessionStore).catch(() => {});
          }
        }),
      );
    return () => offs.forEach((off) => off());
  }, [tabs, activeTabId]);
  // 切到某标签即清掉它的待审批角标(用户已在看它,弹窗会补显)。
  useEffect(() => {
    setPendingTabs((p) => (p[activeTabId] ? { ...p, [activeTabId]: false } : p));
  }, [activeTabId]);
  const [memView, setMemView] = useState<MemoryView | null>(null);
  const [histView, setHistView] = useState<SessionMeta[] | null>(null);
  const [sidebarSessions, setSidebarSessions] = useState<SessionMeta[]>([]);
  // 会话列表按文件夹折叠:folderOverrides 记录用户手动展开/折叠过的文件夹;默认只有
  // 当前文件夹展开,其余折叠(像参考侧栏那样,以文件夹归纳、点开才看会话)。
  const [folderOverrides, setFolderOverrides] = useState<Record<string, boolean>>({});
  // 文件夹备注名(学生名)/置顶/「⋯」菜单当前展开的文件夹。
  const [folderAliases, setFolderAliases] = useState<Record<string, string>>(loadFolderAliases);
  const [folderPinned, setFolderPinned] = useState<string[]>(loadFolderPinned);
  const [sessionPinned, setSessionPinned] = useState<string[]>(loadSessionPinned);
  // 「⋯」菜单:存当前文件夹组 + 屏幕坐标,弹层渲染到 app 根层(和遮罩同一层叠上下文,
  // 否则被侧栏的层叠上下文困住、点击被透明遮罩拦截)。fixed 定位避开滚动容器裁剪。
  const [folderMenu, setFolderMenu] = useState<{
    group: { cwd: string; label: string; sessions: SessionMeta[] };
    pos: { top: number; left: number };
  } | null>(null);
  // 备注名行内编辑:用 HTML input(webview 里原生 window.prompt 常被屏蔽,弹不出来)。
  const [editingFolder, setEditingFolder] = useState<string | null>(null);
  const [editingNote, setEditingNote] = useState("");
  const cancelEditRef = useRef(false);
  const [sidebarCollapsed, setSidebarCollapsed] = useState(loadSidebarCollapsed);
  const [sidebarWidth, setSidebarWidth] = useState(loadSidebarWidth);
  const [sidebarResizing, setSidebarResizing] = useState(false);
  const [workspacePanelOpen, setWorkspacePanelOpen] = useState(false);
  const [workspacePanelWidth, setWorkspacePanelWidth] = useState(loadWorkspacePanelWidth);
  const [workspaceFileTreePanelWidth, setWorkspaceFileTreePanelWidth] = useState(loadWorkspaceFileTreePanelWidth);
  const [workspaceOpenRequest, setWorkspaceOpenRequest] = useState<WorkspaceOpenRequest | null>(null);
  const [workspacePanelResizing, setWorkspacePanelResizing] = useState(false);
  const [workspacePanelMaximized, setWorkspacePanelMaximized] = useState(false);
  const [workspacePreviewModeActive, setWorkspacePreviewModeActive] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [capsOpen, setCapsOpen] = useState(false);
  // 账号会话。session=null 表示加载中;未登录就是本地 API 模式,默认全功能。
  const session = useSession();
  const refreshSession = useCallback(() => {
    app
      .AccountSessionInfo()
      .then(setSessionStore)
      .catch(() =>
        // 拉会话失败(桥接异常的降级路径):按未登录本地态处理,platformMode:false 不挡门,app 仍可用。
        setSessionStore({ loggedIn: false, account: "", isAdmin: false, permissions: [], tiers: [], points: null, selectedTier: 1, platformMode: false })
      );
  }, []);
  useEffect(() => {
    refreshSession();
  }, [refreshSession]);
  const canFeature = useCallback(
    (key: string) => !!session && (!session.loggedIn || session.isAdmin || session.permissions.includes(key)),
    [session],
  );
  // 主区域视图模式:'chat' = 普通对话(Transcript),'hardware' = 硬件 IDE 工作台。
  // 两个视图同时挂载用 display:none 切换,防止 chat 流式输出被中断。
  const [mainView, setMainView] = useState<"chat" | "hardware">("chat");
  // 硬件模式:对话区顶部的实验台 bar 是否收起。
  const [hwCollapsed, setHwCollapsed] = useState(false);
  const [knowledgeOpen, setKnowledgeOpen] = useState(false);
  const [selectedKnowledgeBaseIds, setSelectedKnowledgeBaseIds] = useState(loadKnowledgeSelectedBaseIds);
  // 知识库总开关 + 可选库列表(给输入框的内联选择器用)。默认开=自动检索全部。
  const [knowledgeEnabled, setKnowledgeEnabled] = useState(loadKnowledgeEnabled);
  const [knowledgeBases, setKnowledgeBases] = useState<{ id: string; name: string }[]>([]);
  const [pendingPlanRevision, setPendingPlanRevision] = useState<string | null>(null);
  const [viewportWidth, setViewportWidth] = useState(() => (typeof window === "undefined" ? 1440 : window.innerWidth));
  const [footerHeight, setFooterHeight] = useState(0);
  const footerRef = useRef<HTMLElement>(null);
  const sidebarBeforeWorkspacePreviewRef = useRef<boolean | null>(null);
  const effectiveSidebarWidth = sidebarCollapsed ? SIDEBAR_COLLAPSED_WIDTH : sidebarWidth;
  const effectiveWorkspacePanelWidth = useMemo(
    () =>
      workspacePreviewModeActive
        ? clampWorkspacePanelWidth(workspacePanelWidth, effectiveSidebarWidth, viewportWidth)
        : clampWorkspaceFileTreePanelWidth(workspaceFileTreePanelWidth, effectiveSidebarWidth, viewportWidth),
    [effectiveSidebarWidth, viewportWidth, workspaceFileTreePanelWidth, workspacePanelWidth, workspacePreviewModeActive],
  );

  // applyMode is the single source of truth for the input mode: it updates the
  // local pill and pushes the matching gate state to the controller (plan = read
  // only; yolo = auto-approve every tool call). normal clears both.
  const applyMode = useCallback(
    (m: Mode) => {
      setMode(m);
      setPlan(m === "plan");
      setBypass(m === "yolo");
    },
    [setPlan, setBypass],
  );
  // Shift+Tab cycles normal → plan → yolo → normal.
  const cycleMode = useCallback(() => {
    applyMode(mode === "normal" ? "plan" : mode === "plan" ? "yolo" : "normal");
  }, [mode, applyMode]);

  // Switching models rebuilds the controller, which starts in normal mode — so
  // re-apply the current mode, or the pill would say plan/YOLO while the fresh
  // controller silently uses normal gating.
  const switchModel = useCallback(
    async (name: string) => {
      await setModel(name);
      if (mode === "plan") setPlan(true);
      else if (mode === "yolo") setBypass(true);
    },
    [setModel, mode, setPlan, setBypass],
  );

  // The live task list pinned above the composer comes from the most recent
  // top-level todo_write call; it stays visible while work remains, clears itself
  // once every item is completed, and can be dismissed by the user (the ✕). A
  // dismissal is keyed to that list's id, so a fresh todo_write (a new task)
  // brings the panel back.
  const todoItem = useMemo(() => {
    for (let i = state.items.length - 1; i >= 0; i--) {
      const it = state.items[i];
      if (it.kind === "tool" && it.name === "todo_write" && !it.parentId) return it;
    }
    return null;
  }, [state.items]);
  const todos = useMemo(() => (todoItem ? parseTodos(todoItem.args) : []), [todoItem]);
  const [dismissedTodo, setDismissedTodo] = useState<string | null>(null);
  const showTodos =
    !!todoItem &&
    todoItem.id !== dismissedTodo &&
    todos.length > 0 &&
    todos.some((t) => t.status !== "completed");

  useEffect(() => {
    if (!pendingPlanRevision || state.running) return;
    const text = pendingPlanRevision;
    setPendingPlanRevision(null);
    send(text);
  }, [pendingPlanRevision, send, state.running]);

  useEffect(() => {
    saveKnowledgeSelectedBaseIds(selectedKnowledgeBaseIds);
  }, [selectedKnowledgeBaseIds]);

  useEffect(() => {
    saveKnowledgeEnabled(knowledgeEnabled);
  }, [knowledgeEnabled]);

  // 拉取可选知识库列表(给输入框内联选择器);挂载时取一次,知识库面板关闭后再刷新。
  const refreshKnowledgeBases = useCallback(async () => {
    try {
      const view = await app.KnowledgeView();
      setKnowledgeBases(view.bases.map((b) => ({ id: b.id, name: b.name })));
    } catch {
      /* 知识库不可用时静默 */
    }
  }, []);

  useEffect(() => {
    void refreshKnowledgeBases();
  }, [refreshKnowledgeBases]);

  // 拉取已加载的技能(给输入框的「技能」选择器);挂载时取一次,能力面板关闭后刷新。
  const [skills, setSkills] = useState<{ name: string; description: string }[]>([]);
  const refreshSkillList = useCallback(async () => {
    try {
      const caps = await app.Capabilities();
      setSkills((caps.skills || []).map((s) => ({ name: s.name, description: s.description })));
    } catch {
      /* 能力不可用时静默 */
    }
  }, []);

  useEffect(() => {
    void refreshSkillList();
  }, [refreshSkillList]);

  // 内联选择器的动作:勾选某库=启用并切到「只用选中」;自动=启用且清空选择(检索全部)。
  const toggleKnowledgeBase = (id: string) => {
    setKnowledgeEnabled(true);
    setSelectedKnowledgeBaseIds((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  };
  const setKnowledgeAuto = () => {
    setKnowledgeEnabled(true);
    setSelectedKnowledgeBaseIds([]);
  };

  // Memory drawer: opening fetches a fresh snapshot; writes re-fetch so the
  // panel reflects what landed on disk.
  const openMemory = useCallback(async () => {
    setMemView(await fetchMemory());
  }, [fetchMemory]);

  const closeMemory = useCallback(() => setMemView(null), []);

  // handleSend intercepts the slash commands that need a desktop-native action
  // before they reach the backend: "/model <ref>" rebuilds on that model, and
  // "/memory" / "/knowledge" open local drawers. Everything else — skills (/init, …),
  // custom commands, bare /model and the other read-only management verbs
  // (/skill, /hooks, /mcp) — goes straight to Submit, which the controller
  // resolves (a turn, or a listing Notice).
  const handleSend = useCallback(
    (displayText: string, submitText = displayText) => {
      const trimmed = displayText.trim();
      if (session?.loggedIn && (/^\/model(?:\s|$)/.test(trimmed) || /^\/effort(?:\s|$)/.test(trimmed))) {
        notice("AI 由 OneCreat 平台智能档位统一调度；当前账号只显示档位，不显示底层模型、服务商或路由。", "info");
        return;
      }
      const model = /^\/model\s+(\S+)$/.exec(trimmed);
      if (model) {
        void switchModel(model[1]);
        return;
      }
      if (trimmed === "/memory") {
        void openMemory();
        return;
      }
      if (trimmed === "/knowledge") {
        // L2:/knowledge 命令与侧栏按钮一样受功能门控,未开通的机构点不动。
        if (canFeature("knowledge")) setKnowledgeOpen(true);
        else notice("本机构未开通知识库功能", "warn");
        return;
      }
      const theme = /^\/theme(?:\s+(\S+))?$/.exec(trimmed);
      if (theme) {
        const arg = theme[1]?.toLowerCase();
        if (!arg) {
          const cur = getTheme();
          notice(t("settings.themeCurrent", { theme: cur, style: getThemeStyle(cur) }));
          return;
        }
        if (isThemeMode(arg)) {
          const next = arg;
          const style = getThemeStyle(next);
          applyTheme(next, style);
          notice(t("settings.themeChanged", { theme: next, style }));
          return;
        }
        if (isThemeStyle(arg)) {
          const next = themeForStyle(arg);
          applyTheme(next, arg);
          notice(t("settings.themeChanged", { theme: next, style: arg }));
          return;
        }
        notice(t("settings.themeUnknown", { name: arg }), "warn");
        return;
      }
      const rawSubmit = submitText.trim();
      const programmaticPrompt = submitText !== displayText;
      // 知识库默认「自动」:开关打开时,任何非斜杠消息都自动检索——没手动选库就检索全部;
      // 选了就只用选中的;检索不到相关片段则原样发送(零副作用)。关闭开关则完全不检索。
      // “上传/烧录/继续”等短跟进动作必须优先沿用当前对话上下文,否则全库检索会把意图带偏。
      if (canFeature("knowledge") && knowledgeEnabled && trimmed && !trimmed.startsWith("/") && !rawSubmit.startsWith("/")) {
        if (selectedKnowledgeBaseIds.length === 0 && (programmaticPrompt || shouldBypassAutoKnowledge(rawSubmit))) {
          send(trimmed, rawSubmit);
          return;
        }

        void (async () => {
          let baseIds = selectedKnowledgeBaseIds;
          try {
            if (baseIds.length === 0) {
              const knowledge = await app.KnowledgeView();
              baseIds = knowledge.bases.map((base) => base.id);
            }
            if (baseIds.length === 0) {
              send(trimmed, rawSubmit);
              return;
            }
            const built = await app.KnowledgeBuildPrompt(baseIds, rawSubmit, 8);
            if (built.sources.length > 0 && built.prompt.trim()) {
              send(trimmed, built.prompt.trim());
              return;
            }
          } catch (e) {
            notice(`知识库检索失败，已按原问题发送：${String((e as Error)?.message ?? e)}`, "warn");
          }
          send(trimmed, rawSubmit);
        })();
        return;
      }
      send(trimmed, rawSubmit);
    },
    [session?.loggedIn, switchModel, openMemory, send, selectedKnowledgeBaseIds, knowledgeEnabled, notice, t, canFeature],
  );

  const refreshSessions = useCallback(async () => {
    const sessions = await listSessions();
    setSidebarSessions(sessions.slice(0, 10));
    return sessions;
  }, [listSessions]);

  useEffect(() => {
    void refreshSessions();
  }, [refreshSessions]);

  // ---- 标签(多任务)管理 ----
  // 从后端拉取标签列表(顺序/就绪/标题/活动)。后端是真并行多控制器,后台标签照常跑,
  // 这里只是同步标签栏显示。
  const refreshTabs = useCallback(async () => {
    const list = await app.ListTabs().catch(() => [] as TabMeta[]);
    if (list.length) {
      setTabs(list);
      const active = list.find((tab) => tab.active);
      if (active) setActiveTabId(active.id);
    }
    return list;
  }, []);

  useEffect(() => {
    void refreshTabs();
  }, [refreshTabs]);

  // 一轮结束后刷新标签栏(拿到新标题/就绪态);跑动时不刷,避免抖动。
  useEffect(() => {
    if (!state.running) void refreshTabs();
  }, [state.running, refreshTabs]);

  // 每轮对话结束(running 由 true→false)后,已登录则向平台刷新点数,让消耗实时可见。
  const prevRunningRef = useRef(false);
  useEffect(() => {
    if (prevRunningRef.current && !state.running && session?.loggedIn) {
      void app.RefreshAccountSession().then(setSessionStore).catch(() => {});
    }
    prevRunningRef.current = state.running;
    loggedInRef.current = !!session?.loggedIn; // 供后台 tab 订阅判断是否刷新余额(L1)
  }, [state.running, session]);

  // 当前活动标签从「装配中」变为就绪时,刷新标签栏以清掉它的 loading 小圈。
  useEffect(() => {
    if (state.meta?.ready) void refreshTabs();
  }, [state.meta?.ready, refreshTabs]);

  // 切换标签:把后端「活动镜像」重指到目标标签(既有会话方法随之作用到它),前端
  // useController 重订阅该标签事件并重载会话。
  const switchTab = useCallback(
    async (id: string) => {
      if (id === activeTabId) return;
      await app.SetActiveTab(id).catch(() => {});
      setActiveTabId(id);
      // P1 后硬件工作台是「工作区级」表面(单实例、作用于当前文件夹),不属于某个
      // tab。切标签不再按 tab.kind 强切视图——否则全局 mainView 被各标签来回覆盖,
      // 用户正看着硬件面板切个对话标签就被弹走。视图只由用户显式切换。
      void refreshTabs();
    },
    [activeTabId, refreshTabs],
  );

  // 新建任务标签:后端新起一个独立 controller(异步装配,期间 ready=false 显示 loading),
  // 设为活动。kind 决定初始显示对话还是硬件视图。
  const createTab = useCallback(
    async (kind: "chat" | "hardware") => {
      const meta = await app.CreateTab(kind).catch(() => null);
      if (!meta) return;
      // 新标签继承当前门控 + persona:即使 controller 还在异步装配,后端也会记录 want
      // 状态并在装配完成时施加,保证 pill 不撒谎、plan 只读门确实生效(A8)。
      if (mode === "plan") app.SetPlanMode(meta.id, true).catch(() => {});
      else if (mode === "yolo") app.SetBypass(meta.id, true).catch(() => {});
      const preamble = COACH_MODES.find((m) => m.key === coachKey)?.preamble ?? "";
      if (preamble) app.SetCoachMode(meta.id, preamble).catch(() => {});
      setActiveTabId(meta.id);
      setMainView(kind === "hardware" ? "hardware" : "chat");
      await refreshTabs();
    },
    [refreshTabs, mode, coachKey],
  );

  // 关闭任务标签:后端快照并关掉它的 controller;若关的是当前活动标签,切到剩下的。
  const closeTab = useCallback(
    async (id: string) => {
      await app.CloseTab(id).catch(() => {});
      const list = await refreshTabs();
      if (id === activeTabId) {
        const next = list.find((tab) => tab.active) ?? list[list.length - 1];
        // 同 switchTab:关标签只换会话,不按 kind 强切视图。
        if (next) setActiveTabId(next.id);
      }
    },
    [activeTabId, refreshTabs],
  );

  useEffect(() => {
    const onResize = () => setViewportWidth(window.innerWidth);
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  useEffect(() => {
    const el = footerRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const update = () => setFooterHeight(Math.round(el.getBoundingClientRect().height));
    update();
    const observer = new ResizeObserver(update);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!state.running && state.items.length > 0) void refreshSessions();
  }, [state.running, state.items.length, refreshSessions]);

  // 「新任务」= 新开一个独立标签(像 Codex / Claude Code:每个窗口是一个新任务并行进行),
  // 不再原地轮换当前会话、也不会打断正在跑的标签。
  const startNewSession = useCallback(async () => {
    await createTab("chat");
  }, [createTab]);

  const toggleSidebar = useCallback(() => {
    sidebarBeforeWorkspacePreviewRef.current = null;
    setSidebarCollapsed((collapsed) => {
      const next = !collapsed;
      saveSidebarCollapsed(next);
      return next;
    });
  }, []);

  const handleWorkspacePreviewModeChange = useCallback((active: boolean) => {
    setWorkspacePreviewModeActive(active);
    if (active) {
      if (sidebarBeforeWorkspacePreviewRef.current === null) {
        sidebarBeforeWorkspacePreviewRef.current = sidebarCollapsed;
      }
      if (!sidebarCollapsed) setSidebarCollapsed(true);
      return;
    }
    const restoreCollapsed = sidebarBeforeWorkspacePreviewRef.current;
    sidebarBeforeWorkspacePreviewRef.current = null;
    if (restoreCollapsed !== null && restoreCollapsed !== sidebarCollapsed) {
      setSidebarCollapsed(restoreCollapsed);
    }
  }, [sidebarCollapsed]);

  const setExpandedSidebarWidth = useCallback((width: number) => {
    const next = clampSidebarWidth(width);
    setSidebarWidth(next);
    saveSidebarWidth(next);
  }, []);

  const startSidebarResize = useCallback(
    (event: ReactPointerEvent<HTMLButtonElement>) => {
      if (sidebarCollapsed) return;
      event.preventDefault();
      setSidebarResizing(true);
      let nextWidth = sidebarWidth;
      const onMove = (moveEvent: PointerEvent) => {
        nextWidth = clampSidebarWidth(moveEvent.clientX);
        setSidebarWidth(nextWidth);
      };
      const onDone = () => {
        setSidebarWidth(nextWidth);
        saveSidebarWidth(nextWidth);
        setSidebarResizing(false);
        window.removeEventListener("pointermove", onMove);
        window.removeEventListener("pointerup", onDone);
        window.removeEventListener("pointercancel", onDone);
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
      };
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      window.addEventListener("pointermove", onMove);
      window.addEventListener("pointerup", onDone);
      window.addEventListener("pointercancel", onDone);
    },
    [sidebarCollapsed, sidebarWidth],
  );

  const resizeSidebarWithKeyboard = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>) => {
      if (sidebarCollapsed) return;
      if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
        event.preventDefault();
        setExpandedSidebarWidth(sidebarWidth + (event.key === "ArrowRight" ? 16 : -16));
      } else if (event.key === "Home") {
        event.preventDefault();
        setExpandedSidebarWidth(SIDEBAR_MIN_WIDTH);
      } else if (event.key === "End") {
        event.preventDefault();
        setExpandedSidebarWidth(SIDEBAR_MAX_WIDTH);
      }
    },
    [setExpandedSidebarWidth, sidebarCollapsed, sidebarWidth],
  );

  const setSavedWorkspacePanelWidth = useCallback(
    (width: number) => {
      if (workspacePreviewModeActive) {
        const next = clampWorkspacePanelWidth(width, effectiveSidebarWidth, viewportWidth);
        setWorkspacePanelWidth(next);
        saveWorkspacePanelWidth(next);
      } else {
        const next = clampWorkspaceFileTreePanelWidth(width, effectiveSidebarWidth, viewportWidth);
        setWorkspaceFileTreePanelWidth(next);
        saveWorkspaceFileTreePanelWidth(next);
      }
    },
    [effectiveSidebarWidth, viewportWidth, workspacePreviewModeActive],
  );

  const startWorkspacePanelResize = useCallback(
    (event: ReactPointerEvent<HTMLButtonElement>) => {
      if (!workspacePanelOpen || workspacePanelMaximized) return;
      event.preventDefault();
      setWorkspacePanelResizing(true);
      let nextWidth = effectiveWorkspacePanelWidth;
      const clampWidth = workspacePreviewModeActive ? clampWorkspacePanelWidth : clampWorkspaceFileTreePanelWidth;
      const onMove = (moveEvent: PointerEvent) => {
        nextWidth = clampWidth(window.innerWidth - moveEvent.clientX, effectiveSidebarWidth, window.innerWidth);
        if (workspacePreviewModeActive) {
          setWorkspacePanelWidth(nextWidth);
        } else {
          setWorkspaceFileTreePanelWidth(nextWidth);
        }
      };
      const onDone = () => {
        if (workspacePreviewModeActive) {
          setWorkspacePanelWidth(nextWidth);
          saveWorkspacePanelWidth(nextWidth);
        } else {
          setWorkspaceFileTreePanelWidth(nextWidth);
          saveWorkspaceFileTreePanelWidth(nextWidth);
        }
        setWorkspacePanelResizing(false);
        window.removeEventListener("pointermove", onMove);
        window.removeEventListener("pointerup", onDone);
        window.removeEventListener("pointercancel", onDone);
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
      };
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      window.addEventListener("pointermove", onMove);
      window.addEventListener("pointerup", onDone);
      window.addEventListener("pointercancel", onDone);
    },
    [effectiveSidebarWidth, effectiveWorkspacePanelWidth, workspacePanelMaximized, workspacePanelOpen, workspacePreviewModeActive],
  );

  const resizeWorkspacePanelWithKeyboard = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>) => {
      if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
        event.preventDefault();
        setSavedWorkspacePanelWidth(effectiveWorkspacePanelWidth + (event.key === "ArrowLeft" ? 16 : -16));
      } else if (event.key === "Home") {
        event.preventDefault();
        setSavedWorkspacePanelWidth(workspacePreviewModeActive ? WORKSPACE_PANEL_MIN_WIDTH : WORKSPACE_FILE_TREE_PANEL_MIN_WIDTH);
      } else if (event.key === "End") {
        event.preventDefault();
        setSavedWorkspacePanelWidth(workspacePreviewModeActive ? WORKSPACE_PANEL_MAX_WIDTH : WORKSPACE_FILE_TREE_PANEL_MAX_WIDTH);
      }
    },
    [effectiveWorkspacePanelWidth, setSavedWorkspacePanelWidth, workspacePreviewModeActive],
  );

  const layoutStyle = useMemo(
    () =>
      ({
        "--sidebar-expanded-width": `${sidebarWidth}px`,
        "--workspace-width": `${effectiveWorkspacePanelWidth}px`,
      }) as CSSProperties,
    [effectiveWorkspacePanelWidth, sidebarWidth],
  );

  const setWorkspacePanel = useCallback((open: boolean) => {
    setWorkspacePanelOpen(open);
    if (!open) {
      setWorkspacePanelMaximized(false);
      setWorkspacePreviewModeActive(false);
    }
  }, []);

  // 进入硬件模式时默认展开右侧代码/文件区(像 IDE);只在切入那一刻触发,
  // 用户之后可手动隐藏,不会被强制重开。
  useEffect(() => {
    if (mainView === "hardware") setWorkspacePanel(true);
  }, [mainView, setWorkspacePanel]);

  const toggleWorkspacePanel = useCallback(() => {
    setWorkspacePanelOpen((open) => {
      const next = !open;
      return next;
    });
  }, []);

  const openWorkspaceFile = useCallback((path: string) => {
    const clean = path.trim().replace(/^\.\//, "").replace(/\/$/, "");
    if (!clean) return;
    setWorkspacePanelOpen(true);
    setWorkspaceOpenRequest({ path: clean, nonce: Date.now() });
  }, []);

  // History drawer: opening fetches the saved-session list. Idle row clicks resume;
  // running row clicks only preview through PreviewSession.
  const openHistory = useCallback(async () => {
    setHistView(await refreshSessions());
  }, [refreshSessions]);
  const closeHistory = useCallback(() => setHistView(null), []);
  const onResumeSession = useCallback(
    async (path: string) => {
      if (state.running) return;
      setHistView(null);
      await resumeSession(path);
      await refreshSessions();
    },
    [state.running, resumeSession, refreshSessions],
  );
  // Delete / rename act on disk, then re-fetch so the panel reflects the change.
  const onDeleteSession = useCallback(
    async (path: string) => {
      if (state.running) return;
      await deleteSession(path);
      setHistView(await refreshSessions());
    },
    [state.running, deleteSession, refreshSessions],
  );
  const onRenameSession = useCallback(
    async (path: string, title: string) => {
      if (state.running) return;
      await renameSession(path, title);
      setHistView(await refreshSessions());
    },
    [state.running, renameSession, refreshSessions],
  );

  // Workspace: open the folder chooser and switch projects. The hook resets the
  // transcript and refreshes meta on a pick; refresh the sidebar sessions too so
  // the recent list belongs to the newly selected workspace. A cancel is a no-op.
  const switchFolder = useCallback(async (path?: string) => {
    // 拿到目标路径(无 path = 弹内置文件夹选择器,绕开 macOS 原生对话框跑窗口后面的 bug)。
    let target = path;
    if (target === undefined) {
      target = await openFolderPicker();
      if (!target) return ""; // 取消
    }
    // 工作目录(cwd)是全进程共享的:开着多个任务标签时切文件夹会让后台标签读写到错目录,
    // 后端会直接拒绝。这里弹确认 → 自动关掉其它标签(只留当前活动标签)→ 再切。
    const others = tabs.filter((tb) => tb.id !== activeTabId);
    if (others.length > 0) {
      const ok = await confirmDialog({
        title: "切换项目文件夹",
        message: `工作目录是全局的,切换前需要先关闭其它 ${others.length} 个任务标签(否则后台任务会读写到错误的目录)。\n\n关闭它们并切换?`,
        confirmText: "关闭并切换",
        danger: true,
      });
      if (!ok) return "";
      for (const tb of others) await app.CloseTab(tb.id).catch(() => {});
      await refreshTabs();
    }
    const picked = await switchWorkspace(target);
    if (picked) await refreshSessions();
    return picked;
  }, [tabs, activeTabId, switchWorkspace, refreshSessions, refreshTabs]);

  const onRemember = useCallback(
    async (scope: string, note: string) => {
      await remember(scope, note);
      setMemView(await fetchMemory());
    },
    [remember, fetchMemory],
  );

  const onForget = useCallback(
    async (name: string) => {
      await forget(name);
      setMemView(await fetchMemory());
    },
    [forget, fetchMemory],
  );

  const onSaveDoc = useCallback(
    async (path: string, body: string) => {
      await saveDoc(path, body);
      setMemView(await fetchMemory());
    },
    [saveDoc, fetchMemory],
  );

  // 文件夹默认展开规则:当前 workspace 的文件夹展开,其余折叠;folderOverrides 是用户的手动覆盖。
  const currentCwd = state.meta?.cwd ?? "";
  const isFolderOpen = (cwd: string) => folderOverrides[cwd] ?? (cwd === currentCwd);
  const toggleFolder = (cwd: string) =>
    setFolderOverrides((prev) => ({ ...prev, [cwd]: !(prev[cwd] ?? (cwd === currentCwd)) }));

  // 文件夹悬停出现的「+」:在该项目下新建一个对话(像 Codex)。同项目直接开新任务标签;
  // 不同项目先切到该 workspace 再开(受进程 cwd 全局所限,这是 v1 行为)。
  const newTaskInFolder = async (cwd: string) => {
    if (cwd && cwd !== currentCwd) await switchFolder(cwd);
    await createTab("chat");
  };

  // 文件夹「⋯」菜单:备注名(学生名)、置顶、在 Finder 打开、删除该项目会话。
  const folderDisplayName = (cwd: string, fallback: string) => folderAliases[cwd]?.trim() || fallback;
  const isPinned = (cwd: string) => folderPinned.includes(cwd);
  const togglePin = (cwd: string) => {
    setFolderMenu(null);
    setFolderPinned((prev) => {
      const next = prev.includes(cwd) ? prev.filter((x) => x !== cwd) : [...prev, cwd];
      saveFolderPinned(next);
      return next;
    });
  };
  // 对话置顶
  const isSessionPinned = (path: string) => sessionPinned.includes(path);
  const toggleSessionPin = (path: string) => {
    setSessionPinned((prev) => {
      const next = prev.includes(path) ? prev.filter((x) => x !== path) : [...prev, path];
      saveSessionPinned(next);
      return next;
    });
  };
  const pinnedSessions = sidebarSessions.filter((s) => isSessionPinned(s.path));
  // 渲染一条会话行(置顶组和文件夹内复用):标题+时间 + 置顶/取消置顶 + 删除。
  const renderSessionRow = (session: SessionMeta) => {
    const sPinned = isSessionPinned(session.path);
    return (
      <div className={`sidebar-session${session.current ? " sidebar-session--current" : ""}`} key={session.path}>
        <button
          className="sidebar-session__main"
          onClick={() => void onResumeSession(session.path)}
          disabled={state.running || session.current}
          title={session.path}
        >
          <span className="sidebar-session__title">
            {session.kind === "hardware" && (
              <Cpu size={11} className="sidebar-session__kind" aria-label="硬件项目" />
            )}
            {sessionTitle(session, t("history.emptySession"))}
          </span>
          <span className="sidebar-session__meta">
            {session.current ? t("history.current") : relativeTime(sessionActivityTime(session))}
          </span>
        </button>
        <button
          type="button"
          className={`sidebar-session__pin${sPinned ? " sidebar-session__pin--on" : ""}`}
          onClick={(e) => {
            e.stopPropagation();
            toggleSessionPin(session.path);
          }}
          title={sPinned ? t("sidebar.unpin") : t("sidebar.pinSession")}
          aria-label={sPinned ? t("sidebar.unpin") : t("sidebar.pinSession")}
        >
          <Pin size={12} className="sidebar-session__pin-on" />
          <PinOff size={12} className="sidebar-session__pin-off" />
        </button>
        <button
          className="sidebar-session__delete"
          onClick={(e) => {
            e.stopPropagation();
            void confirmDialog({
              message: t("sidebar.deleteSessionConfirm", { title: sessionTitle(session, t("history.emptySession")) }),
              confirmText: t("common.delete"),
              danger: true,
            }).then((ok) => {
              if (ok) void onDeleteSession(session.path);
            });
          }}
          disabled={state.running || session.current}
          title="删除"
        >
          <Trash2 size={12} />
        </button>
      </div>
    );
  };
  // 点「备注名」→ 进入行内编辑(显示一个输入框,预填已有备注)。
  const startEditFolder = (cwd: string) => {
    setFolderMenu(null);
    setEditingNote(folderAliases[cwd] || "");
    cancelEditRef.current = false;
    setEditingFolder(cwd);
  };
  const saveFolderNote = (cwd: string) => {
    if (cancelEditRef.current) {
      cancelEditRef.current = false;
      setEditingFolder(null);
      return;
    }
    const note = editingNote.trim();
    setFolderAliases((prev) => {
      const next = { ...prev };
      if (note) next[cwd] = note;
      else delete next[cwd];
      saveFolderAliases(next);
      return next;
    });
    setEditingFolder(null);
  };
  const openFolderInFinder = (cwd: string) => {
    setFolderMenu(null);
    if (cwd) void app.OpenFolder(cwd).catch(() => {});
  };
  const deleteFolderSessions = async (group: { cwd: string; label: string; sessions: SessionMeta[] }) => {
    setFolderMenu(null);
    const removable = group.sessions.filter((s) => !s.current);
    if (removable.length === 0) {
      notice(t("sidebar.noRemovable"), "warn");
      return;
    }
    const ok = await confirmDialog({
      message: t("sidebar.deleteFolderConfirm", { name: folderDisplayName(group.cwd, group.label), count: removable.length }),
      confirmText: t("common.delete"),
      danger: true,
    });
    if (!ok) return;
    for (const s of removable) await deleteSession(s.path).catch(() => {});
    await refreshSessions();
  };

  const sidebarExpandBlocked = sidebarCollapsed && workspacePreviewModeActive;
  const sidebarToggleTitle = sidebarExpandBlocked
    ? t("sidebar.expandBlocked")
    : sidebarCollapsed
      ? t("sidebar.expand")
      : t("sidebar.collapse");

  // 会话还没拉到先空着(加载中)。
  if (session === null) return <div className="app" />;
  // 平台模式(打包注入/联平台)且未登录 → 挡住整个 app 要求登录;本地模式 platformMode=false,
  // 不挡、默认全功能。登录成功后 refreshSession 重新拉会话,loggedIn=true 放行,再按功能权限门控。
  if (session.platformMode && !session.loggedIn) return <LoginGate onLoggedIn={refreshSession} />;

  return (
    <div className="app">
      {/* 文件夹「⋯」菜单:遮罩 + 弹层都渲染在 app 根层(同一层叠上下文,点击才落到菜单上) */}
      {folderMenu && (
        <>
          <div className="folder-menu-backdrop" onClick={() => setFolderMenu(null)} />
          <div
            className="sidebar-folder__pop"
            style={{ top: folderMenu.pos.top, left: folderMenu.pos.left }}
          >
            <button className="sidebar-folder__pop-item" onClick={() => togglePin(folderMenu.group.cwd)}>
              <Pin size={13} />
              {isPinned(folderMenu.group.cwd) ? t("sidebar.unpin") : t("sidebar.pinFolder")}
            </button>
            <button
              className="sidebar-folder__pop-item"
              onClick={() => startEditFolder(folderMenu.group.cwd)}
            >
              <Pencil size={13} />
              {t("sidebar.noteName")}
            </button>
            <button
              className="sidebar-folder__pop-item"
              disabled={!folderMenu.group.cwd}
              onClick={() => openFolderInFinder(folderMenu.group.cwd)}
            >
              <FolderOpen size={13} />
              {t("sidebar.openInFolder")}
            </button>
            <button
              className="sidebar-folder__pop-item sidebar-folder__pop-item--danger"
              onClick={() => void deleteFolderSessions(folderMenu.group)}
            >
              <Trash2 size={13} />
              {t("sidebar.deleteFolderSessions")}
            </button>
          </div>
        </>
      )}
      <div
        className={[
          "layout",
          mainView === "hardware" ? "layout--hardware" : "",
          mainView === "hardware" && hwCollapsed ? "layout--hw-rail-collapsed" : "",
          sidebarCollapsed ? "layout--sidebar-collapsed" : "",
          sidebarResizing ? "layout--resizing layout--sidebar-resizing" : "",
          workspacePanelOpen ? "layout--workspace-open" : "",
          workspacePanelResizing ? "layout--resizing layout--workspace-resizing" : "",
          workspacePanelOpen && workspacePanelMaximized ? "layout--workspace-maximized" : "",
        ]
          .filter(Boolean)
          .join(" ")}
        style={layoutStyle}
      >
        <aside className={`sidebar${sidebarCollapsed ? " sidebar--collapsed" : ""}`} aria-label={t("sidebar.navigation")}>
          <div className="sidebar__brand">
            <img src={logo} alt="" className="sidebar__logo" />
            <span>OneCreat</span>
            <button
              className={`sidebar__toggle${sidebarExpandBlocked ? " sidebar__toggle--blocked" : ""}`}
              onClick={sidebarExpandBlocked ? undefined : toggleSidebar}
              title={sidebarToggleTitle}
              aria-label={sidebarToggleTitle}
              aria-disabled={sidebarExpandBlocked}
            >
              {sidebarCollapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}
            </button>
          </div>

          <button
            className="sidebar__new"
            onClick={() => void startNewSession()}
            disabled={state.running}
            title={state.running ? t("common.busyHint") : t("topbar.newSession")}
          >
            <SquarePen size={15} />
            <span>{t("topbar.newSession")}</span>
          </button>

          {canFeature("knowledge") && (
            <button
              className={selectedKnowledgeBaseIds.length ? "sidebar__knowledge sidebar__knowledge--active" : "sidebar__knowledge"}
              onClick={() => setKnowledgeOpen(true)}
              title="知识库"
            >
              <BookOpen size={16} />
              <span>知识库</span>
              {selectedKnowledgeBaseIds.length > 0 && <small>{selectedKnowledgeBaseIds.length}</small>}
            </button>
          )}

          {canFeature("hardware") && (
            <button
              className={`sidebar__hardware${mainView === "hardware" ? " sidebar__hardware--active" : ""}`}
              onClick={() => setMainView(mainView === "hardware" ? "chat" : "hardware")}
              title={t("sidebar.hardwareTitle")}
            >
              <Cpu size={16} />
              <span>{t("sidebar.hardware")}</span>
            </button>
          )}

          <section className="sidebar__section">
            <div className="sidebar__section-head">
              <div className="sidebar__section-title">{t("sidebar.conversations")}</div>
              <button className="sidebar__view-all" onClick={() => void openHistory()} title={t("topbar.history")}>
                {t("sidebar.viewAll")}
              </button>
            </div>
            <div className="sidebar__sessions">
              {/* 进行中:只有 2 个及以上并行任务时才显示(单个对话主区域已呈现,无需重复列出)。
                  多任务时它作为切换器,后台任务照常并行跑。 */}
              {tabs.length > 1 && (
                <div className="sidebar-tasks">
                  <div className="sidebar__group-head">{t("sidebar.inProgress")}</div>
                  {tabs.map((tab, idx) => (
                    <div
                      className={`sidebar-task${tab.id === activeTabId ? " sidebar-task--active" : ""}`}
                      key={tab.id}
                    >
                      <button
                        type="button"
                        className="sidebar-task__main"
                        onClick={() => void switchTab(tab.id)}
                        title={tab.kind === "hardware" ? t("tabs.hardware") : t("tabs.chat")}
                      >
                        {tab.kind === "hardware" ? <Cpu size={14} /> : <MessageSquare size={14} />}
                        <span className="sidebar-task__label">
                          {(tab.kind === "hardware" ? t("tabs.hardware") : t("tabs.chat")) + " " + (idx + 1)}
                        </span>
                        {!tab.ready && <span className="sidebar-task__spin" aria-hidden />}
                        {/* A2:后台标签有待审批时显示小红点,提示用户切过去处理。 */}
                        {pendingTabs[tab.id] && tab.id !== activeTabId && (
                          <span className="sidebar-task__pending" title="该标签有待审批" aria-label="待审批" />
                        )}
                      </button>
                      {tabs.length > 1 && (
                        <button
                          type="button"
                          className="sidebar-task__close"
                          onClick={(e) => {
                            e.stopPropagation();
                            void closeTab(tab.id);
                          }}
                          title="关闭任务"
                          aria-label="关闭任务"
                        >
                          <X size={12} />
                        </button>
                      )}
                    </div>
                  ))}
                </div>
              )}
              {/* 置顶:被置顶的对话单独排到顶部,悬停可取消置顶(像 Codex/CC) */}
              {pinnedSessions.length > 0 && (
                <div className="sidebar-tasks">
                  <div className="sidebar__group-head">置顶</div>
                  {pinnedSessions.map(renderSessionRow)}
                </div>
              )}
              {sidebarSessions.length === 0 ? (
                <div className="sidebar__empty">{t("sidebar.noRecent")}</div>
              ) : (
                groupSessionsByCwd(sidebarSessions, state.meta?.cwd ?? "")
                  .slice()
                  .sort((a, b) => (isPinned(b.cwd) ? 1 : 0) - (isPinned(a.cwd) ? 1 : 0))
                  .map((group) => {
                  const open = isFolderOpen(group.cwd);
                  const hasCurrent = group.sessions.some((s) => s.current);
                  const pinned = isPinned(group.cwd);
                  return (
                    <div className="sidebar-folder" key={group.cwd || "__no_cwd__"}>
                      {editingFolder === group.cwd ? (
                        <div className="sidebar-folder__edit">
                          <input
                            className="sidebar-folder__edit-input"
                            value={editingNote}
                            autoFocus
                            placeholder={t("sidebar.notePlaceholder")}
                            onChange={(e) => setEditingNote(e.target.value)}
                            onKeyDown={(e) => {
                              if (e.key === "Enter") saveFolderNote(group.cwd);
                              else if (e.key === "Escape") {
                                cancelEditRef.current = true;
                                setEditingFolder(null);
                              }
                            }}
                            onBlur={() => saveFolderNote(group.cwd)}
                          />
                        </div>
                      ) : (
                      <div
                        className={`sidebar-folder__head-row${folderMenu?.group.cwd === group.cwd ? " sidebar-folder__head-row--menu" : ""}`}
                      >
                        <button
                          type="button"
                          className={`sidebar-folder__head${hasCurrent ? " sidebar-folder__head--current" : ""}`}
                          onClick={() => toggleFolder(group.cwd)}
                          title={group.cwd || group.label}
                        >
                          {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                          {open ? <FolderOpen size={13} /> : <FolderClosed size={13} />}
                          <span className="sidebar-folder__name">{group.label}</span>
                          {folderAliases[group.cwd]?.trim() && (
                            <span className="sidebar-folder__note">{folderAliases[group.cwd]}</span>
                          )}
                        </button>
                        {pinned && (
                          <button
                            type="button"
                            className="sidebar-folder__pin"
                            onClick={() => togglePin(group.cwd)}
                            title={t("sidebar.unpin")}
                            aria-label={t("sidebar.unpin")}
                          >
                            <Pin size={11} className="sidebar-folder__pin-on" />
                            <PinOff size={11} className="sidebar-folder__pin-off" />
                          </button>
                        )}
                        <span className="sidebar-folder__count">{group.sessions.length}</span>
                        <button
                          type="button"
                          className="sidebar-folder__add"
                          onClick={() => void newTaskInFolder(group.cwd)}
                          disabled={state.running}
                          title="在此项目新建对话"
                          aria-label="在此项目新建对话"
                        >
                          <Plus size={13} />
                        </button>
                        <button
                          type="button"
                          className="sidebar-folder__menu"
                          onClick={(e) => {
                            e.stopPropagation();
                            if (folderMenu?.group.cwd === group.cwd) {
                              setFolderMenu(null);
                              return;
                            }
                            const r = e.currentTarget.getBoundingClientRect();
                            setFolderMenu({ group, pos: { top: r.bottom + 4, left: Math.max(8, r.right - 176) } });
                          }}
                          title="更多"
                          aria-label="更多"
                        >
                          <MoreHorizontal size={14} />
                        </button>
                      </div>
                      )}
                      {open &&
                        group.sessions
                          .filter((session) => !isSessionPinned(session.path))
                          .map((session) => renderSessionRow(session))}
                    </div>
                  );
                })
              )}
            </div>
          </section>

          <nav className="sidebar__nav">
            <button
              className="sidebar__navitem sidebar__navitem--sessions"
              onClick={() => void openHistory()}
              title={t("topbar.history")}
            >
              <History size={15} />
              <span>{t("topbar.history")}</span>
            </button>
            <button className="sidebar__navitem" onClick={() => void openMemory()} title={t("topbar.memory")}>
              <Brain size={15} />
              <span>{t("topbar.memory")}</span>
            </button>
            <button className="sidebar__navitem" onClick={() => setCapsOpen(true)} title={t("caps.title")}>
              <Blocks size={15} />
              <span>{t("caps.title")}</span>
            </button>
            <button className="sidebar__navitem" onClick={() => setHelpOpen(true)} title="使用教程 / 帮助">
              <BookOpen size={15} />
              <span>使用教程</span>
            </button>
            <button
              className="sidebar__navitem"
              onClick={() => setSettingsOpen(true)}
              disabled={state.running}
              title={state.running ? t("common.busyHint") : t("topbar.settings")}
            >
              <SettingsIcon size={15} />
              <span>{t("topbar.settings")}</span>
            </button>
            {session.loggedIn && (
              <button
                className="sidebar__navitem"
                onClick={() => {
                  void confirmDialog({
                    title: "退出登录",
                    message: `当前账号：${session.account}${session.isAdmin ? "（超级管理员）" : ""}\n退出后切回本地 API 模式。`,
                    confirmText: "退出",
                    danger: true,
                  }).then((ok) => {
                    if (ok) void app.AccountLogout().then(refreshSession);
                  });
                }}
                title={`当前账号：${session.account}${session.isAdmin ? "（超级管理员）" : ""} — 点击退出登录`}
              >
                <LogOut size={15} />
                <span>退出（{session.account}）</span>
              </button>
            )}
          </nav>

        </aside>
        <button
          className="sidebar-resizer"
          type="button"
          role="separator"
          aria-orientation="vertical"
          aria-label={t("sidebar.resize")}
          aria-valuemin={SIDEBAR_MIN_WIDTH}
          aria-valuemax={SIDEBAR_MAX_WIDTH}
          aria-valuenow={sidebarWidth}
          onPointerDown={startSidebarResize}
          onKeyDown={resizeSidebarWithKeyboard}
          onDoubleClick={() => setExpandedSidebarWidth(SIDEBAR_DEFAULT_WIDTH)}
          title={t("sidebar.resize")}
        />

        {/* 硬件模式:实验台作为独立的第二栏(可收起)。四栏 = 导航 | 实验台 | 对话 | 代码。
            竖向整列高度,内容放得下、可滚动,不再压住对话。 */}
        {mainView === "hardware" && (
          <aside className={`hw-rail${hwCollapsed ? " hw-rail--collapsed" : ""}`} aria-label={t("sidebar.hardware")}>
            <div className="hw-rail__head">
              <Cpu size={15} className="hw-rail__icon" />
              {!hwCollapsed && <span className="hw-rail__title">{t("sidebar.hardware")}</span>}
              <button
                className="hw-rail__toggle"
                onClick={() => setHwCollapsed((v) => !v)}
                title={hwCollapsed ? "展开实验台" : "收起实验台"}
                aria-label={hwCollapsed ? "展开实验台" : "收起实验台"}
              >
                {hwCollapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}
              </button>
            </div>
            {!hwCollapsed && (
              <div className="hw-rail__body">
                <HardwarePanel
                  onPrompt={handleSend}
                  onOpenWorkspace={(path) => (path ? openWorkspaceFile(path) : setWorkspacePanel(true))}
                  onPickProjectDir={switchFolder}
                  onBackToChat={() => setMainView("chat")}
                  selectedKnowledgeCount={selectedKnowledgeBaseIds.length}
                  active={mainView === "hardware"}
                  cwd={state.meta?.cwd}
                />
              </div>
            )}
          </aside>
        )}

        <section className="chat-pane">
          <header className="topbar">
            <div className="topbar__identity" title={state.meta?.cwd || undefined}>
              <span className="topbar__title">
                {state.meta?.cwd ? cwdFolderLabel(state.meta.cwd) : "OneCreat"}
              </span>
              {/* 网关模式(已登录)只显示档位名,绝不显示真实模型 / planner 名,与底栏档位切换器一致。 */}
              <span className="topbar__model">
                {session?.loggedIn
                  ? (session.tiers?.find((tr) => tr.index === session.selectedTier)?.name ?? "智能")
                  : (state.meta?.label ?? "…")}
              </span>
            </div>
            {/* P1 翻面：顶部不再并列「对话 | 硬件编程」双 tab。对话是唯一主视图，
                设备实验台从侧栏按需打开。 */}
            <div className="topbar__spacer" />
            <button
              className="chip chip--icon topbar__workspace-toggle"
              onClick={toggleWorkspacePanel}
              title={workspacePanelOpen ? t("workspace.close") : t("workspace.open")}
            >
              {workspacePanelOpen ? <PanelRightClose size={13} /> : <PanelRightOpen size={13} />}
            </button>
            <div className="topbar__actions">
              <button
                className={detailMode ? "chip chip--on topbar__detail-toggle" : "chip topbar__detail-toggle"}
                onClick={() => toggleDetailMode()}
                title={t("detail.toggleTitle")}
              >
                <Eye size={13} />
                <span>{detailMode ? t("detail.detailed") : t("detail.simple")}</span>
              </button>
              <button className="chip chip--icon" onClick={() => void openHistory()} title={t("topbar.history")}>
                <History size={13} />
              </button>
              <button className="chip chip--icon" onClick={() => void openMemory()} title={t("topbar.memory")}>
                <Brain size={13} />
              </button>
              <button className="chip chip--icon" onClick={() => setCapsOpen(true)} title={t("caps.title")}>
                <Blocks size={13} />
              </button>
              {canFeature("knowledge") && (
                <button
                  className={selectedKnowledgeBaseIds.length ? "chip chip--icon chip--on" : "chip chip--icon"}
                  onClick={() => setKnowledgeOpen(true)}
                  title={selectedKnowledgeBaseIds.length ? `知识库 · 已用于聊天 ${selectedKnowledgeBaseIds.length}` : "知识库"}
                >
                  <BookOpen size={13} />
                </button>
              )}
              <button
                className="chip chip--icon"
                onClick={() => setSettingsOpen(true)}
                disabled={state.running}
                title={state.running ? t("common.busyHint") : t("topbar.settings")}
              >
                <SettingsIcon size={13} />
              </button>
              <button
                className="chip chip--icon"
                onClick={() => void startNewSession()}
                disabled={state.running}
                title={state.running ? t("common.busyHint") : t("topbar.newSession")}
              >
                <SquarePen size={13} />
              </button>
            </div>
          </header>

          {state.meta?.startupErr && (
            <div className="banner banner--error">{t("topbar.startupError", { msg: state.meta.startupErr })}</div>
          )}

          <UpdateBanner />

          <main className={`main main--${mainView}`}>
            {state.meta?.ready === false && !state.meta?.startupErr ? (
              <div className="loading-screen">
                <div className="loading-screen__spinner" />
                <span className="loading-screen__text">{t("common.loading")}</span>
              </div>
            ) : (
              <>
                {/* Transcript 永远挂载;硬件模式下它就是右侧聊天栏,不再 display:none。
                    实验台(HardwarePanel)已迁到左侧 hw-rail。 */}
                <div className="main__view main__view--chat">
                  <Transcript
                    items={state.items}
                    live={state.live}
                    footerHeight={footerHeight}
                    onPrompt={send}
                    onOpenHardware={() => setMainView("hardware")}
                    onRewind={rewind}
                  />
                  {/* 待办清单:任务进行时浮在对话区右上角,不再挤占左侧会话栏 */}
                  {showTodos && (
                    <div className="main-todos">
                      <TodoPanel todos={todos} onDismiss={() => setDismissedTodo(todoItem!.id)} />
                    </div>
                  )}
                  {/* 本次产出:聚合会话写过的文件,右下角浮条,点条目在工作区面板打开 */}
                  <SessionArtifacts items={state.items} onOpenFile={openWorkspaceFile} />
                </div>
              </>
            )}
          </main>

          <footer className="footer" ref={footerRef}>
            {state.approval && (
              <ApprovalModal
                approval={state.approval}
                onAnswer={(allow, session) => {
                  // Approving an exit_plan_mode plan leaves plan mode (the controller
                  // flips the executor; mirror it here for the indicator).
                  if (state.approval!.tool === "exit_plan_mode" && allow) setMode("normal");
                  approve(state.approval!.id, allow, session);
                }}
                onRevisePlan={(text) => {
                  setPendingPlanRevision(text);
                  approve(state.approval!.id, false, false);
                }}
                onExitPlan={() => {
                  setMode("normal");
                  setPlan(false);
                  approve(state.approval!.id, false, false);
                }}
                onOpenFile={openWorkspaceFile}
              />
            )}
            <Composer
              running={state.running}
              mode={mode}
              cwd={state.meta?.cwd}
              onSend={handleSend}
              onCancel={cancel}
              onCycleMode={cycleMode}
              onPickFolder={switchFolder}
              knowledge={{
                bases: knowledgeBases,
                selected: selectedKnowledgeBaseIds,
                enabled: knowledgeEnabled,
                toggleBase: toggleKnowledgeBase,
                setAuto: setKnowledgeAuto,
                setEnabled: setKnowledgeEnabled,
                manage: () => setKnowledgeOpen(true),
              }}
              skills={skills}
              onManageSkills={() => setCapsOpen(true)}
              coach={{
                key: coachKey,
                setKey: setCoachKey,
                modes: COACH_MODES.map((m) => ({ key: m.key, label: t(m.labelKey), desc: t(m.descKey) })),
              }}
              disabled={state.meta?.ready === false || state.approval != null}
            />
            <StatusBar
              meta={state.meta}
              context={state.context}
	      usage={state.usage}
	      balance={state.balance}
	      effort={state.effort}
	      jobs={state.jobs}
              running={state.running}
              mode={mode}
              coachLabel={coachKey ? t(COACH_MODES.find((m) => m.key === coachKey)?.labelKey ?? "coach.default") : undefined}
              turnStartAt={state.turnStartAt}
	      turnTokens={state.turnTokens}
	      onSwitchModel={switchModel}
	      onSetEffort={setEffort}
	    />
          </footer>
        </section>

        {workspacePanelOpen && !workspacePanelMaximized && (
          <button
            className="workspace-panel-resizer"
            type="button"
            role="separator"
            aria-orientation="vertical"
            aria-label={t("workspace.resizePanel")}
            aria-valuemin={workspacePreviewModeActive ? WORKSPACE_PANEL_MIN_WIDTH : WORKSPACE_FILE_TREE_PANEL_MIN_WIDTH}
            aria-valuemax={workspacePreviewModeActive ? WORKSPACE_PANEL_MAX_WIDTH : WORKSPACE_FILE_TREE_PANEL_MAX_WIDTH}
            aria-valuenow={effectiveWorkspacePanelWidth}
            onPointerDown={startWorkspacePanelResize}
            onKeyDown={resizeWorkspacePanelWithKeyboard}
            onDoubleClick={() =>
              setSavedWorkspacePanelWidth(
                workspacePreviewModeActive ? WORKSPACE_PANEL_DEFAULT_WIDTH : WORKSPACE_FILE_TREE_PANEL_DEFAULT_WIDTH,
              )
            }
            title={t("workspace.resizePanel")}
          />
        )}

        <WorkspacePanel
          open={workspacePanelOpen}
          cwd={state.meta?.cwd}
          maximized={workspacePanelMaximized}
          panelWidth={workspacePanelMaximized ? viewportWidth - effectiveSidebarWidth : effectiveWorkspacePanelWidth}
          openFileRequest={workspaceOpenRequest}
          onClose={() => setWorkspacePanel(false)}
          onToggleMaximized={() => setWorkspacePanelMaximized((value) => !value)}
          onPreviewModeChange={handleWorkspacePreviewModeChange}
        />
      </div>

      {state.ask && (
        <AskCard
          ask={state.ask}
          onAnswer={answerQuestion}
          onDismiss={() => answerQuestion(state.ask!.id, [])}
        />
      )}

      {memView !== null && (
        <MemoryPanel
          view={memView}
          onClose={closeMemory}
          onRemember={onRemember}
          onForget={onForget}
          onSaveDoc={onSaveDoc}
        />
      )}

      {histView !== null && (
        <HistoryPanel
          sessions={histView}
          running={state.running}
          onResume={onResumeSession}
          onPreview={previewSession}
          onDelete={onDeleteSession}
          onRename={onRenameSession}
          onOpenFile={openWorkspaceFile}
          onClose={closeHistory}
        />
      )}

      {settingsOpen && <SettingsPanel onClose={() => setSettingsOpen(false)} onChanged={() => void refreshMeta()} />}
      {helpOpen && <HelpDrawer onClose={() => setHelpOpen(false)} />}

      {/* app 内置文件夹选择器(承诺式,全局一个;绕开 macOS 原生对话框跑窗口后面的 bug) */}
      <FolderPicker />

      {capsOpen && (
        <CapabilitiesPanel
          onClose={() => {
            setCapsOpen(false);
            void refreshSkillList();
          }}
        />
      )}

      {knowledgeOpen && (
        <KnowledgePanel
          onClose={() => {
            setKnowledgeOpen(false);
            void refreshKnowledgeBases();
          }}
          selectedBaseIds={selectedKnowledgeBaseIds}
          onSelectionChange={setSelectedKnowledgeBaseIds}
        />
      )}

      <ConfirmHost />
    </div>
  );
}
