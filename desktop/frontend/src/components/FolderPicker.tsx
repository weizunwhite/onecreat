import { useCallback, useEffect, useState } from "react";
import { ArrowUp, Folder, Home, Monitor, X } from "lucide-react";
import { app } from "../lib/bridge";
import type { FolderListing } from "../lib/types";
import { resolveFolderPicker, subscribeFolderPicker } from "../lib/folderPicker";

// app 内置文件夹选择器:在 app 里直接浏览目录、点进点出、选中即可,不用系统对话框
// (系统那个在隐藏标题栏窗口下会开到窗口后面,看着像没反应)。

function joinPath(base: string, name: string): string {
  if (!base) return name;
  return base.endsWith("/") ? base + name : base + "/" + name;
}

export function FolderPicker() {
  const [open, setOpen] = useState(false);
  const [listing, setListing] = useState<FolderListing | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async (path: string) => {
    setLoading(true);
    try {
      const l = await app.BrowseDir(path).catch(() => null);
      if (l) setListing(l);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    return subscribeFolderPicker((isOpen, startPath) => {
      setOpen(isOpen);
      if (isOpen) void load(startPath || "");
    });
  }, [load]);

  if (!open) return null;

  const cur = listing?.path ?? "";
  const cancel = () => resolveFolderPicker("");
  const pickCurrent = () => {
    if (cur) resolveFolderPicker(cur);
  };

  return (
    <div className="fp-backdrop" onClick={cancel}>
      <div className="fp" onClick={(e) => e.stopPropagation()}>
        <div className="fp__head">
          <span className="fp__title">选择文件夹</span>
          <div className="fp__spacer" />
          <button className="chip chip--icon" onClick={cancel} title="取消">
            <X size={14} />
          </button>
        </div>

        <div className="fp__bar">
          <button className="chip chip--sm" disabled={!listing} onClick={() => listing && void load(listing.parent)} title="上一级">
            <ArrowUp size={13} /> 上级
          </button>
          <button className="chip chip--sm" disabled={!listing} onClick={() => listing && void load(listing.home)} title="主目录">
            <Home size={13} /> 主目录
          </button>
          {listing?.desktop && (
            <button className="chip chip--sm" onClick={() => void load(listing.desktop)} title="桌面">
              <Monitor size={13} /> 桌面
            </button>
          )}
          <code className="fp__path" title={cur}>
            {cur || "…"}
          </code>
        </div>

        {listing?.error && <div className="fp__err">{listing.error}</div>}

        <div className="fp__list">
          {loading && <div className="fp__hint">读取中…</div>}
          {!loading && listing && listing.dirs.length === 0 && (
            <div className="fp__hint">这个文件夹里没有子文件夹——可以直接点下面「选择此文件夹」。</div>
          )}
          {!loading &&
            listing &&
            listing.dirs.map((d) => (
              <button key={d} className="fp__row" onClick={() => void load(joinPath(cur, d))} title={`进入 ${d}`}>
                <Folder size={14} />
                <span>{d}</span>
              </button>
            ))}
        </div>

        <div className="fp__foot">
          <span className="fp__pickhint">
            当前位置：<b>{cur || "…"}</b>
          </span>
          <div className="fp__spacer" />
          <button className="btn" onClick={cancel}>
            取消
          </button>
          <button className="btn btn--primary" disabled={!cur} onClick={pickCurrent}>
            选择此文件夹
          </button>
        </div>
      </div>
    </div>
  );
}
