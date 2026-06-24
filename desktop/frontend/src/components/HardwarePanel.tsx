import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Activity,
  AlertTriangle,
  ArrowLeft,
  CheckCircle2,
  ChevronDown,
  Copy,
  Cpu,
  Download,
  Eye,
  Hammer,
  Loader2,
  RefreshCw,
  Upload,
  Usb,
} from "lucide-react";
import { app, openExternal } from "../lib/bridge";
import { copyText } from "../lib/crash";
import { SerialMonitor } from "./SerialMonitor";
import { OTAPanel } from "./OTAPanel";
import type {
  CapabilitiesView,
  HardwareDetectView,
  HardwareEvidenceStatusView,
  HardwareMCPView,
  HardwareRunResult,
  ServerView,
} from "../lib/types";

type HardwareAction = {
  kind: "debug" | "validate" | "review";
  title: string;
  subtitle: string;
};

// 一键安装的单步实时状态(前端分步驱动,逐步刷新进度)。
type InstallStepUI = {
  tool: string;
  label: string;
  status: "pending" | "running" | "done" | "failed";
  message?: string;
};

type BoardPreset = {
  value: string;
  label: string;
  framework: string;
};

// (helper 组件 HardwarePathSummary / HardwareCommandSummary / HardwareFieldFile / FlowStep timeline /
//  HardwareFileHint 列表已删除 — 都是抽屉版的装饰元素,全屏 IDE 视图改用 toolbar + 精简卡片。)

// 「自定义板卡」是纯 UI 选项(不在注册表里),始终追加在板卡列表末尾。
const CUSTOM_BOARD: BoardPreset = { value: "custom", label: "自定义板卡", framework: "按用户说明" };

// 板卡选项现在从后端共享注册表(boards.json,经 HardwareBoardList)动态拉取——加一块板
// 只改 JSON,这里自动多一项。下面这份静态表只在后端不可用时兜底。
const FALLBACK_BOARD_PRESETS: BoardPreset[] = [
  { value: "arduino_uno", label: "Arduino UNO", framework: "Arduino IDE / Arduino CLI" },
  { value: "arduino_nano", label: "Arduino Nano", framework: "Arduino IDE / Arduino CLI" },
  { value: "esp32_arduino", label: "ESP32 Dev Module", framework: "Arduino / PlatformIO" },
  { value: "esp32_idf", label: "ESP32 ESP-IDF", framework: "ESP-IDF" },
  { value: "unihiker", label: "Unihiker 行空板", framework: "Python / SSH" },
  { value: "maixcam", label: "MaixCAM K230", framework: "MaixPy" },
  { value: "raspberry_pi", label: "Raspberry Pi", framework: "Python / SSH" },
  CUSTOM_BOARD,
];

const actions: HardwareAction[] = [
  {
    kind: "debug",
    title: "调试线程",
    subtitle: "从失败现象回到项目结构、编译和设备验证",
  },
  {
    kind: "validate",
    title: "验证线程",
    subtitle: "审计上下文、编译或语法检查、生成真机计划",
  },
  {
    kind: "review",
    title: "审查线程",
    subtitle: "教学可解释性、引脚风险、通信协议",
  },
];

const ACTION_RECIPES: Record<HardwareAction["kind"], string[]> = {
  debug: [
    "先复现或读取最近的失败现象，再调用 hardware_detect、hardware_project_audit、hardware_repair_catalog。",
    "优先区分工程结构、编译错误、烧录错误、串口无输出、端口占用、库缺失、接线/电平/供电问题。",
    "做最小修复后必须重新运行 hardware_project_validate；涉及真机时先给 device_verify_plan，再进入设备实验台动作。",
  ],
  validate: [
    "先调用 hardware_detect、hardware_project_audit、hardware_project_validate。",
    "上下文缺失时用 hardware_project_context 补齐，但不要覆盖已有内容。",
    "验证结束后调用 hardware_evidence_status，明确软件侧通过、真机待验证、已真机验证或证据过期。",
  ],
  review: [
    "检查代码是否符合教学可解释性：中文注释、命名清晰、魔数常量化、学生能逐行解释。",
    "结合 board_profile 和 module_spec 检查引脚、电平、供电、UART/I2C/SPI 通信风险。",
    "确认真实验证路径完整：编译、烧录、串口/运行日志、hardware_evidence_record。",
  ],
};

function buildWorkflowActionPrompt({
  action,
  detect,
  evidence,
}: {
  action: HardwareAction;
  detect: HardwareDetectView | null;
  evidence: HardwareEvidenceStatusView | null;
}): string {
  const projectSummary = detect
    ? [
        `项目目录：${detect.projectDir || "unknown"}`,
        `项目类型：${detect.projectTypes.join(" / ") || "unknown"}`,
        `可用工具链：${
          detect.toolchains
            .filter((tool) => tool.available)
            .map((tool) => tool.name)
            .join(", ") || "无"
        }`,
        `串口：${detect.serialPorts.join(", ") || "无"}`,
      ].join("\n")
    : "尚未取得本机检测结果，请先调用 hardware_detect。";
  const evidenceSummary = evidence
    ? [
        `状态：${evidence.status || "unknown"}`,
        `有效记录：${evidence.currentRecordCount || 0}`,
        `过期记录：${evidence.staleRecordCount || 0}`,
        `缺口：${evidence.missingGroups.join(" / ") || "无"}`,
      ].join("\n")
    : "尚未取得证据状态，请调用 hardware_evidence_status。";
  return [
    `请启动 OneCreat 硬件${action.title}。`,
    "",
    "工作边界：",
    "1. 项目线程负责方案、代码、审查、软件验证。",
    "2. 设备实验台只负责编译、烧录、串口、OTA 等真实设备动作。",
    "3. 证据账本必须记录软件验证和真机验证状态；没有真机证据时不能声称硬件完成。",
    "",
    "当前项目摘要：",
    projectSummary,
    "",
    "当前证据状态：",
    evidenceSummary,
    "",
    "本次 recipe：",
    ...ACTION_RECIPES[action.kind].map((step, index) => `${index + 1}. ${step}`),
    "",
    "请按上述步骤执行，并在每个阶段说明下一步是否需要打开设备实验台。",
  ].join("\n");
}

function findHardwareServer(view: CapabilitiesView | null): ServerView | undefined {
  return view?.servers.find((server) => server.name === "hardware");
}

// detectPlatformFromTypes 把 hardware_detect.projectTypes 映射到 platform 枚举,
// 供「一键编译/烧录/看串口」按钮决定调哪个 MCP 工具。返回空表示当前不是已识别的硬件项目。
function detectPlatformFromTypes(types: string[] | undefined): string {
  const lowered = (types ?? []).map((t) => t.toLowerCase());
  const has = (k: string) => lowered.some((t) => t.includes(k));
  if (has("platformio")) return "platformio";
  if (has("esp_idf") || has("esp-idf") || has("idf")) return "esp_idf";
  if (has("arduino")) return "arduino";
  if (has("micropython")) return "micropython";
  if (has("unihiker")) return "unihiker_python";
  if (has("maixcam")) return "maixcam_python";
  if (has("raspberry")) return "raspberry_pi_python";
  return "";
}

// 这几个平台靠 SSH 部署(scp 传代码 + ssh 跑 main.py),没法像 Arduino 那样
// 本地一键烧录:必须先知道设备 IP 和用户名。一键按钮对它们改走对话交接。
const SSH_PLATFORMS = new Set(["unihiker_python", "maixcam_python", "raspberry_pi_python"]);

// 同一件事(编译/烧录)连续失败到这个次数就升级:不再小修小补,提示从
// 接线/电平/供电/换思路整体排查,杜绝「改了→还是这个错→再改」空转。
const FAIL_ESCALATE_AT = 3;

function uniqueStrings(values: string[]): string[] {
  return Array.from(new Set(values.map((value) => value.trim()).filter(Boolean)));
}

