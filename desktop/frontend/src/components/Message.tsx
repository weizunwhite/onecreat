import { memo, useState } from "react";
import { ChevronRight } from "lucide-react";
import { Markdown } from "./Markdown";
import { CopyButton } from "./CopyButton";
import { useT } from "../lib/i18n";
import { useDetailMode } from "../lib/detailMode";
import { streamingDisplayText } from "../lib/streamText";
import type { Item } from "../lib/useController";

type AssistantItem = Extract<Item, { kind: "assistant" }>;

export function UserMessage({
  text,
  turn,
  open,
  onToggle,
  onRewind,
}: {
  text: string;
  turn?: number;
  open?: boolean; // whether this message's rewind menu is the open one (lifted to Transcript)
  onToggle?: () => void;
  onRewind?: (turn: number, scope: string) => void;
  onOpenFile?: (path: string) => void;
}) {
  const t = useT();
  const canRewind = onRewind != null && turn != null;
  const rewind = (scope: string) => onRewind?.(turn as number, scope);
  const displayText = text.replace(/@\.(?:onecreat|reasonix)\/attachments\/[^\s]+/g, "[image]");
  return (
    <div className="msg msg--user">
      <span className="msg__caret">›</span>
      <div className="msg__userbody">
        <div className="msg__text">{displayText}</div>
        {/* 复制按钮:浅绿框右上角,悬停显示,方便复制自己问过的话 */}
        <CopyButton text={text} className="msg__usercopy" />
      </div>
      {canRewind && (
        <div className="rewind">
          <button className="rewind__btn" title={t("rewind.label")} onClick={onToggle}>
            ⟲
          </button>
          {open && (
            <div className="rewind__menu">
              <button onClick={() => rewind("both")}>{t("rewind.both")}</button>
              <button onClick={() => rewind("conversation")}>{t("rewind.conversation")}</button>
              <button onClick={() => rewind("code")}>{t("rewind.code")}</button>
              <button onClick={() => rewind("fork")}>{t("rewind.fork")}</button>
              <button onClick={() => rewind("summ-from")}>{t("rewind.summFrom")}</button>
              <button onClick={() => rewind("summ-upto")}>{t("rewind.summUpto")}</button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// memo: an unchanged message keeps a stable `item` ref across a streaming turn's
// per-token re-renders, so only the live bubble re-parses markdown, not the whole
// backlog.
export const AssistantMessage = memo(function AssistantMessage({
  item,
}: {
  item: AssistantItem;
  turn?: number;
  onOpenFile?: (path: string) => void;
}) {
  const t = useT();
  const detail = useDetailMode();
  const [open, setOpen] = useState(false);
  return (
    <div className="msg msg--assistant">
      {/* 思考过程只在「详细模式」显示;简洁模式下对学生/老师隐藏,减少噪音 */}
      {detail && item.reasoning && (
        <div className="reasoning">
          <button className="reasoning__toggle" onClick={() => setOpen((v) => !v)}>
            <ChevronRight
              className={`reasoning__chevron ${open ? "reasoning__chevron--open" : ""}`}
              size={12}
            />
            {t("msg.thinking")}
          </button>
          {open && <div className="reasoning__body">{item.reasoning}</div>}
        </div>
      )}
      <div className="msg__body">
        {item.streaming ? (
          // 流式时不做完整 Markdown 渲染(每 token 重解析+代码高亮+KaTeX 会卡顿抖动),
          // 而是用 streamingDisplayText 轻量「去标记」:把 ** # ` 围栏等符号抹掉显示成
          // 干净文字,代码内容保留。生成结束后由下面的 <Markdown> 一次性排成正式格式。
          <div className="msg__stream">
            {streamingDisplayText(item.text)}
            <span className="cursor" />
          </div>
        ) : (
          <Markdown text={item.text} />
        )}
      </div>
      {!item.streaming && item.text && (
        <div className="msg__actions">
          <CopyButton text={item.text} label={t("msg.copy")} />
        </div>
      )}
    </div>
  );
});
