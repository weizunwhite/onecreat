import { useState } from "react";
import { Check, ChevronsUpDown } from "lucide-react";
import type { EffortInfo } from "../lib/types";

// 推理强度档位的中文名(原始值 auto/high/max 仍原样发后端,这里只管显示)。
const EFFORT_LABELS: Record<string, string> = { auto: "自动", high: "高", max: "最高" };
const effortLabel = (level: string) => EFFORT_LABELS[level] ?? level;

export function EffortSwitcher({
  effort,
  disabled,
  onPick,
}: {
  effort?: EffortInfo;
  disabled: boolean;
  onPick: (level: string) => void;
}) {
  const [open, setOpen] = useState(false);
  if (!effort?.supported || effort.levels.length === 0) return null;

  const current = effort.current || "auto";
  const title = "AI 推理强度:越高思考越深、越慢;自动 = 由 AI 按问题决定";
  const pick = (level: string) => {
    setOpen(false);
    if (level !== current) onPick(level);
  };

  return (
    <div className="modelsw effortsw">
      <button
        className={`modelsw__trigger effortsw__trigger ${current !== "auto" ? "effortsw__trigger--explicit" : ""}`}
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        title={title}
      >
        <span className="modelsw__label">推理强度·{effortLabel(current)}</span>
        <ChevronsUpDown size={11} />
      </button>
      {open && !disabled && (
        <>
          <div className="modelsw__backdrop" onClick={() => setOpen(false)} />
          <div className="modelsw__menu effortsw__menu" role="listbox">
            {effort.levels.map((level) => (
              <button
                key={level}
                role="option"
                aria-selected={level === current}
                className={`modelsw__item ${level === current ? "modelsw__item--current" : ""}`}
                onClick={() => pick(level)}
              >
                <span className="modelsw__model">{effortLabel(level)}</span>
                {level === current && <Check size={13} className="modelsw__check" />}
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