function buildGuidePrompt({
  board,
  framework,
  port,
  task,
  detect,
  selectedKnowledgeCount,
  boardFacts,
}: {
  board: string;
  framework: string;
  port: string;
  task: string;
  detect: HardwareDetectView | null;
  selectedKnowledgeCount: number;
  boardFacts: string;
}): string {
  const detectSummary = detect
    ? `项目目录=${detect.projectDir || "unknown"}；项目类型=${detect.projectTypes.join(" / ") || "unknown"}；可用工具链=${
        detect.toolchains
          .filter((tool) => tool.available)
          .map((tool) => tool.name)
          .join(", ") || "无"
      }；检测到的串口=${detect.serialPorts.join(", ") || "无"}。`
    : "尚未取得本机检测结果。";
  return [
    "# OneCreat 硬件编程流程",
    "",
    "请严格按阶段执行。当前只允许进入“阶段 1：方案确认”，不要编写完整程序，不要创建文件，不要调用写入工具。",
    "",
    "## 用户选择",
    `- 板卡：${board || "未选择"}`,
    `- 开发方式：${framework || "未选择"}`,
    `- 上传端口：${port || "未选择"}`,
    `- 用户需求：${task}`,
    `- 知识库策略：${selectedKnowledgeCount ? `优先检索 ${selectedKnowledgeCount} 个已选知识库` : "未手动选择时自动检索全部本机知识库"}`,
    "",
    "## 本机检测摘要",
    detectSummary,
    "",
    // 板卡事实由后端从校验过的 catalog 直接取出并注入（不依赖模型自觉调工具）。
    ...(boardFacts.trim()
      ? [
          "## 板卡硬约束（已校验事实库，方案与代码必须遵守，禁止凭记忆改写引脚/库/API）",
          boardFacts.trim(),
          "",
        ]
      : []),
    "## 知识库与示例代码规则",
    "如果本轮消息包含“本地知识库片段”，必须先阅读这些片段，先说明引用了哪些示例/资料，再进行方案设计。",
    "如果片段中有代码示例、接线方式、库版本、引脚约定或课堂规范，后续方案和代码必须优先对齐这些内容。",
    "如果知识库没有命中相关资料，明确说明“未找到相关示例”，但仍然只能做阶段 1 方案，不要直接写代码。",
    "",
    "## 板卡 Profile 与项目上下文规则",
    "阶段 1 也必须先调用硬件 MCP 的 hardware_detect 和 hardware_board_profile，读取板卡默认引脚、风险引脚、电压、工具链和验证流程。",
    "如果当前项目目录已有代码，先调用 hardware_project_audit；若缺 hardware_manifest.json、docs/wiring.md、docs/verification.md、docs/board_profile.md、docs/failure_patterns.md 或 tests/hardware_checklist.md，只能建议或调用 hardware_project_context 补齐缺失上下文，不要覆盖客户已有内容。",
    "遇到已知失败时先查 hardware_repair_catalog，再决定是否调用自动修复工具。",
    "",
    "## 阶段 1 输出要求",
    "1. 先确认板卡、端口和外设是否足够；缺关键外设信息时最多问 3 个具体问题。",
    "2. 输出接线图：优先用 Mermaid flowchart 或清晰的 ASCII 连接图，标出数据流方向。",
    "3. 输出引脚说明表：模块/器件、板卡引脚、方向、协议、电压或注意事项。",
    "4. 输出程序逻辑：只写步骤、状态机或伪代码，不写完整源码。",
    "5. 输出依赖和工具链：Arduino IDE / PlatformIO / ESP-IDF / Python 等，以及需要安装的库。",
    "6. 输出上传与验证计划：包含端口、串口波特率、最小测试步骤、可能失败点。",
    "7. 输出安全与一致性检查清单（结合上面『板卡硬约束』里的电平和风险引脚逐条核对，有风险就在接线图里标出来）：",
    "   - 电平匹配：5V 器件信号进 3.3V 板（ESP32 / 行空板 / MaixCAM）的输入脚，必须分压或电平转换；",
    "   - 限流：LED 及部分器件要串限流电阻，不要直连 IO；",
    "   - 共地：所有模块、传感器、独立电源必须共 GND；",
    "   - 供电：电机 / 舵机 / 灯带等执行器要独立供电，不要从板子的 5V/3.3V 直接拉大电流；",
    "   - 多板通信一致性：板间 UART 必须 TX↔RX 交叉、两端波特率相同、且共地；I2C 必须共地且从机地址不冲突。",
    "8. 最后必须停下来询问用户是否确认接线和逻辑；只有用户确认后，下一轮才进入代码编写。",
    "",
    "## 用户确认后的硬件闭环规则（当前轮不要执行）",
    "1. 代码阶段必须先选择唯一工程体系：Arduino CLI 项目使用 项目名/项目名.ino；PlatformIO 项目必须使用 platformio.ini + src/main.cpp，不要把 .ino 只放在项目根目录。",
    "2. 写代码前先确保项目上下文齐全：hardware_project_context 可补齐 manifest、接线、验证、board profile、failure patterns 和检查清单。",
    "3. 写完代码后必须调用硬件 MCP 的 hardware_project_audit 和 hardware_project_validate；如果失败，先查 hardware_repair_catalog，再修复工程结构或编译错误，并重新验证。",
    "   - 若是 PlatformIO 根目录 .ino 问题，优先调用 hardware_project_repair repair=platformio_root_ino_to_src_main 迁移到 src/main.cpp，并重新审计和编译。",
    "4. 只有编译通过且检测到真实端口后，才进入 upload/flash；上传后必须采集串口/运行日志，并用 hardware_evidence_record 记录证据。",
    "5. 没有编译、烧录和串口证据时，只能说“软件侧已验证/真实硬件待验证”，不能声称硬件项目完成。",
  ].join("\n");
}

// buildDirectPrompt 是「直接写代码」模式:适合简单项目,跳过单独的方案确认阶段,一轮内
// 直接完成从写代码到编译验证的闭环。但仍强制先读板卡 profile / 模块规格 / 知识库,保证
// 引脚、库和 API 正确(防幻觉),只是不停下来等用户确认接线。
function buildDirectPrompt({
  board,
  framework,
  port,
  task,
  detect,
  selectedKnowledgeCount,
  boardFacts,
}: {
  board: string;
  framework: string;
  port: string;
  task: string;
  detect: HardwareDetectView | null;
  selectedKnowledgeCount: number;
  boardFacts: string;
}): string {
  const detectSummary = detect
    ? `项目目录=${detect.projectDir || "unknown"}；项目类型=${detect.projectTypes.join(" / ") || "unknown"}；可用工具链=${
        detect.toolchains
          .filter((tool) => tool.available)
          .map((tool) => tool.name)
          .join(", ") || "无"
      }；检测到的串口=${detect.serialPorts.join(", ") || "无"}。`
    : "尚未取得本机检测结果。";
  return [
    "# OneCreat 硬件编程流程（直接写代码模式）",
    "",
    "这是一个相对简单的项目，跳过单独的“方案确认”阶段，本轮直接完成从写代码到编译验证的闭环。",
    "但仍然必须先读板卡 profile、模块规格和知识库，保证引脚、库和 API 正确——不要凭记忆猜。",
    "",
    "## 用户选择",
    `- 板卡：${board || "未选择"}`,
    `- 开发方式：${framework || "未选择"}`,
    `- 上传端口：${port || "未选择"}`,
    `- 用户需求：${task}`,
    `- 知识库策略：${selectedKnowledgeCount ? `优先检索 ${selectedKnowledgeCount} 个已选知识库` : "未手动选择时自动检索全部本机知识库"}`,
    "",
    "## 本机检测摘要",
    detectSummary,
    "",
    // 板卡事实由后端从校验过的 catalog 直接取出并注入（不依赖模型自觉调工具）。
    ...(boardFacts.trim()
      ? [
          "## 板卡硬约束（已校验事实库，代码必须遵守，禁止凭记忆改写引脚/库/API）",
          boardFacts.trim(),
          "",
        ]
      : []),
    "## 必须先做（防止幻觉，写代码前完成）",
    "1. 先调用硬件 MCP 的 hardware_detect 和 hardware_board_profile，读板卡默认引脚、风险引脚、电压、工具链和验证流程。",
    "2. 涉及具体模块（传感器/舵机/电机驱动/显示屏等）时调用 hardware_module_spec，读已校验的库、引脚、I2C 地址和 gotchas；例如 ESP32 的 PWM 用 LEDC 不要 analogWrite、舵机用 ESP32Servo，一切以 board profile / module_spec 为准。",
    "3. 如果本轮消息包含“本地知识库片段”或示例工程，先对齐其中的库版本、引脚约定和写法。",
    "4. 安全自检（结合上面板卡硬约束）：5V 信号进 3.3V 输入脚要电平转换、LED/器件按需串限流电阻、所有模块共 GND、执行器独立供电；多板项目板间 UART 要 TX↔RX 交叉+同波特率+共地。代码注释里把这些接线要点写清楚，方便学生照接。",
    "",
    "## 直接执行（不要停下来等用户确认接线）",
    "1. 选择唯一工程体系：Arduino CLI 用 项目名/项目名.ino；PlatformIO 用 platformio.ini + src/main.cpp（.ino 不要只放在项目根目录）。必要时先调用 hardware_project_context 补齐项目上下文。",
    "2. 写完整、可读、带中文注释的代码：引脚号提取成命名常量，学生要能逐行解释。开头用一两句话说明选了哪块板、关键引脚和用到的库，但不要停下来等确认，直接往下写。",
    "3. 写完调用 hardware_project_audit 和 hardware_project_validate；失败先查 hardware_repair_catalog 再做最小修复并重新验证（PlatformIO 根目录 .ino 问题用 hardware_project_repair repair=platformio_root_ino_to_src_main）。",
    "4. 编译通过且检测到真实端口后再 upload/flash；上传后采集串口/运行日志，并用 hardware_evidence_record 记录证据。",
    "5. 没有编译、烧录和串口证据时，只说“软件侧已验证 / 真实硬件待验证”，不要声称硬件项目完成。",
    "6. 最后给学生一句话总结：接线怎么接、代码做了什么、怎么验证。",
  ].join("\n");
}

