import { useCallback, useEffect, useMemo, useState } from "react";
import {
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
import type {
  CapabilitiesView,
  HardwareDetectView,
  HardwareInstallToolchainView,
  HardwareEvidenceStatusView,
  HardwareMCPView,
  HardwareRunResult,
  ServerView,
} from "../lib/types";

type HardwareAction = {
  title: string;
  subtitle: string;
  prompt: string;
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
    title: "硬件调试",
    subtitle: "编译、烧录、串口、SSH 运行日志",
    prompt:
      "请调试当前硬件项目。先调用 mcp__hardware__hardware_detect，再调用 mcp__hardware__hardware_board_profile 读取板卡 profile，并调用 mcp__hardware__hardware_repair_catalog 读取常见错误修复规则；然后调用 mcp__hardware__hardware_project_audit 检查 manifest、接线说明、验证文档、板卡 profile、失败模式、硬件检查清单和工程布局。若 audit 发现上下文文件缺失，先调用 mcp__hardware__hardware_project_context 补齐缺失文件，不覆盖客户已有内容，再重新 audit。尤其检查 PlatformIO 是否有 platformio.ini + src/main.cpp，根目录 .ino 不要当成 PlatformIO 入口。若 audit/validate 报 platformio_layout 或 project_layout 失败，先调用 mcp__hardware__hardware_project_repair repair=platformio_root_ino_to_src_main 做最小修复，再重新 audit 和 validate。然后调用 mcp__hardware__hardware_project_validate；如果涉及真实开发板，先调用 mcp__hardware__hardware_device_verify_plan 生成准确的烧录/部署/串口验证命令；每完成一段编译、烧录、串口、mpremote 或 SSH 验证，都调用 mcp__hardware__hardware_evidence_record 记录证据，并调用 mcp__hardware__hardware_evidence_status 汇总当前验证状态；根据结果区分项目上下文缺失、工程结构错误、编译错误、烧录错误、串口无输出、端口占用、库缺失、供电/接线问题，并给出下一条最小验证命令。",
  },
  {
    title: "自动验证",
    subtitle: "自动识别项目并编译或检查语法",
    prompt:
      "请自动验证当前硬件项目。依次调用 mcp__hardware__hardware_detect、mcp__hardware__hardware_board_profile、mcp__hardware__hardware_repair_catalog、mcp__hardware__hardware_project_audit 和 mcp__hardware__hardware_project_validate；如果审计发现缺少 hardware_manifest、docs/wiring.md、docs/verification.md、docs/board_profile.md、docs/failure_patterns.md 或 tests/hardware_checklist.md，先调用 mcp__hardware__hardware_project_context 补齐缺失上下文后重新审计；如果发现 PlatformIO/Arduino/ESP-IDF 工程布局错误，先按 repair catalog 做最小修复后重新验证；如果是 PlatformIO 根目录 .ino 问题，调用 mcp__hardware__hardware_project_repair repair=platformio_root_ino_to_src_main 自动迁移。验证完成后调用 mcp__hardware__hardware_evidence_record 写入 tests/hardware_evidence.jsonl 和 tests/hardware_checklist.md，再调用 mcp__hardware__hardware_device_verify_plan 生成真实板卡验证计划，最后调用 mcp__hardware__hardware_evidence_status 判断是 hardware_verified 还是 hardware_pending；如果审计或验证失败，读取相关文件做最小修复后重新验证；如果没有真实开发板，只报告项目上下文、编译/语法已验证和实机缺口，不要假装已烧录。",
  },
  {
    title: "代码审查",
    subtitle: "教学可解释性、引脚风险、通信协议",
    prompt:
      "请审查当前硬件项目代码。先调用 mcp__hardware__hardware_detect 确认平台，再调用 mcp__hardware__hardware_board_profile 和 mcp__hardware__hardware_repair_catalog 读取板卡约束与失败规则，再调用 mcp__hardware__hardware_project_audit 检查项目上下文和工程布局，并调用 mcp__hardware__hardware_device_verify_plan 检查真实板卡验证命令是否完整，再调用 mcp__hardware__hardware_evidence_status 汇总证据状态；重点检查中文注释、命名、魔数常量、学生能否逐行解释、PlatformIO/Arduino 入口文件是否真实参与编译、引脚冲突、电压/通信协议风险、上传和串口验证步骤是否完整，以及 tests/hardware_evidence.jsonl 是否记录了真实验证证据。",
  },
];

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
    "# onecreat 硬件编程流程",
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
    "# onecreat 硬件编程流程（直接写代码模式）",
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
  const [err, setErr] = useState<string | null>(null);
  // 项目条上「工具就绪 X/Y」点开后,列出具体每个工具是否就绪。
  const [toolchainsOpen, setToolchainsOpen] = useState(false);
  // 一键安装核心工具链(arduino-cli + 板卡 core)的状态。
  const [installing, setInstalling] = useState(false);
  const [installResult, setInstallResult] = useState<HardwareInstallToolchainView | null>(null);
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

  // 一键安装核心工具链:下载 arduino-cli 并补齐板卡 core(默认 avr + esp32),
  // 装完重新检测,工具链就绪状态会自动刷新。给学生/老师打包后缺工具时点一下即可。
  const installToolchain = useCallback(async () => {
    setInstalling(true);
    setInstallResult(null);
    try {
      const result = await app.HardwareInstallToolchain([]);
      setInstallResult(result);
      await reload(); // 刷新工具链 ✅/⚠️ 状态
    } catch (e) {
      setInstallResult({
        available: false,
        steps: [],
        allOK: false,
        error: String((e as Error)?.message ?? e),
      });
    } finally {
      setInstalling(false);
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
    evidence?.status === "hardware_verified" ? "ok" : evidence?.status === "no_evidence" || evidence?.status === "unavailable" ? "warn" : "pending";

  // 「一键运行」按钮:把学生最常用的三件事 — 编译/烧录/看串口 —
  // 直接绑到对应 MCP 工具,不必再绕一圈到对话里让 AI 决定调什么。
  const detectedPlatform = useMemo(() => detectPlatformFromTypes(detect?.projectTypes), [detect?.projectTypes]);
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

  // 失败时学生点「让 AI 排查」就把已蒸馏的根因+修法+输出摘要直接塞给 chat,
  // 比让 AI 重跑一遍 validate 高效得多。连续失败到阈值则升级为「换思路」整体排查,
  // 并总是要求 AI 改完后自动重跑 validate(闭环),不要等学生手动再点。
  const askAIToFix = useCallback(
    (kind: "validate" | "upload" | "monitor", result: HardwareRunResult) => {
      const label = { validate: "编译/验证", upload: "烧录", monitor: "串口" }[kind];
      const count = kind === "monitor" ? 0 : failCount[kind];
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
        const context = detect
          ? `\n\n当前硬件检测摘要：项目类型=${detect.projectTypes.join(" / ") || "unknown"}；工具链=${detect.toolchains
              .filter((tool) => tool.available)
              .map((tool) => tool.name)
              .join(", ") || "无"}；串口数量=${detect.serialPorts.length}；项目目录=${detect.projectDir || "unknown"}。`
          : "";
        const evidenceContext = evidence
          ? `\n当前验证证据状态：status=${evidence.status || "unknown"}；currentRecords=${evidence.currentRecordCount || 0}；missing=${evidence.missingGroups.join(" / ") || "无"}；summary=${evidence.summary || evidence.error || "无"}。`
          : "";
        onPrompt(action.title, action.prompt + context + evidenceContext);
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
      {/* IDE 风格顶部工具栏:板卡/串口/框架 + 一键编译烧录看串口 + MCP 徽章 + 返回对话 */}
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
            title="采样 8 秒串口"
          >
            {running.monitor ? <Loader2 size={14} className="hardware-spin" /> : <Eye size={14} />}
            <span>看串口</span>
          </button>
        </div>

        <div className="hardware-view__toolbar-group hardware-view__toolbar-group--right">
          <div className={`hardware-view__mcp hardware-view__mcp--${connected ? "ok" : hardwareMCP?.available ? "warn" : "off"}`}>
            <span className="hardware-view__mcp-dot" />
            <span className="hardware-view__mcp-label">
              {connected ? "助手已启用" : hardwareMCP?.available ? "未启用" : "未安装"}
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
            <button className="hardware-view__back" onClick={onBackToChat} title="返回对话视图">
              <ArrowLeft size={13} />
              <span>返回对话</span>
            </button>
          )}
        </div>
      </div>

      <div className="hardware-view__body">
        {err && <div className="banner banner--error">{err}</div>}
        {hardwareMCP?.error && !hardwareMCP.available && <div className="banner banner--error">{hardwareMCP.error}</div>}
        {detect?.error && <div className="banner banner--error">{detect.error}</div>}

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
                    onClick={() => void installToolchain()}
                    title="自动下载 arduino-cli 并补齐 Arduino/ESP32 板卡 core（装到用户目录，免管理员、免 Python）"
                  >
                    {installing ? <Loader2 size={13} className="hardware-spin" /> : <Download size={13} />}
                    {installing ? "安装中…首次下载 core 可能几分钟" : "一键安装 Arduino/ESP32 工具链"}
                  </button>
                  {installResult && (
                    <div className="hardware-view__install-result">
                      {installResult.error ? (
                        <div className="hardware-view__install-msg hardware-view__install-msg--err">{installResult.error}</div>
                      ) : (
                        installResult.steps.map((step) => (
                          <div
                            key={step.tool}
                            className={`hardware-view__install-msg hardware-view__install-msg--${step.ok ? "ok" : "err"}`}
                          >
                            {step.ok ? "✅" : "❌"} {step.tool} — {step.message}
                          </div>
                        ))
                      )}
                      {installResult.nextStep && <div className="hardware-view__install-next">{installResult.nextStep}</div>}
                    </div>
                  )}
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
            {onOpenWorkspace && (
              <button className="chip" onClick={() => openWorkspace()} title="在文件面板打开">
                打开项目文件
              </button>
            )}
          </div>
        ) : (
          <div className="hardware-view__empty">
            <div className="hardware-view__empty-title">当前工作区不是硬件项目</div>
            <p>请打开包含 <code>platformio.ini</code> 、<code>.ino</code> 或 <code>hardware_manifest.json</code> 的目录。AI 会自动识别并启用一键编译/烧录。</p>
            {onOpenWorkspace && (
              <button className="btn btn--primary" onClick={() => openWorkspace()}>
                选择项目目录
              </button>
            )}
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
          return (
            <div key={kind} className={`hardware-quickrun__result hardware-quickrun__result--${tone}`}>
              <div className="hardware-quickrun__head">
                {result.status === "passed" && !syntaxOnly ? <CheckCircle2 size={14} /> : <AlertTriangle size={14} />}
                <strong>{label} · {syntaxOnly ? "语法通过（未验证 API/真机）" : result.status}</strong>
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

        {/* AI 写代码入口:学生描述需求,AI 给出方案 */}
        <section className="hardware-section">
          <div className="hardware-section__title">让 AI 写代码</div>
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
              直接写代码
            </button>
            <button
              className="btn"
              disabled={busy || !hardwareMCP?.available || !task.trim()}
              onClick={() => void runGuidedPlan()}
              title="复杂项目:先生成接线图、引脚说明、程序逻辑,确认后再写代码"
            >
              先出方案再写
            </button>
            {selectedKnowledgeCount > 0 && (
              <small className="hardware-view__task-hint">已选 {selectedKnowledgeCount} 个知识库参考资料</small>
            )}
          </div>
        </section>

        {/* AI 接管:调试 / 自动验证 / 代码审查 */}
        <section className="hardware-section">
          <div className="hardware-section__title">让 AI 接管</div>
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
    </div>
  );
}
