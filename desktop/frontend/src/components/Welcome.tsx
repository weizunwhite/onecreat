import { Cpu, PencilRuler, FileText, GraduationCap, BookMarked, ClipboardList, Award, type LucideIcon } from "lucide-react";
import logo from "../assets/onecreat-logo.png";
import { useT } from "../lib/i18n";
import type { DictKey } from "../locales/en";

// Welcome 是空状态首页：从「硬件优先」翻成「任务/技能启动台」。
// 每张卡 = 一个教培垂直：硬件项目直接开工作台；其余发起对应 skill 的起手 prompt
// （文案里带 skill 触发词，引擎会自动浮现对应技能）。下方输入框仍是通用对话入口。
// 卡片标题/描述走 i18n;prompt 是发给模型的指令,保持中文(产品与技能均中文优先)。

type Vertical = {
  key: string;
  icon: LucideIcon;
  titleKey: DictKey;
  descKey: DictKey;
  // hardware 卡打开硬件工作台；其余卡发一句起手 prompt 进对话。
  prompt?: string;
  openHardware?: boolean;
};

const VERTICALS: Vertical[] = [
  {
    key: "hardware",
    icon: Cpu,
    titleKey: "welcome.v.hardware.title",
    descKey: "welcome.v.hardware.desc",
    openHardware: true,
  },
  {
    key: "proposal",
    icon: PencilRuler,
    titleKey: "welcome.v.proposal.title",
    descKey: "welcome.v.proposal.desc",
    prompt: "我要为一个科技创新项目写技术方案（硬件选型、系统架构、软件流程）。请帮我开始，先了解我的项目想法、目标和涉及的硬件。",
  },
  {
    key: "paper",
    icon: FileText,
    titleKey: "welcome.v.paper.title",
    descKey: "welcome.v.paper.desc",
    prompt: "我要写一篇青少年科技创新竞赛的研究报告（论文）。请帮我开始，先了解我的项目背景、方法和数据。",
  },
  {
    key: "lesson",
    icon: GraduationCap,
    titleKey: "welcome.v.lesson.title",
    descKey: "welcome.v.lesson.desc",
    prompt: "我要为一个科技创新项目课程写教案。请帮我开始，先了解课程主题、面向年级和课时安排。",
  },
  {
    key: "tutorial",
    icon: BookMarked,
    titleKey: "welcome.v.tutorial.title",
    descKey: "welcome.v.tutorial.desc",
    prompt: "我要为一个科技创新项目写教师上课辅导手册（这个需要先有技术方案作为输入）。请帮我开始，先了解项目和已有的技术方案。",
  },
  {
    key: "log",
    icon: ClipboardList,
    titleKey: "welcome.v.log.title",
    descKey: "welcome.v.log.desc",
    prompt: "我要整理一份项目研究日志。请帮我开始，先了解项目的研究过程和关键时间节点。",
  },
  {
    key: "jinpeng",
    icon: Award,
    titleKey: "welcome.v.jinpeng.title",
    descKey: "welcome.v.jinpeng.desc",
    prompt: "我要准备北京金鹏科技论坛的参赛材料，注意全程匿名化要求。请帮我开始，先了解项目和需要哪些材料。",
  },
];

// 产出归属约定:内容类垂直生成的文档统一进 产出/ 子目录,
// 不混进代码目录——老师在一个固定地方就能找到所有材料。
const OUTPUT_DIR_NOTE =
  "\n\n约定:本任务生成的所有文档(docx/pptx/xlsx/图表/Markdown 等)统一保存到当前项目的 产出/ 子目录(例如 产出/研究报告.docx),不要散落在项目根目录或代码目录里;如目录不存在请先创建。";

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
    else if (v.prompt) onPrompt(v.prompt + OUTPUT_DIR_NOTE);
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
            <button key={v.key} className="welcome__vertical" onClick={() => launch(v)} title={t(v.descKey)}>
              <Icon size={20} className="welcome__vertical-icon" />
              <span className="welcome__vertical-title">{t(v.titleKey)}</span>
              <span className="welcome__vertical-desc">{t(v.descKey)}</span>
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
