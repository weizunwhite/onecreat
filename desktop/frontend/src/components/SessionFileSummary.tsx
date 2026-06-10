import { FileText, MessageSquare } from "lucide-react";
import { basename, type FileReference } from "../lib/fileRemarks";

function dirname(path: string): string {
  const clean = path.replace(/\/$/, "");
  const parts = clean.split("/").filter(Boolean);
  return parts.length > 1 ? parts.slice(0, -1).join("/") : "";
}

export function SessionFileSummary({
  files,
  label,
  compact = false,
  onOpenFile,
}: {
  files: FileReference[];
  label: string;
  compact?: boolean;
  onOpenFile?: (path: string) => void;
}) {
  if (files.length === 0) return null;
  const primary = files[0];
  const dir = dirname(primary.path);
  const openPath = primary.openPath ?? primary.path;
  const clickable = Boolean(onOpenFile && openPath);
  const content = (
    <>
      <FileText size={13} />
      <span className="session-file-summary__body">
        <span className="session-file-summary__label">
          {label}
          <small>{files.length}</small>
        </span>
        <strong>{basename(primary.path)}</strong>
        <span className="session-file-summary__meta">
          {primary.remark}
          {dir && <code>{dir}</code>}
        </span>
      </span>
    </>
  );

  if (!clickable) {
    return (
      <div className={`session-file-summary${compact ? " session-file-summary--compact" : ""}`} title={`${primary.path} · ${primary.remark}`}>
        {content}
      </div>
    );
  }

  return (
    <button
      type="button"
      className={`session-file-summary${compact ? " session-file-summary--compact" : ""}`}
      title={`${primary.path} · ${primary.remark}`}
      onClick={() => onOpenFile?.(openPath)}
    >
      {content}
    </button>
  );
}

export function SessionPathSummary({
  path,
  title,
  meta,
  current,
  label,
  currentLabel,
  compact = false,
}: {
  path: string;
  title: string;
  meta: string;
  current?: boolean;
  label: string;
  currentLabel: string;
  compact?: boolean;
}) {
  const dir = dirname(path);
  return (
    <span className={`session-path-summary${compact ? " session-path-summary--compact" : ""}`}>
      <MessageSquare size={compact ? 12 : 14} />
      <span className="session-path-summary__body">
        <span className="session-path-summary__label">
          {label}
          {current && <small>{currentLabel}</small>}
        </span>
        <span className="session-path-summary__line">
          <strong>{basename(path) || title}</strong>
          <small>{title}</small>
        </span>
        <span className="session-path-summary__meta">
          {meta}
          {dir && <code>{dir}</code>}
        </span>
      </span>
    </span>
  );
}