// 把后端的英文状态枚举翻成学生能懂的中文标签。
const EVIDENCE_STATUS_LABEL: Record<string, string> = {
  hardware_verified: "✅ 已通过真机验证",
  hardware_pending: "⏳ 真机待验证",
  local_pending: "软件侧已验证",
  stale: "验证证据已过期",
  failed: "验证失败",
  no_evidence: "尚无验证记录",
  unavailable: "未启用",
};
function evidenceStatusText(status?: string): string {
  if (!status) return "未知";
  return EVIDENCE_STATUS_LABEL[status] ?? status;
}
// 用本地计数拼一句中文说明,替代后端那句英文 summary。
function evidenceSummaryText(ev: HardwareEvidenceStatusView): string {
  const cur = ev.currentRecordCount ?? 0;
  const stale = ev.staleRecordCount ?? 0;
  if (cur + stale === 0) return "还没有验证记录;编译、烧录、看串口后会自动记下。";
  const parts = [`已记录 ${cur} 条有效验证（软件编译 + 真机运行）`];
  if (stale > 0) parts.push(`另有 ${stale} 条因代码改动已过期`);
  return parts.join("，") + "。";
}

// 未安装工具的「安装」入口先打开官方安装页(真能帮到);一键自动安装需后端跑
// brew/pip,属后续增量。按工具名模糊匹配,匹配不到就回退到搜索。
const TOOL_INSTALL_URL: { match: string; url: string }[] = [
  { match: "arduino-cli", url: "https://arduino.github.io/arduino-cli/latest/installation/" },
  { match: "platformio", url: "https://platformio.org/install/cli" },
  { match: "esp-idf", url: "https://docs.espressif.com/projects/esp-idf/en/stable/esp32/get-started/" },
  { match: "eim", url: "https://github.com/espressif/idf-im-ui/releases" },
  { match: "espressif", url: "https://docs.espressif.com/projects/esp-idf/en/stable/esp32/get-started/" },
  { match: "mpremote", url: "https://docs.micropython.org/en/latest/reference/mpremote.html" },
];
function toolInstallUrl(name: string): string {
  const n = name.toLowerCase();
  const hit = TOOL_INSTALL_URL.find((e) => n.includes(e.match));
  return hit ? hit.url : `https://www.google.com/search?q=${encodeURIComponent("install " + name)}`;
}

