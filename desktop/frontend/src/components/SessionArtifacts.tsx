import { useMemo, useState } from "react";
import { ChevronDown, ChevronUp, FolderOutput } from "lucide-react";
import type { Item } from "../lib/useController";
import { fileRemarkFor, type FileReference } from "../lib/fileRemarks";
import { FileReferenceStrip } from "./FileReferenceStrip";

// 「本次产出」:把会话里 write_file / edit_file / multi_edit 成功写过的文件
// 聚合成右下角一枚可展开的清单。竞赛论文/教案这类任务一次会生成多个文档,
// 老师不用再去对话文字里逐条找文件名——点条目直接在工作区面板打开。
const WRITER_TOOLS = new Set(["write_file", "edit_file", "multi_edit"]);

function pathFromArgs(args: string): string {
  try {
    const parsed = JSON.parse(args || "{}") as { path?: unknown };
    return typeof parsed.path === "string" ? parsed.path.trim().replace(/^\.\//, "") : "";
  } catch {
    return "";
  }
}

export function SessionArtifacts({
  items,
  onOpenFile,
}: {
  items: Item[];
  onOpenFile: (path: string) => void;
}) {
  const [open, setOpen] = useState(false);

  const files = useMemo<FileReference[]>(() => {
    const seen = new Set<string>();
    const out: FileReference[] = [];
    // 倒序遍历:最新写的文件排最前
    for (let i = items.length - 1; i >= 0; i--) {
      const it = items[i];
      if (it.kind !== "tool" || !WRITER_TOOLS.has(it.name) || it.status !== "done") continue;
      const path = pathFromArgs(it.args);
      if (!path || seen.has(path)) continue;
      seen.add(path);
      out.push({ path, remark: fileRemarkFor(path) });
    }
    return out;
  }, [items]);

  if (files.length === 0) return null;

  return (
    <div className="session-artifacts">
      {open && (
        <div className="session-artifacts__panel">
          <FileReferenceStrip files={files} variant="session" onOpenFile={onOpenFile} />
        </div>
      )}
      <button
        type="button"
        className="session-artifacts__pill"
        onClick={() => setOpen((o) => !o)}
        title="本次会话生成/修改过的文件"
      >
        <FolderOutput size={13} />
        <span>本次产出</span>
        <small>{files.length}</small>
        {open ? <ChevronDown size={12} /> : <ChevronUp size={12} />}
      </button>
    </div>
  );
}
