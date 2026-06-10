import type { ReactNode } from "react";
import { FileText, type LucideIcon } from "lucide-react";
import { basename } from "../lib/fileRemarks";

function dirname(path: string): string {
  const clean = path.replace(/[\\/]+$/, "");
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts.length > 1 ? parts.slice(0, -1).join("/") : "";
}

function classNames(values: Array<string | undefined | false>) {
  return values.filter(Boolean).join(" ");
}

export type FileCardProps = {
  path: string;
  label: string;
  remark: string;
  meta?: ReactNode;
  badge?: string;
  openPath?: string;
  onOpenFile?: (path: string) => void;
  icon?: LucideIcon;
  compact?: boolean;
  className?: string;
  line?: ReactNode;
};

export function FileCard({
  path,
  label,
  remark,
  meta,
  badge,
  openPath,
  onOpenFile,
  icon: Icon = FileText,
  compact = false,
  className,
  line,
}: FileCardProps) {
  const dir = dirname(path);
  const name = basename(path);
  const canOpen = Boolean(onOpenFile && openPath);
  const cardPath = openPath ?? path;
  const title = `${path} · ${remark}`;
  const content = (
    <>
      <Icon size={compact ? 12 : 14} aria-hidden="true" />
      <span className="file-card__body">
        <span className="file-card__label">
          {label}
          {badge ? <small>{badge}</small> : null}
        </span>
        <span className="file-card__line">
          <strong>{name}</strong>
          <small>{remark}</small>
          {line}
        </span>
        {(meta || dir) && (
          <span className="file-card__meta">
            {meta}
            {dir && <code>{dir}</code>}
          </span>
        )}
      </span>
    </>
  );

  if (!canOpen) {
    return (
      <div className={classNames(["file-card", compact ? "file-card--compact" : "", className])} title={title}>
        {content}
      </div>
    );
  }

  return (
    <button
      type="button"
      className={classNames(["file-card", compact ? "file-card--button" : "file-card--button", compact ? "file-card--compact" : "", className])}
      title={title}
      onClick={() => onOpenFile?.(cardPath)}
    >
      {content}
    </button>
  );
}