function formatDuration(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  if (m <= 0) return `${s}s`;
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

export function HardwarePanel({
  onPrompt,
  onOpenWorkspace,
  onBackToChat,
  selectedKnowledgeCount,
  active = true,
}: {
  onPrompt: (display: string, submit?: string) => void;
  onOpenWorkspace?: (path?: string) => void;
  onBackToChat?: () => void;
  selectedKnowledgeCount: number;
  active?: boolean;
}) {
  const [view, setView] = useState<CapabilitiesView | null>(null);
  const [hardwareMCP, setHardwareMCP] = useState<HardwareMCPView | null>(null);
  const [detect, setDetect] = useState<HardwareDetectView | null>(null);
  const [evidence, setEvidence] = useState<HardwareEvidenceStatusView | null>(null);
  const [board, setBoard] = useState("esp32_arduino");
  const [framework, setFramework] = useState("Arduino / PlatformIO");
  const [port, setPort] = useState("");
  const [task, setTask] = useState("");
  const [busy, setBusy] = useState(false);
  const [portRefreshing, setPortRefreshing] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // 串口监视器(常驻窗口,像 Arduino IDE)是否打开。
  const [serialMonitorOpen, setSerialMonitorOpen] = useState(false);
  // 项目条上「工具就绪 X/Y」点开后,列出具体每个工具是否就绪。
  const [toolchainsOpen, setToolchainsOpen] = useState(false);
  // 一键安装核心工具链(arduino-cli + 板卡 core)的实时分步状态。
  const [installing, setInstalling] = useState(false);
  const [installSteps, setInstallSteps] = useState<InstallStepUI[] | null>(null);
  const [installStartedAt, setInstallStartedAt] = useState<number | null>(null);
  const [installElapsed, setInstallElapsed] = useState(0);
  // 板卡选项:来自后端共享注册表,加板=改 JSON 即自动多一项;失败兜底静态表。
  const [boardPresets, setBoardPresets] = useState<BoardPreset[]>(FALLBACK_BOARD_PRESETS);
  useEffect(() => {
    app
      .HardwareBoardList()
      .then((list) => {
        if (list && list.length) {
          setBoardPresets([...list.map((b) => ({ value: b.value, label: b.label, framework: b.framework })), CUSTOM_BOARD]);
        }
      })
      .catch(() => {});
  }, []);

  const reload = useCallback(async () => {
    const [capabilities, hardware, detected, evidenceStatus] = await Promise.all([
      app.Capabilities().catch(() => ({ servers: [], skills: [], skillRoots: [] })),
      app.HardwareMCP().catch(() => null),
      app.HardwareDetect().catch((e) => ({
        available: false,
        projectTypes: [],
        serialPorts: [],
        boards: [],
        devices: [],
        toolchains: [],
        recommendations: [],
        error: String((e as Error)?.message ?? e),
      })),
      app.HardwareEvidenceStatus().catch((e) => ({
        available: false,
        recordCount: 0,
        currentRecordCount: 0,
        staleRecordCount: 0,
        status: "unavailable",
        summary: "",
        missingGroups: [],
        recommendations: [],
        error: String((e as Error)?.message ?? e),
      })),
    ]);
    setView(capabilities);
    setHardwareMCP(hardware);
    setDetect(detected);
    setEvidence(evidenceStatus);
  }, []);

  useEffect(() => {
    // 仅在视图可见时拉取硬件状态,切回 chat 视图时不再轮询。
    if (active) void reload();
  }, [reload, active]);

  useEffect(() => {
    if (!installing || installStartedAt === null) return;
    const tick = () => setInstallElapsed(Math.floor((Date.now() - installStartedAt) / 1000));
    tick();
    const timer = window.setInterval(tick, 1000);
    return () => window.clearInterval(timer);
  }, [installStartedAt, installing]);

  const refreshSerialPorts = useCallback(async () => {
    setPortRefreshing(true);
    setErr(null);
    try {
      const ports = await app.SerialPorts();
      setDetect((prev) => ({
        available: prev?.available ?? true,
        workspace: prev?.workspace,
        projectDir: prev?.projectDir,
        projectTypes: prev?.projectTypes ?? [],
        serialPorts: ports,
        boards: prev?.boards.filter((item) => ports.includes(item.port)) ?? [],
        devices: prev?.devices.filter((item) => ports.includes(item.port)) ?? [],
        toolchains: prev?.toolchains ?? [],
        recommendations: prev?.recommendations ?? [],
        espIdfOfficialMcp: prev?.espIdfOfficialMcp,
        error: prev?.error,
      }));
      setPort((current) => {
        if (current && ports.includes(current)) return current;
        return ports[0] ?? "";
      });
    } catch (e) {
      setErr(`刷新串口失败:${String((e as Error)?.message ?? e)}`);
    } finally {
      setPortRefreshing(false);
    }
  }, []);

  // 一键安装核心工具链:前端分步驱动(先 arduino-cli,再逐个 core),
  // 每步开始/完成都即时刷新进度,用户能清楚看到「正在装哪一步」。装完重新检测。
  const installToolchain = useCallback(async () => {
    const coreList = [
      { tool: "arduino:avr", label: "Arduino UNO / Nano / Mega 板卡" },
      { tool: "esp32:esp32", label: "ESP32 全系板卡（约 200MB，较慢）" },
    ];
    let steps: InstallStepUI[] = [
      { tool: "arduino-cli", label: "arduino-cli（编译 / 烧录核心工具）", status: "pending", message: "等待开始" },
      ...coreList.map((c) => ({ tool: c.tool, label: c.label, status: "pending" as const, message: "等待开始" })),
    ];
    const patch = (tool: string, next: Partial<InstallStepUI>) => {
      steps = steps.map((s) => (s.tool === tool ? { ...s, ...next } : s));
      setInstallSteps([...steps]);
    };
    setErr(null);
    setInstallStartedAt(Date.now());
    setInstallElapsed(0);
    setInstalling(true);
    setInstallSteps([...steps]);
    try {
      patch("arduino-cli", { status: "running", message: "正在检查；未安装会下载官方 arduino-cli" });
      const cli = await app.HardwareInstallArduinoCLI();
      patch("arduino-cli", { status: cli.ok ? "done" : "failed", message: cli.message });
      if (cli.ok) {
        for (const c of coreList) {
          patch(c.tool, {
            status: "running",
            message: c.tool === "esp32:esp32" ? "正在安装 ESP32 core，约 200MB，可能需要几分钟" : "正在安装 Arduino AVR core",
          });
          const r = await app.HardwareInstallCore(c.tool);
          patch(c.tool, { status: r.ok ? "done" : "failed", message: r.message });
        }
      } else {
        coreList.forEach((c) => patch(c.tool, { status: "failed", message: "arduino-cli 未装上，已跳过" }));
      }
    } catch (e) {
      const msg = String((e as Error)?.message ?? e);
      steps = steps.map((s) => (s.status === "running" || s.status === "pending" ? { ...s, status: "failed", message: msg } : s));
      setInstallSteps([...steps]);
    } finally {
      setInstalling(false);
      await reload(); // 刷新工具链 ✅/⚠️ 状态
    }
  }, [reload]);

  const hardware = useMemo(() => findHardwareServer(view), [view]);
  const connected = hardware?.status === "connected";
  const availableToolchains = detect?.toolchains.filter((tool) => tool.available).length ?? 0;
  const detectedPorts = useMemo(
    () =>
      uniqueStrings([
        ...(detect?.boards.map((item) => item.port) ?? []),
        ...(detect?.devices.map((item) => item.port) ?? []),
        ...(detect?.serialPorts ?? []),
      ]),
    [detect],
  );
  const boardOptions = useMemo(() => {
    const detected = (detect?.boards ?? [])
      .map((item) => ({
        value: `detected:${item.port}:${item.fqbn || item.boardName || item.core || item.port}`,
        label: item.boardName || item.fqbn || item.core || item.port,
        framework: item.fqbn?.includes("esp32") ? "Arduino / PlatformIO" : "按检测结果",
      }))
      .filter((item) => item.label);
    return [...boardPresets, ...detected];
  }, [detect, boardPresets]);
  const selectedBoard = boardOptions.find((item) => item.value === board);
  const evidenceTone =
    evidence?.status === "hardware_verified"
      ? "ok"
      : evidence?.status === "hardware_pending" || evidence?.status === "local_pending"
        ? "pending"
        : "warn";
  const installTotal = installSteps?.length ?? 0;
  const installDoneCount = installSteps?.filter((step) => step.status === "done").length ?? 0;
  const installFailedCount = installSteps?.filter((step) => step.status === "failed").length ?? 0;
  const installRunningStep = installSteps?.find((step) => step.status === "running");
  const installAllDone = !!installSteps && installSteps.length > 0 && installSteps.every((step) => step.status === "done");
  const installTone = installing ? "running" : installAllDone ? "ok" : "warn";
  const installSummary = installing
    ? `安装中 ${installDoneCount}/${installTotal} · ${formatDuration(installElapsed)}`
    : installAllDone
      ? "工具链已就绪"
      : installSteps
        ? `安装未完成 ${installFailedCount}/${installTotal}`
        : "";

  // 「一键运行」按钮:把学生最常用的三件事 — 编译/烧录/看串口 —
  // 直接绑到对应 MCP 工具,不必再绕一圈到对话里让 AI 决定调什么。
  const detectedPlatform = useMemo(() => detectPlatformFromTypes(detect?.projectTypes), [detect?.projectTypes]);
  // OTA 用的板卡值:检测到的真实板 value 形如 detected:port:fqbn,要抽出真 fqbn(同 runOneTouch)。
  const resolvedBoard = useMemo(() => (board.startsWith("detected:") ? board.split(":").slice(2).join(":") : board), [board]);
  const hasHardwareProject = !!detectedPlatform && !!detect?.projectDir;
  const [running, setRunning] = useState<{ validate: boolean; upload: boolean; monitor: boolean }>({
    validate: false,
    upload: false,
    monitor: false,
  });
  const [runResults, setRunResults] = useState<{ validate?: HardwareRunResult; upload?: HardwareRunResult; monitor?: HardwareRunResult }>({});
  // 同一件事连续失败计数(成功即清零)。达到 FAIL_ESCALATE_AT 触发「换思路」升级提示。
  const [failCount, setFailCount] = useState<{ validate: number; upload: number; monitor: number }>({ validate: 0, upload: 0, monitor: 0 });

  const runOneTouch = useCallback(
    async (kind: "validate" | "upload" | "monitor") => {
      if (!detectedPlatform || !detect?.projectDir) return;
      app.MarkSessionKind("hardware").catch(() => {}); // 真正跑硬件动作 → 标记会话为硬件项目

      // SSH 平台（行空板/MaixCAM/树莓派）的「烧录/看串口」没法本地一键完成：
      // 必须知道设备 IP 和用户名才能 scp + ssh。不再返回死的「skipped」，
      // 而是把带好工程路径和运行命令的 SSH 部署提示直接交给对话，
      // AI 缺设备地址就问学生，再真正调用 ssh_deploy_run。
      if ((kind === "upload" || kind === "monitor") && SSH_PLATFORMS.has(detectedPlatform)) {
        const boardLabel = selectedBoard?.label || board || "目标设备";
        const action = kind === "upload" ? "部署并运行" : "运行并采集串口/运行日志";
        const parts = [
          `请把当前硬件项目通过 SSH ${action}到 ${boardLabel}。`,
          `工程目录：${detect.projectDir}`,
          "步骤：先调用 mcp__hardware__hardware_device_verify_plan 生成准确的部署/运行/串口命令；再调用 mcp__hardware__ssh_deploy_run 把代码传到设备并执行 main.py。",
          "如果你还不知道设备的 IP 地址、SSH 用户名（行空板默认 root），先问我，不要自己编。",
          "部署运行后采集真实输出，并调用 mcp__hardware__hardware_evidence_record 记录证据。",
        ];
        onPrompt(`通过 SSH ${kind === "upload" ? "部署" : "看串口"}到 ${boardLabel}`, parts.join("\n"));
        onBackToChat?.();
        return;
      }
      // 后端按「板卡 id」映射 FQBN（esp32_arduino → esp32:esp32:esp32）。
      // 预设板直接传 value（id）；检测到的真实板 value 形如
      // detected:串口:fqbn，要把后面的真实 fqbn 抽出来透传，否则会被
      // 当成非法 FQBN 导致编译第一步就报错。
      const runBoard = board.startsWith("detected:")
        ? board.split(":").slice(2).join(":")
        : board;
      const input = {
        projectDir: detect.projectDir,
        platform: detectedPlatform,
        board: runBoard,
        port,
        seconds: kind === "monitor" ? 8 : undefined,
      };
      setRunning((r) => ({ ...r, [kind]: true }));
      try {
        const callMap = { validate: app.HardwareValidate, upload: app.HardwareUpload, monitor: app.HardwareMonitor };
        const result = await callMap[kind](input);
        setRunResults((prev) => ({ ...prev, [kind]: result }));
        // 失败累加、成功清零(monitor 只是看串口,不计入失败护栏)。
        if (kind !== "monitor") {
          setFailCount((prev) => ({ ...prev, [kind]: result.status === "failed" ? prev[kind] + 1 : 0 }));
        }
      } catch (e) {
        setRunResults((prev) => ({
          ...prev,
          [kind]: { status: "failed", summary: "调用失败", error: String((e as Error)?.message ?? e) },
        }));
        if (kind !== "monitor") {
          setFailCount((prev) => ({ ...prev, [kind]: prev[kind] + 1 }));
        }
      } finally {
        setRunning((r) => ({ ...r, [kind]: false }));
      }
    },
    [board, detect?.projectDir, detectedPlatform, port, selectedBoard, onPrompt, onBackToChat],
  );

  // 失败时学生点「让 AI 排查」就把已蒸馏的根因+修法+输出摘要直接塞给 chat。
  // 编译/烧录失败可以进入“修复 -> validate”闭环；串口无输出更常见是板子运行、
  // 波特率、端口占用或硬件状态问题，默认只读诊断，不诱导 AI 空改代码。
  const askAIToFix = useCallback(
    (kind: "validate" | "upload" | "monitor", result: HardwareRunResult) => {
      const label = { validate: "编译/验证", upload: "烧录", monitor: "串口" }[kind];
      if (kind === "monitor") {
        const parts: string[] = [
          "刚才点了「串口」按钮,没有采到输出。请做只读排查,不要默认编辑代码。",
          result.summary ? `结果摘要:${result.summary}` : "",
          result.rootCause ? `根因:${result.rootCause}` : "",
          result.fixHint ? `已知修法提示:${result.fixHint}` : "",
          result.error ? `错误:${result.error}` : "",
          result.output ? `输出片段:\n\`\`\`\n${result.output.slice(0, 2000)}\n\`\`\`` : "",
          "排查边界:",
          "1. 先读取当前入口文件,确认是否有 Serial.begin(115200) 以及 setup/loop 中是否真的会 Serial.print/println。",
          "2. 如果代码里已有 Serial.begin 且有明确输出语句,不要改文件,不要重跑完整编译;直接报告「串口无输出,真机运行待确认」,并给出最小人工检查:端口、波特率、板子是否复位运行、USB CDC 设置、是否刚烧录了正确固件。",
          "3. 不要用 bash 的 screen / cu / cat / timeout 反复读串口;这些在本机不可靠。若确需重试,最多调用一次 mcp__hardware__arduino_monitor_sample。",
          "4. 只有当你从代码中明确发现缺少 Serial.begin、波特率不一致、或没有任何输出语句时,才做最小代码修改;改完后再调用 mcp__hardware__hardware_project_validate 重新编译验证。",
          "5. 不要伪造串口证据;没有真实输出就如实说明。",
        ].filter(Boolean);
        onPrompt("让 AI 排查串口无输出", parts.join("\n\n"));
        onBackToChat?.();
        return;
      }

      const count = failCount[kind];
      const escalated = count >= FAIL_ESCALATE_AT;
      const header = escalated
        ? `「${label}」已经连续失败 ${count} 次。不要再小修小补——请退一步，从整体排查:接线、电平(5V 与 3.3V 是否需要电平转换)、限流电阻、共地 GND、供电是否独立、库版本是否匹配,或换一种实现思路。`
        : `刚才点了「${label}」按钮,失败了。请帮我排查并给出最小修复。`;
      const parts: string[] = [
        header,
        result.summary ? `结果摘要:${result.summary}` : "",
        result.rootCause ? `根因:${result.rootCause}` : "",
        result.fixHint ? `已知修法提示:${result.fixHint}` : "",
        result.error ? `错误:${result.error}` : "",
        result.output ? `输出片段:\n\`\`\`\n${result.output.slice(0, 2000)}\n\`\`\`` : "",
        "改完后请直接调用 mcp__hardware__hardware_project_validate 重新编译验证,把通过/失败结果告诉我;不要假设已修好,通过了再继续。",
      ].filter(Boolean);
      onPrompt(escalated ? `连续失败${count}次,换思路排查${label}` : `让 AI 排查${label}失败`, parts.join("\n\n"));
      onBackToChat?.();
    },
    [failCount, onBackToChat, onPrompt],
  );

  // 把真机验证记录(tests/hardware_evidence.jsonl)汇总成 Markdown 复制到剪贴板,
  // 学生直接粘进研究日志/论文——竞赛材料要用真实采集的证据,不能凭记忆编数字。
  const [exportMsg, setExportMsg] = useState("");
  const exportEvidence = useCallback(async () => {
    setExportMsg("");
    const md = await app.HardwareEvidenceExport(detect?.projectDir || "").catch(() => "");
    if (!md.trim()) {
      setExportMsg("还没有验证记录");
      return;
    }
    const ok = await copyText(md);
    setExportMsg(ok ? "已复制，可粘进研究日志 / 论文" : "复制失败，请手动选择");
  }, [detect?.projectDir]);

  const enableHardware = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      await app.AddHardwareMCPServer();
      await reload();
    } catch (e) {
      setErr(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [reload]);

  const runGuidedPlan = useCallback(async () => {
    const trimmedTask = task.trim();
    if (!trimmedTask) {
      setErr("请先说明要编写什么程序。");
      return;
    }
    app.MarkSessionKind("hardware").catch(() => {}); // 生成硬件代码 → 标记会话为硬件项目
    setBusy(true);
    setErr(null);
    try {
      if (!hardwareMCP?.available) {
        throw new Error("未找到 hardware MCP 二进制，无法执行硬件工作流。");
      }
      if (!connected) {
        await app.AddHardwareMCPServer();
      }
      // 写代码/出方案前，确定性取出已选板卡的校验事实硬注入 prompt（防 flash 幻觉）。
      // 取失败不致命：回退到原有「让模型自己调 board_profile/module_spec」的流程。
      let boardFacts = "";
      try {
        const f = await app.HardwareBoardFacts(board, "");
        if (f?.found) boardFacts = f.facts;
      } catch {
        boardFacts = "";
      }
      const boardLabel = selectedBoard?.label || board || "未选择";
      const prompt = buildGuidePrompt({
        board: boardLabel,
        framework,
        port,
        task: trimmedTask,
        detect,
        selectedKnowledgeCount,
        boardFacts,
      });
      const display = `硬件方案确认：${boardLabel}${port ? ` · ${port}` : ""} · ${trimmedTask}`;
      onPrompt(display, prompt);
      onBackToChat?.();
    } catch (e) {
      setErr(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [board, connected, detect, framework, hardwareMCP?.available, onBackToChat, onPrompt, port, selectedBoard?.label, selectedKnowledgeCount, task]);

  // runDirectCode：「直接写代码」模式——简单项目跳过方案确认,一轮内直接写码+编译验证
  // (仍先读 board profile / module_spec / 知识库防幻觉)。
  const runDirectCode = useCallback(async () => {
    const trimmedTask = task.trim();
    if (!trimmedTask) {
      setErr("请先说明要编写什么程序。");
      return;
    }
    app.MarkSessionKind("hardware").catch(() => {}); // 直接生成硬件代码 → 标记会话为硬件项目
    setBusy(true);
    setErr(null);
    try {
      if (!hardwareMCP?.available) {
        throw new Error("未找到 hardware MCP 二进制，无法执行硬件工作流。");
      }
      if (!connected) {
        await app.AddHardwareMCPServer();
      }
      // 写代码前，确定性取出已选板卡的校验事实硬注入 prompt（防 flash 幻觉）。
      let boardFacts = "";
      try {
        const f = await app.HardwareBoardFacts(board, "");
        if (f?.found) boardFacts = f.facts;
      } catch {
        boardFacts = "";
      }
      const boardLabel = selectedBoard?.label || board || "未选择";
      const prompt = buildDirectPrompt({
        board: boardLabel,
        framework,
        port,
        task: trimmedTask,
        detect,
        selectedKnowledgeCount,
        boardFacts,
      });
      const display = `直接写代码：${boardLabel}${port ? ` · ${port}` : ""} · ${trimmedTask}`;
      onPrompt(display, prompt);
      onBackToChat?.();
    } catch (e) {
      setErr(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [board, connected, detect, framework, hardwareMCP?.available, onBackToChat, onPrompt, port, selectedBoard?.label, selectedKnowledgeCount, task]);

  const runAction = useCallback(
    async (action: HardwareAction) => {
      setBusy(true);
      setErr(null);
      try {
        if (!hardwareMCP?.available) {
          throw new Error("未找到 hardware MCP 二进制，无法执行硬件工作流。");
        }
        if (!connected) {
          await app.AddHardwareMCPServer();
        }
        onPrompt(action.title, buildWorkflowActionPrompt({ action, detect, evidence }));
        onBackToChat?.();
      } catch (e) {
        setErr(String((e as Error)?.message ?? e));
      } finally {
        setBusy(false);
      }
    },
    [connected, detect, evidence, hardwareMCP?.available, onBackToChat, onPrompt],
  );

  const openWorkspace = useCallback(
    (path?: string) => {
      // 在硬件视图内打开文件,Workspace 面板从右侧滑出,硬件视图保留可见
      onOpenWorkspace?.(path);
    },
    [onOpenWorkspace],
  );

  return (
    <div className="hardware-view">
      {/* 设备实验台顶部工具栏:板卡/串口/框架 + 一键编译烧录看串口 + MCP 徽章 + 回到项目线程 */}
      <div className="hardware-view__toolbar">
        <div className="hardware-view__toolbar-group hardware-view__toolbar-group--selects">
          <label className="hardware-view__field">
            <span>板卡</span>
            <select
              value={board}
              onChange={(event) => {
                const next = event.target.value;
                setBoard(next);
                const option = boardOptions.find((item) => item.value === next);
                if (option?.framework) setFramework(option.framework);
              }}
            >
              {boardOptions.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
          </label>
          <div className="hardware-view__port-picker">
            <label className="hardware-view__field">
              <span>串口</span>
              <select value={port} onChange={(event) => setPort(event.target.value)}>
                <option value="">{detectedPorts.length ? "未选" : "无设备"}</option>
                {detectedPorts.map((item) => (
                  <option key={item} value={item}>
                    {item}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="chip chip--icon hardware-view__port-refresh"
              disabled={portRefreshing}
              onClick={() => void refreshSerialPorts()}
              title="刷新串口列表"
            >
              <RefreshCw size={13} className={portRefreshing ? "hardware-spin" : undefined} />
            </button>
          </div>
          <label className="hardware-view__field">
            <span>框架</span>
            <select value={framework} onChange={(event) => setFramework(event.target.value)}>
              {uniqueStrings([
                selectedBoard?.framework || "",
                "Arduino IDE / Arduino CLI",
                "Arduino / PlatformIO",
                "ESP-IDF",
                "MicroPython",
                "MaixPy",
                "Python / SSH",
                "按用户说明",
              ]).map((item) => (
                <option key={item} value={item}>
                  {item}
                </option>
              ))}
            </select>
          </label>
        </div>

        <div className="hardware-view__toolbar-group hardware-view__toolbar-group--actions">
          <button
            className="hardware-view__btn hardware-view__btn--primary"
            disabled={!hasHardwareProject || !hardwareMCP?.available || running.validate}
            onClick={() => void runOneTouch("validate")}
            title={!hasHardwareProject ? "当前不是硬件项目" : "编译当前项目"}
          >
            {running.validate ? <Loader2 size={14} className="hardware-spin" /> : <Hammer size={14} />}
            <span>编译</span>
          </button>
          <button
            className="hardware-view__btn"
            disabled={!hasHardwareProject || !hardwareMCP?.available || running.upload || (!port && (detectedPlatform === "arduino" || detectedPlatform === "micropython"))}
            onClick={() => void runOneTouch("upload")}
            title={!port && (detectedPlatform === "arduino" || detectedPlatform === "micropython") ? "请先选串口" : "烧录到开发板"}
          >
            {running.upload ? <Loader2 size={14} className="hardware-spin" /> : <Upload size={14} />}
            <span>烧录</span>
          </button>
          <button
            className="hardware-view__btn"
            disabled={!hasHardwareProject || !hardwareMCP?.available || running.monitor || (!port && detectedPlatform === "arduino")}
            onClick={() => void runOneTouch("monitor")}
            title="快速采样 8 秒串口并记入验证证据(给竞赛留证据用)"
          >
            {running.monitor ? <Loader2 size={14} className="hardware-spin" /> : <Eye size={14} />}
            <span>看串口</span>
          </button>
          <button
            className="hardware-view__btn"
            onClick={() => setSerialMonitorOpen(true)}
            title="打开常驻串口窗口(像 Arduino IDE:选波特率、实时查看数据、发送指令调试)"
          >
            <Activity size={14} />
            <span>串口监视器</span>
          </button>
        </div>

        <div className="hardware-view__toolbar-group hardware-view__toolbar-group--right">
          <div
            className={`hardware-view__mcp hardware-view__mcp--${connected ? "ok" : hardwareMCP?.available ? "warn" : "off"}`}
            title="硬件助手 = 让 AI 能真的检测板卡 / 编译 / 烧录 / 看串口的后端(硬件 MCP）。显示「已启用」才说明它接进了当前对话，上面的编译/烧录/看串口才能用。"
          >
            <span className="hardware-view__mcp-dot" />
            <span className="hardware-view__mcp-label">
              {connected ? "硬件助手已启用" : hardwareMCP?.available ? "硬件助手未启用" : "硬件助手未安装"}
            </span>
            {!connected && hardwareMCP?.available && (
              <button className="hardware-view__mcp-btn" disabled={busy} onClick={() => void enableHardware()}>
                {busy ? "..." : "启用"}
              </button>
            )}
          </div>
          <button className="chip chip--icon" disabled={busy} onClick={() => void reload()} title="刷新">
            <RefreshCw size={13} />
          </button>
          {onBackToChat && (
            <button className="hardware-view__back" onClick={onBackToChat} title="回到项目线程">
              <ArrowLeft size={13} />
              <span>项目线程</span>
            </button>
          )}
        </div>
      </div>

      <div className="hardware-view__body">
        {err && <div className="banner banner--error">{err}</div>}
        {hardwareMCP?.error && !hardwareMCP.available && <div className="banner banner--error">{hardwareMCP.error}</div>}
        {detect?.error && <div className="banner banner--error">{detect.error}</div>}

        <section className="hardware-workflow" aria-label="硬件三线流程">
          <div className="hardware-workflow__intro">
            <div className="hardware-workflow__eyebrow">设备实验台</div>
            <h2>真实硬件动作在这里做，项目讨论回到对话线程</h2>
            <p>编译、烧录、串口和 OTA 会占用真实设备资源；方案、代码、审查和修复由对话线程推进，证据账本记录验证状态。</p>
          </div>
          <div className="hardware-workflow__lanes">
            <div className="hardware-workflow__lane">
              <span className="hardware-workflow__lane-label">项目线程</span>
              <strong>{hasHardwareProject ? "已识别项目" : "等待项目"}</strong>
              <small>{detect?.projectTypes?.join(" / ") || "先从首页硬件项目卡启动"}</small>
            </div>
            <div className="hardware-workflow__lane hardware-workflow__lane--active">
              <span className="hardware-workflow__lane-label">设备实验台</span>
              <strong>{connected ? "硬件助手已启用" : hardwareMCP?.available ? "可启用硬件助手" : "未安装硬件助手"}</strong>
              <small>{detectedPorts.length ? `${detectedPorts.length} 个串口可选` : "未检测到串口设备"}</small>
            </div>
            <div className={`hardware-workflow__lane hardware-workflow__lane--${evidenceTone}`}>
              <span className="hardware-workflow__lane-label">证据账本</span>
              <strong>{evidenceStatusText(evidence?.status)}</strong>
              <small>{evidence ? `${evidence.currentRecordCount ?? 0} 条有效记录` : "尚未读取证据状态"}</small>
            </div>
          </div>
        </section>

        {/* 项目路径条:有项目时一行精简显示;无项目时给醒目空状态指引学生 */}
        {detect?.projectDir ? (
          <div className="hardware-view__project-bar">
            <Cpu size={14} />
            <code title={detect.projectDir}>{detect.projectDir}</code>
            <span className="hardware-view__project-meta">
              {detect.projectTypes?.length ? detect.projectTypes.join(" / ") : "未识别项目类型"}
            </span>
            <div className="hardware-view__toolchains-wrap">
              <button
                className="hardware-view__toolchains-btn"
                onClick={() => setToolchainsOpen((v) => !v)}
                title="点击查看具体哪些开发工具已就绪"
              >
                工具就绪 {availableToolchains}/{detect.toolchains.length}
                <ChevronDown size={11} />
              </button>
              {toolchainsOpen && (
                <div className="hardware-view__toolchains-menu">
                  <div className="hardware-view__toolchains-head">本机开发工具（✅ 已装 / ⚠️ 未装）</div>
                  <button
                    className="hardware-view__toolchain-autoinstall"
                    disabled={installing}
                    onClick={() => {
                      setToolchainsOpen(false); // 收起下拉,进度显示在下方醒目卡片里
                      void installToolchain();
                    }}
                    title="自动下载 arduino-cli 并补齐 Arduino/ESP32 板卡 core（装到用户目录，免管理员、免 Python）。进度在下方卡片实时显示。"
                  >
                    {installing ? <Loader2 size={13} className="hardware-spin" /> : <Download size={13} />}
                    {installing ? "安装中…" : "一键安装 Arduino/ESP32 工具链"}
                  </button>
                  {detect.toolchains.map((tool) => (
                    <div className="hardware-view__toolchain-row" key={tool.name}>
                      {tool.available ? (
                        <CheckCircle2 size={13} className="hardware-view__toolchain-ico--ok" />
                      ) : (
                        <AlertTriangle size={13} className="hardware-view__toolchain-ico--warn" />
                      )}
                      <span className="hardware-view__toolchain-name">{tool.name}</span>
                      {tool.available ? (
                        <span className="hardware-view__toolchain-note">{tool.version || "已安装"}</span>
                      ) : (
                        <button
                          className="hardware-view__toolchain-install"
                          onClick={() => openExternal(toolInstallUrl(tool.name))}
                          title={tool.hint || `查看 ${tool.name} 的安装方法`}
                        >
                          安装
                        </button>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
            {installSteps && (
              <div className={`hardware-view__install-pill hardware-view__install-pill--${installTone}`} title={installRunningStep?.message || installSummary}>
                {installing ? (
                  <Loader2 size={12} className="hardware-spin" />
                ) : installAllDone ? (
                  <CheckCircle2 size={12} />
                ) : (
                  <AlertTriangle size={12} />
                )}
                <span>{installSummary}</span>
              </div>
            )}
            {onOpenWorkspace && (
              <button className="chip" onClick={() => openWorkspace()} title="在文件面板打开">
                打开项目文件
              </button>
            )}
          </div>
        ) : (
          <div className="hardware-view__empty">
            <div className="hardware-view__empty-title">当前工作区还不是硬件项目</div>
            <p>先回到项目线程确认目标、板卡和工程目录；打开包含 <code>platformio.ini</code> 、<code>.ino</code> 或 <code>hardware_manifest.json</code> 的目录后，实验台才会启用编译/烧录。</p>
            {onOpenWorkspace && (
              <button className="btn btn--primary" onClick={() => openWorkspace()}>
                选择项目目录
              </button>
            )}
          </div>
        )}

        {/* 一键安装的实时进度卡片:紧跟项目条显示,点击后第一眼就能看到正在做哪一步 */}
        {installSteps && (
          <div
            className={`hardware-view__install-card hardware-view__install-card--${installTone}`}
          >
            <div className="hardware-view__install-card-head">
              {installing ? (
                <Loader2 size={15} className="hardware-spin" />
              ) : installAllDone ? (
                <CheckCircle2 size={15} />
              ) : (
                <AlertTriangle size={15} />
              )}
              <span className="hardware-view__install-card-title">
                {installing
                  ? `正在安装工具链…${installRunningStep?.label ?? "准备中"} · 已用 ${formatDuration(installElapsed)}`
                  : installAllDone
                    ? "工具链已就绪 ✅ 现在可以直接编译 / 烧录"
                    : "部分项未完成，看下方每步说明"}
              </span>
              {!installing && (
                <button
                  className="hardware-view__install-card-close"
                  onClick={() => setInstallSteps(null)}
                  title="关闭"
                >
                  ×
                </button>
              )}
            </div>
            <div className="hardware-view__install-steps">
              {installSteps.map((s) => (
                <div key={s.tool} className={`hardware-view__install-step hardware-view__install-step--${s.status}`}>
                  <span className="hardware-view__install-step-ico">
                    {s.status === "running" ? (
                      <Loader2 size={13} className="hardware-spin" />
                    ) : s.status === "done" ? (
                      <CheckCircle2 size={13} className="hardware-view__toolchain-ico--ok" />
                    ) : s.status === "failed" ? (
                      <AlertTriangle size={13} className="hardware-view__toolchain-ico--warn" />
                    ) : (
                      <span className="hardware-view__install-step-dot" />
                    )}
                  </span>
                  <span className="hardware-view__install-step-label">{s.label}</span>
                  <span className="hardware-view__install-step-msg">
                    {s.status === "running" ? s.message || "正在处理…" : s.status === "pending" ? s.message || "等待中" : s.message || ""}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}

        {/* 一键运行的结果显示(按钮在顶部工具栏触发) */}
        {hasHardwareProject && hardwareMCP?.available && (["validate", "upload", "monitor"] as const).map((kind) => {
          const result = runResults[kind];
          if (!result) return null;
          const label = { validate: "编译", upload: "烧录", monitor: "串口" }[kind];
          // py_compile 只查语法，不是真编译：不给绿勾，降级成黄色「语法通过（未验证）」，
          // 否则学生会把幻觉出来、能过语法的国产 Python 代码误当已验证。
          const syntaxOnly = result.kind === "python_syntax" && result.status === "passed";
          const tone = syntaxOnly ? "warn" : result.status === "passed" ? "ok" : result.status === "skipped" ? "warn" : "fail";
          const count = kind === "monitor" ? 0 : failCount[kind];
          const escalated = count >= FAIL_ESCALATE_AT;
          // 从命令尾部抽出正在编/烧的 sketch 目录名(命令以 sketch 路径结尾),让"编译通过"
          // 能看出是哪个项目通过的——多子项目父目录下尤其重要。按 "/" 切,含空格的中文路径也稳。
          const sketchName = result.command
            ? result.command.trim().replace(/["']+$/, "").split("/").filter(Boolean).pop()
            : "";
          return (
            <div key={kind} className={`hardware-quickrun__result hardware-quickrun__result--${tone}`}>
              <div className="hardware-quickrun__head">
                {result.status === "passed" && !syntaxOnly ? <CheckCircle2 size={14} /> : <AlertTriangle size={14} />}
                <strong>{label} · {syntaxOnly ? "语法通过（未验证 API/真机）" : result.status}</strong>
                {sketchName && <span className="hardware-quickrun__sketch" title="本次编译/烧录的项目目录">{sketchName}</span>}
                {count >= 2 && <span className="hardware-quickrun__fails">连续失败 {count} 次</span>}
                {result.summary && <span className="hardware-quickrun__summary">{result.summary}</span>}
              </div>
              {escalated && (
                <div className="hardware-quickrun__escalate">
                  ⚠️ 同一个问题改了 {count} 次还没过。别再小修小补——先检查接线/电平（5V↔3.3V 是否要转换）、限流电阻、共地 GND、供电，或换个思路。点下面让 AI 从整体排查。
                </div>
              )}
              {result.rootCause && (
                <div className="hardware-quickrun__row">
                  <span className="hardware-quickrun__tag">根因</span>
                  <code>{result.rootCause}</code>
                </div>
              )}
              {result.fixHint && (
                <div className="hardware-quickrun__row">
                  <span className="hardware-quickrun__tag hardware-quickrun__tag--fix">修法</span>
                  <span>{result.fixHint}</span>
                </div>
              )}
              {result.nextStep && !result.fixHint && (
                <div className="hardware-quickrun__row">
                  <span className="hardware-quickrun__tag">下一步</span>
                  <span>{result.nextStep}</span>
                </div>
              )}
              {result.error && !result.rootCause && (
                <div className="hardware-quickrun__row">
                  <span className="hardware-quickrun__tag hardware-quickrun__tag--fail">错误</span>
                  <code>{result.error}</code>
                </div>
              )}
              {result.output && result.status !== "passed" && (
                <details className="hardware-quickrun__details">
                  <summary>查看输出</summary>
                  <pre>{result.output}</pre>
                </details>
              )}
              {result.status === "failed" && (
                <button className="hardware-quickrun__askai" onClick={() => askAIToFix(kind, result)}>
                  <Cpu size={12} /> {escalated ? `换思路排查${label}（已失败${count}次）` : `让 AI 排查${label}失败`}
                </button>
              )}
            </div>
          );
        })}

        {/* 验证证据(精简,只在有数据时显示) */}
        {evidence && evidence.status !== "unavailable" && (evidence.summary || evidence.error || evidence.currentRecordCount || evidence.staleRecordCount) ? (
          <section className={`hardware-evidence hardware-evidence--${evidenceTone}`}>
            <div>
              <div className="hardware-evidence__label">验证记录</div>
              <strong>{evidenceStatusText(evidence?.status)}</strong>
              <p>{evidence?.error ? evidence.error : evidence ? evidenceSummaryText(evidence) : ""}</p>
            </div>
            <div className="hardware-evidence__stats">
              <span>
                <strong>{evidence?.currentRecordCount ?? 0}</strong>
                <small>有效记录</small>
              </span>
              <span>
                <strong>{evidence?.staleRecordCount ?? 0}</strong>
                <small>已过期</small>
              </span>
            </div>
            {evidence?.missingGroups.length ? <div className="hardware-evidence__missing">缺:{evidence.missingGroups.join(" / ")}</div> : null}
            {(evidence?.currentRecordCount ?? 0) > 0 ? (
              <div className="hardware-evidence__export">
                <button type="button" className="hardware-evidence__export-btn" onClick={() => void exportEvidence()}>
                  <Copy size={12} /> 导出验证证据
                </button>
                {exportMsg ? <span className="hardware-evidence__export-msg">{exportMsg}</span> : null}
              </div>
            ) : null}
          </section>
        ) : null}

        {/* 项目线程入口:学生描述需求,AI 在对话线程里推进方案、代码与验证 */}
        <section className="hardware-section">
          <div className="hardware-section__title">回到项目线程</div>
          <textarea
            className="hardware-view__task"
            value={task}
            onChange={(event) => setTask(event.target.value)}
            placeholder="例如:用 ESP32 读取超声波传感器,距离小于 20cm 时蜂鸣器报警,并通过串口输出距离。"
            rows={3}
          />
          {/* 两种写码方式,用户按项目复杂度自己选 */}
          <div className="hardware-view__task-actions">
            <button
              className="btn btn--primary"
              disabled={busy || !hardwareMCP?.available || !task.trim()}
              onClick={() => void runDirectCode()}
              title="简单项目:跳过方案确认,直接写代码并编译验证(仍会先读板卡/模块/知识库防幻觉)"
            >
              直接生成并验证
            </button>
            <button
              className="btn"
              disabled={busy || !hardwareMCP?.available || !task.trim()}
              onClick={() => void runGuidedPlan()}
              title="复杂项目:先生成接线图、引脚说明、程序逻辑,确认后再写代码"
            >
              先确认方案
            </button>
            {selectedKnowledgeCount > 0 && (
              <small className="hardware-view__task-hint">已选 {selectedKnowledgeCount} 个知识库参考资料</small>
            )}
          </div>
        </section>

        {/* AI 接管:调试 / 自动验证 / 代码审查都回到项目线程执行 */}
        <section className="hardware-section">
          <div className="hardware-section__title">交给项目线程</div>
          <div className="hardware-actions">
            {actions.map((action) => (
              <button
                key={action.title}
                className="hardware-action"
                disabled={busy || !hardwareMCP?.available}
                onClick={() => void runAction(action)}
              >
                <Cpu size={16} />
                <span>
                  <strong>{action.title}</strong>
                  <small>{action.subtitle}</small>
                </span>
              </button>
            ))}
          </div>
        </section>

        {/* 远程烧录(OTA):新建项目脚手架 / WiFi 烧录 / 发布到 NAS */}
        <OTAPanel
          projectDir={detect?.projectDir ?? ""}
          board={resolvedBoard}
          mcpReady={!!hardwareMCP?.available}
          onOpenWorkspace={onOpenWorkspace}
        />

        {/* 本机检测详情(默认折叠,需要时打开) */}
        <details className="hardware-section hardware-view__detail">
          <summary>本机检测详情(工具链 / 设备 / 串口 / 建议)</summary>
          <div className="hardware-summary-grid">
            <div>
              <strong>{detect?.projectTypes?.join(" / ") || "unknown"}</strong>
              <small>项目类型</small>
            </div>
            <div>
              <strong>{detect?.boards?.length || detect?.devices?.length || detect?.serialPorts?.length || 0}</strong>
              <small>设备</small>
            </div>
            <div>
              <strong>
                {availableToolchains}/{detect?.toolchains.length ?? 0}
              </strong>
              <small>工具链</small>
            </div>
          </div>

          {detect?.boards.length ? (
            <div className="hardware-devices">
              {detect.boards.map((b) => (
                <div className="hardware-device" key={`${b.port}-${b.fqbn || b.boardName}`}>
                  <strong>{b.boardName || "Unknown board"}</strong>
                  <small>{b.fqbn || b.core || b.protocol || b.port}</small>
                  <code>{b.port}</code>
                </div>
              ))}
            </div>
          ) : detect?.devices.length ? (
            <div className="hardware-devices">
              {detect.devices.map((device) => (
                <div className="hardware-device" key={device.port}>
                  <strong>{device.description || "Serial device"}</strong>
                  <small>{device.hwid || "PlatformIO device"}</small>
                  <code>{device.port}</code>
                </div>
              ))}
            </div>
          ) : null}

          <div className="hardware-toolchains">
            {detect?.toolchains.map((tool) => (
              <div className="hardware-toolchain" key={tool.name}>
                <span className={tool.available ? "hardware-dot hardware-dot--ok" : "hardware-dot hardware-dot--warn"}>
                  {tool.available ? <CheckCircle2 size={14} /> : <AlertTriangle size={14} />}
                </span>
                <span>
                  <strong>{tool.name}</strong>
                  {tool.available && tool.version ? <small>{tool.version}</small> : <small>{tool.hint}</small>}
                </span>
              </div>
            ))}
          </div>

          {detect?.serialPorts.length ? (
            <div className="hardware-ports">
              {detect.serialPorts.slice(0, 8).map((p) => (
                <span key={p}>
                  <Usb size={12} />
                  {p}
                </span>
              ))}
            </div>
          ) : null}

          {detect?.recommendations.length ? (
            <div className="hardware-recommendations">
              {detect.recommendations.map((item) => (
                <p key={item}>{item}</p>
              ))}
            </div>
          ) : null}
        </details>
      </div>

      {serialMonitorOpen && <SerialMonitor initialPort={port} onClose={() => setSerialMonitorOpen(false)} />}
    </div>
  );
}
