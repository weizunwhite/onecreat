import { FileText } from "lucide-react";
import { basename, fileRemarkFor, type FileReference } from "../lib/fileRemarks";
import { useT } from "../lib/i18n";

function dirname(path: string): string {
  const clean = path.replace(/\/$/, "");
  const parts = clean.split("/").filter(Boolean);
  return parts.length > 1 ? parts.slice(0, -1).join("/") : "";
}

export function FileReferenceStrip({
  files,
  label,
  variant = "default",
  onOpenFile,
}: {
  files: FileReference[];
  label?: string;
  variant?: "default" | "user" | "tool" | "approval" | "todo" | "session" | "context" | "hardware" | "knowledge" | "knowledge-match";
  onOpenFile?: (path: string) => void;
}) {
  const t = useT();
  if (files.length === 0) return null;

  return (
    <div className={`file-strip file-strip--${variant}`} aria-label={label ?? t("files.referenced")}>
      {label && (
        <div className="file-strip__head">
          <FileText size={12} />
          <span>{label}</span>
          <small>{files.length}</small>
        </div>
      )}
      <div className="file-strip__list">
        {files.map((file) => {
          const dir = dirname(file.path);
          const openPath = file.openPath ?? file.path;
          const clickable = Boolean(onOpenFile && openPath);
          const dirRemark = dir ? fileRemarkFor(`${dir}/`, true) : "";
          const content = (
            <>
              <FileText size={13} />
              <span className="file-strip__text">
                <strong>{basename(file.path)}</strong>
                <small>{file.remark}</small>
                {dir && <code>{dir}</code>}
                {dirRemark && <small className="file-strip__dir-remark">{dirRemark}</small>}
                {file.detail && <em>{file.detail}</em>}
              </span>
              {file.badge && <span className="file-strip__badge">{file.badge}</span>}
            </>
          );
          if (!clickable) {
            return (
              <div className="file-strip__item file-strip__item--static" key={file.path} title={`${file.path} · ${file.remark}`}>
                {content}
              </div>
            );
          }
          return (
            <button
              type="button"
              className="file-strip__item"
              key={file.path}
              title={`${file.path} · ${file.remark}`}
              aria-label={t("workspace.openFile", { path: openPath })}
              onClick={() => onOpenFile?.(openPath)}
            >
              {content}
            </button>
          );
        })}
      </div>
    </div>
  );
}
