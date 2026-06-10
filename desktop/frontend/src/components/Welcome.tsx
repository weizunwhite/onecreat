import { Cpu, PencilRuler, FileText, GraduationCap, BookMarked, ClipboardList, Award, type LucideIcon } from "lucide-react";
import logo from "../assets/onecreat-logo.png";
import { useT } from "../lib/i18n";

// Welcome 是空状态首页：从「硬件优先」翻成「任务/技能启动台」。
// 每张卡 = 一个教培垂直：硬件项目直接开工作台；其余发起对应 skill 的起手 prompt
// （文案里带 skill 触发词，引擎会自动浮现对应技能）。下方输入框仍是通用对话入口。

type Vertical = {
  key: string;
  icon: LucideIcon;
  title: string;
  desc: string;
  // hardware 卡打开硬件工作台；其余卡发一句起手 prompt 进对话。
  prompt?: string;
  openHardware?: boolean;
};

const VERTICALS: Vertical[] = [
  {
    key: "hardware",
    icon: Cpu,
    title: "硬件项目",
    desc: "选板卡、编译、烧录、看串口 — 打开硬件工作台",
    openHardware: true,
  },
  {
    key: "proposal",
    icon: PencilRuler,
    title: "技术方案",
    desc: "硬件选型 / 系统架构 / 软件流程",
    prompt: "我要为一个科技创新项目写技术方案（硬件选型、系统架构、软件流程）。请帮我开始，先了解我的项目想法、目标和涉及的硬件。",
  },
  {
    key: "paper",
    icon: FileText,
    title: "竞赛论文",
    desc: "生成青创赛研究报告 / 论文",
    prompt: "我要写一篇青少年科技创新竞赛的研究报告（论文）。请帮我开始，先了解我的项目背景、方法和数据。",
  },
  {
    key: "lesson",
    icon: GraduationCap,
    title: "课程教案",
    desc: "科创项目课程的教案",
    prompt: "我要为一个科技创新项目课程写教案。请帮我开始，先了解课程主题、面向年级和课时安排。",
  },
  {
    key: "tutorial",
    icon: BookMarked,
    title: "教师辅导手册",
    desc: "老师上课用的项目辅导手册",
    prompt: "我要为一个科技创新项目写教师上课辅导手册（这个需要先有技术方案作为输入）。请帮我开始，先了解项目和已有的技术方案。",
  },
  {
    key: "log",
    icon: ClipboardList,
    title: "研究日志",
    desc: "按时间线整理项目研究日志",
    prompt: "我要整理一份项目研究日志。请帮我开始，先了解项目的研究过程和关键时间节点。",
  },
  {
    key: "jinpeng",
    icon: Award,
    title: "金鹏材料",
    desc: "金鹏论坛参赛包（含匿名化）",
    prompt: "我要准备北京金鹏科技论坛的参赛材料，注意全程匿名化要求。请帮我开始，先了解项目和需要哪些材料。",
  },
];

export function Welcome({
  onPrompt,
  onOpenHardware,
}: {
  onPrompt: (text: string) => void;
  onOpenHardware?: () => void;
}) {
  const t = useT();

  const launch = (v: Vertical) => {
    if (v.openHardware) onOpenHardware?.();
    else if (v.prompt) onPrompt(v.prompt);
  };

  return (
    <div className="welcome">
      <img src={logo} className="welcome__logo" alt="onecreat" />
      <div className="welcome__title">onecreat</div>
      <div className="welcome__tag">{t("welcome.tagline")}</div>

      {/* 垂直启动台：点一张卡开始一个任务 */}
      <div className="welcome__verticals">
        {VERTICALS.map((v) => {
          const Icon = v.icon;
          return (
            <button key={v.key} className="welcome__vertical" onClick={() => launch(v)} title={v.desc}>
              <Icon size={20} className="welcome__vertical-icon" />
              <span className="welcome__vertical-title">{v.title}</span>
              <span className="welcome__vertical-desc">{v.desc}</span>
            </button>
          );
        })}
      </div>

      <div className="welcome__hints">
        <span>
          <kbd>/</kbd> {t("welcome.hintCommands")}
        </span>
        <span>
          <kbd>@</kbd> {t("welcome.hintFiles")}
        </span>
        <span>
          <kbd>⏎</kbd> {t("welcome.hintSend")}
        </span>
      </div>
    </div>
  );
}
