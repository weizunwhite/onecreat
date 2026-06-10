const pathRemarks: Record<string, string> = {
  "platformio.ini": "PlatformIO 工程配置",
  "CMakeLists.txt": "ESP-IDF 构建入口",
  "cmakelists.txt": "ESP-IDF 构建入口",
  "sdkconfig": "ESP-IDF 配置",
  "sdkconfig.defaults": "ESP-IDF 默认配置",
  "partitions.csv": "ESP32 分区表",
  "requirements.txt": "Python 依赖清单",
  "reasonix.toml": "onecreat 配置文件",
  "AGENTS.md": "智能体协作指令",
  "agents.md": "智能体协作指令",
  "CLAUDE.md": "Claude Code 指令",
  "claude.md": "Claude Code 指令",
  "ONECREAT.md": "智能体指令文件",
  "ONECREAT.local.md": "个人指令(不入库)",
  "REASONIX.md": "智能体指令文件(旧名)",
  "reasonix.md": "智能体指令文件",
  "hardware_manifest.json": "硬件项目清单",
  "docs/wiring.md": "接线说明",
  "docs/verification.md": "验证流程",
  "docs/board_profile.md": "板卡约束",
  "docs/failure_patterns.md": "失败修复规则",
  "tests/hardware_checklist.md": "硬件检查清单",
  "tests/hardware_evidence.jsonl": "真实验证证据",
  "src/main.cpp": "主程序入口",
  "src/main.c": "主程序入口",
  "src/main.py": "主程序入口",
  "src/main_controller": "主控代码",
  "src/sensor_module": "传感器模块",
  "src/vision_module": "视觉模块",
  "include/index_html.h": "网页资源头文件",
  "readme.md": "项目说明",
  "README.md": "项目说明",
  "go.mod": "Go 模块配置",
  "package.json": "前端依赖配置",
  "tsconfig.json": "TypeScript 配置",
  "vite.config.ts": "Vite 构建配置",
};

const nameRemarks: Record<string, string> = {
  src: "源代码目录",
  include: "头文件和配置",
  lib: "外部库目录",
  docs: "项目文档",
  tests: "测试与验证记录",
  examples: "示例代码",
  components: "ESP-IDF 组件",
  firmware: "固件代码",
  hardware: "硬件资料",
  boards: "板卡配置",
  config: "配置文件",
  data: "运行数据",
  "3d_parts": "3D 打印文件",
  assets: "图片和静态资源",
  models: "AI 模型文件",
  knowledge: "本地知识库存储",
  sessions: "会话记录目录",
  memories: "记忆存储目录",
  memory: "记忆存储目录",
  skills: "Agent 技能目录",
  plugins: "插件目录",
  cache: "缓存目录",
  "openai-bundled": "内置插件资源",
  "openai-curated": "精选插件资源",
  scripts: "自动化脚本",
  main: "主程序",
  main_cpp: "主程序",
  desktop: "桌面端代码",
  frontend: "前端界面代码",
  internal: "核心内部逻辑",
  cmd: "命令行入口",
};

export function basename(path: string): string {
  const clean = path.replace(/[\\/]$/, "");
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] ?? clean;
}

export function fileRemarkFor(path: string, isDir = false): string {
  const clean = path.replace(/\\/g, "/").replace(/^\.\//, "").replace(/\/$/, "");
  const lower = clean.toLowerCase();
  const name = basename(lower);
  const exact = pathRemarks[clean] ?? pathRemarks[lower] ?? pathRemarks[name];
  if (exact) return exact;
  const suffix = Object.entries(pathRemarks).find(([key]) => lower.endsWith("/" + key.toLowerCase()));
  if (suffix) return suffix[1];
  if (isDir) return nameRemarks[name] ?? "";
  if (name.endsWith(".ino")) return "Arduino 草图";
  if (name.endsWith(".cpp")) return "C++ 源代码";
  if (name.endsWith(".c")) return "C 源代码";
  if (name.endsWith(".h") || name.endsWith(".hpp")) return "头文件";
  if (name.endsWith(".py")) return "Python 程序";
  if (name.endsWith(".go")) return "Go 源代码";
  if (name.endsWith(".ts") || name.endsWith(".tsx")) return "TypeScript 源代码";
  if (name.endsWith(".json")) return "结构化配置";
  if (name.endsWith(".jsonl")) return "会话记录文件";
  if (name.endsWith(".md")) return "说明文档";
  if (name.endsWith(".csv")) return "表格数据";
  if (name.endsWith(".mod")) return "模块配置";
  if (name.endsWith(".toml") || name.endsWith(".ini") || name.endsWith(".yaml") || name.endsWith(".yml")) return "配置文件";
  return "";
}

export function projectRemarkFor(pathOrName: string): string {
  const direct = fileRemarkFor(pathOrName, true);
  if (direct) return direct;

  const lower = pathOrName.toLowerCase();
  const name = basename(lower);
  if (name.includes("esp32")) return "ESP32 项目";
  if (name.includes("arduino")) return "Arduino 项目";
  if (name.includes("esp-idf") || name.includes("idf")) return "ESP-IDF 项目";
  if (name.includes("hardware") || lower.includes("/hardware/")) return "硬件项目";
  if (name.includes("reasonix") || name.includes("onecreat")) return "AI 工作台";
  return "项目文件夹";
}

export type FileReference = {
  path: string;
  remark: string;
  detail?: string;
  badge?: string;
  openPath?: string;
};

const pathLikePattern =
  /(?:^|[\s([{"'`])((?:\.{0,2}\/)?(?:[A-Za-z0-9_.-]+\/)+(?:[A-Za-z0-9_.-]+(?:\.[A-Za-z0-9_.-]+)?))|(?:^|[\s([{"'`])([A-Za-z0-9_.-]+\.(?:ino|cpp|c|h|hpp|py|go|mod|ts|tsx|js|jsx|json|md|toml|ini|csv|yaml|yml))/g;

export function extractFileReferences(text: string, limit = 8): FileReference[] {
  const seen = new Set<string>();
  const refs: FileReference[] = [];
  const searchable = text.replace(/(^|\s)@(?=[A-Za-z0-9_.-]+(?:\/|\.))/g, "$1");
  for (const match of searchable.matchAll(pathLikePattern)) {
    const raw = (match[1] || match[2] || "").replace(/[),.;:，。；：]+$/, "");
    if (!raw || raw.includes("://") || raw.length > 96) continue;
    const clean = raw.replace(/^\.\//, "");
    if (clean.startsWith(".reasonix/attachments/")) continue;
    if (seen.has(clean)) continue;
    seen.add(clean);
    refs.push({ path: clean, remark: fileRemarkFor(clean) || "项目文件" });
    if (refs.length >= limit) break;
  }
  return refs;
}
