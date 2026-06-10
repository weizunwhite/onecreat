import { useEffect, useRef } from "react";
import { FileTerminal } from "lucide-react";
import { useT } from "../lib/i18n";
import type { CommandInfo } from "../lib/types";

// SlashMenu is the "/" autocomplete dropdown above the composer. Presentational:
// the Composer owns filtering, the active index, and key handling; this renders
// the list and reports hover/pick. Uses mousedown (not click) so picking an item
// doesn't blur the textarea first.
export function SlashMenu({
  items,
  activeIndex,
  onPick,
  onHover,
}: {
  items: CommandInfo[];
  activeIndex: number;
  onPick: (c: CommandInfo) => void;
  onHover: (i: number) => void;
}) {
  const t = useT();
  // Keep the keyboard-selected item scrolled into view: the list is capped at
  // 280px and overflows, so ArrowDown past the visible window would otherwise
  // hide the active row. block:"nearest" only scrolls when it's actually off-screen.
  const activeRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: "nearest" });
  }, [activeIndex]);
  // builtin commands get no tag; custom (project) and mcp commands are labelled.
  const kindTag = (kind: CommandInfo["kind"]) =>
    kind === "custom"
      ? t("slash.project")
      : kind === "mcp"
        ? t("slash.mcp")
        : kind === "skill"
          ? t("slash.skill")
          : "";
  const commandFileName = (name: string) => `command_${name.replace(/[^A-Za-z0-9_-]+/g, "_") || "run"}.md`;

  return (
    <div className="slashmenu" role="listbox">
      {items.map((c, i) => (
        <button
          key={c.kind + ":" + c.name}
          ref={i === activeIndex ? activeRef : undefined}
          role="option"
          aria-selected={i === activeIndex}
          className={`slashmenu__item ${i === activeIndex ? "slashmenu__item--active" : ""}`}
          onMouseDown={(e) => {
            e.preventDefault();
            onPick(c);
          }}
          onMouseMove={() => onHover(i)}
        >
          <FileTerminal size={13} className="slashmenu__command-icon" />
          <span className="slashmenu__command">
            <span className="slashmenu__command-head">
              <span className="slashmenu__command-label">{t("slash.commandFile")}</span>
              {kindTag(c.kind) && <span className="slashmenu__kind">{kindTag(c.kind)}</span>}
            </span>
            <span className="slashmenu__command-line">
              <strong>{commandFileName(c.name)}</strong>
              <small>{t("slash.commandRemark")}</small>
            </span>
            <span className="slashmenu__command-desc">
              <code>/{c.name}</code>
              {c.hint && <span className="slashmenu__hint">{c.hint}</span>}
              <span>{c.description}</span>
            </span>
          </span>
        </button>
      ))}
    </div>
  );
}
