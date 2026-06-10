import { useCallback, useEffect, useMemo, useState } from "react";
import {
  BookOpen,
  CheckCircle2,
  Circle,
  Loader2,
  Plus,
  Search,
  Trash2,
  Upload,
} from "lucide-react";
import { app } from "../lib/bridge";
import type { FileReference } from "../lib/fileRemarks";
import type { KnowledgeImportResult, KnowledgeSearchResult, KnowledgeView } from "../lib/types";
import { FileReferenceStrip } from "./FileReferenceStrip";
import { ResizableDrawer } from "./ResizableDrawer";

function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size <= 0) return "0 B";
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

function knowledgeDocReference(doc: KnowledgeView["documents"][number]): FileReference {
  const path = doc.originalPath || doc.name;
  return {
    path,
    remark: "",
    detail: `${formatBytes(doc.size)} · ${doc.chunks} 片段`,
    badge: doc.status === "ready" ? "" : doc.status || "待处理",
    openPath: "",
  };
}

function knowledgeMatchReference(match: KnowledgeSearchResult["matches"][number], index: number): FileReference {
  return {
    path: match.documentName,
    remark: "",
    detail: `${match.baseName} · 片段 ${match.chunkIndex + 1}`,
    badge: String(index + 1),
    openPath: "",
  };
}

export function KnowledgePanel({
  onClose,
  selectedBaseIds,
  onSelectionChange,
}: {
  onClose: () => void;
  selectedBaseIds: string[];
  onSelectionChange: (ids: string[]) => void;
}) {
  const [view, setView] = useState<KnowledgeView | null>(null);
  const [activeBaseId, setActiveBaseId] = useState("");
  const [newName, setNewName] = useState("");
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState<KnowledgeSearchResult | null>(null);
  const [lastImport, setLastImport] = useState<KnowledgeImportResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [searching, setSearching] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const selectedSet = useMemo(() => new Set(selectedBaseIds), [selectedBaseIds]);
  const activeBase = view?.bases.find((base) => base.id === activeBaseId) ?? view?.bases[0] ?? null;
  const activeDocs = useMemo(
    () => (view && activeBase ? view.documents.filter((doc) => doc.baseId === activeBase.id) : []),
    [activeBase, view],
  );
  const activeDocReferences = useMemo(() => activeDocs.map(knowledgeDocReference), [activeDocs]);

  const loadView = useCallback(
    async (selectedOverride = selectedBaseIds) => {
      const next = await app.KnowledgeView();
      setView(next);
      setActiveBaseId((current) => {
        if (current && next.bases.some((base) => base.id === current)) return current;
        return next.bases[0]?.id ?? "";
      });
      const validSelected = selectedOverride.filter((id) => next.bases.some((base) => base.id === id));
      if (validSelected.length !== selectedOverride.length) {
        onSelectionChange(validSelected);
      }
    },
    [onSelectionChange, selectedBaseIds],
  );

  useEffect(() => {
    void loadView().catch((e) => setErr(String((e as Error)?.message ?? e)));
  }, [loadView]);

  useEffect(() => {
    const text = query.trim();
    if (!text) {
      setSearch(null);
      return;
    }
    const baseIDs = selectedBaseIds.length ? selectedBaseIds : activeBase ? [activeBase.id] : [];
    let stale = false;
    setSearching(true);
    app
      .KnowledgeSearch(baseIDs, text, 8)
      .then((result) => {
        if (!stale) setSearch(result);
      })
      .catch((e) => {
        if (!stale) setErr(String((e as Error)?.message ?? e));
      })
      .finally(() => {
        if (!stale) setSearching(false);
      });
    return () => {
      stale = true;
    };
  }, [activeBase, query, selectedBaseIds]);

  const createBase = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      const base = await app.KnowledgeCreate(newName);
      setNewName("");
      const nextSelected = [...selectedBaseIds, base.id];
      onSelectionChange(nextSelected);
      setActiveBaseId(base.id);
      await loadView(nextSelected);
    } catch (e) {
      setErr(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [loadView, newName, onSelectionChange, selectedBaseIds]);

  const deleteBase = useCallback(
    async (id: string) => {
      const base = view?.bases.find((item) => item.id === id);
      if (!base) return;
      if (!window.confirm(`删除知识库「${base.name}」？本地导入副本和索引会一起删除。`)) return;
      setBusy(true);
      setErr(null);
      try {
        await app.KnowledgeDelete(id);
        const nextSelected = selectedBaseIds.filter((item) => item !== id);
        onSelectionChange(nextSelected);
        await loadView(nextSelected);
      } catch (e) {
        setErr(String((e as Error)?.message ?? e));
      } finally {
        setBusy(false);
      }
    },
    [loadView, onSelectionChange, selectedBaseIds, view?.bases],
  );

  const importFiles = useCallback(async () => {
    const baseID = activeBase?.id;
    if (!baseID) {
      setErr("请先创建或选择一个知识库。");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const result = await app.KnowledgeImportFiles(baseID);
      setLastImport(result);
      await loadView();
    } catch (e) {
      setErr(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [activeBase?.id, loadView]);

  const toggleSelected = useCallback(
    (id: string) => {
      const next = selectedSet.has(id) ? selectedBaseIds.filter((item) => item !== id) : [...selectedBaseIds, id];
      onSelectionChange(next);
    },
    [onSelectionChange, selectedBaseIds, selectedSet],
  );

  const totalChunks = view?.bases.reduce((sum, base) => sum + base.chunks, 0) ?? 0;
  const supportedHint = view?.supportedExtensions.slice(0, 16).join(" ") || ".txt .md .py .ino .cpp";

  return (
    <ResizableDrawer onClose={onClose} wide subtle>
      <header className="drawer__head">
        <div className="drawer__title">知识库</div>
        <button className="chip" onClick={onClose} title="关闭">
          ✕
        </button>
      </header>

      <div className="drawer__body knowledge-panel">
        {err && <div className="banner banner--error">{err}</div>}
        {lastImport && (lastImport.imported.length > 0 || lastImport.skipped.length > 0) && (
          <div className="banner knowledge-import-banner">
            已导入 {lastImport.imported.length} 个文件
            {lastImport.skipped.length ? `,跳过 ${lastImport.skipped.length} 个:${lastImport.skipped[0].error}` : ""}
          </div>
        )}

        <div className="knowledge-overview">
          <span>{view?.bases.length ?? 0} 库</span>
          <span>{view?.documents.length ?? 0} 文件</span>
          <span>{totalChunks} 片段</span>
          {selectedBaseIds.length > 0 && <span className="knowledge-overview__hot">已用于聊天 {selectedBaseIds.length}</span>}
        </div>

        <section className="knowledge-create">
          <input
            value={newName}
            onChange={(event) => setNewName(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") void createBase();
            }}
            placeholder="新知识库名称"
            disabled={busy}
          />
          <button className="btn btn--small" onClick={() => void createBase()} disabled={busy}>
            <Plus size={14} />
            创建
          </button>
        </section>

        <div className="knowledge-layout">
          <section className="knowledge-section">
            <div className="knowledge-section__head">
              <span>知识库</span>
            </div>
            <div className="knowledge-base-list">
              {view?.bases.length ? (
                view.bases.map((base) => {
                  const selected = selectedSet.has(base.id);
                  const active = activeBase?.id === base.id;
                  return (
                    <div className={active ? "knowledge-base knowledge-base--active" : "knowledge-base"} key={base.id}>
                      <button className="knowledge-base__main" onClick={() => setActiveBaseId(base.id)} title={base.name}>
                        <BookOpen size={15} />
                        <span>
                          <strong>{base.name}</strong>
                          <small>
                            {base.documents} 文件 · {base.chunks} 片段
                          </small>
                        </span>
                      </button>
                      <button className={selected ? "chip chip--icon chip--on" : "chip chip--icon"} onClick={() => toggleSelected(base.id)} title={selected ? "取消用于聊天" : "用于聊天"}>
                        {selected ? <CheckCircle2 size={14} /> : <Circle size={14} />}
                      </button>
                      <button className="chip chip--icon" onClick={() => void deleteBase(base.id)} disabled={busy} title="删除">
                        <Trash2 size={13} />
                      </button>
                    </div>
                  );
                })
              ) : (
                <div className="knowledge-empty">还没有知识库</div>
              )}
            </div>
          </section>

          <section className="knowledge-section">
            <div className="knowledge-section__head">
              <span>{activeBase?.name || "文件"}</span>
              <button className="btn btn--small" onClick={() => void importFiles()} disabled={busy || !activeBase} title={`支持: ${supportedHint}`}>
                {busy ? <Loader2 className="spin" size={14} /> : <Upload size={14} />}
                导入文件
              </button>
            </div>

            <div className="knowledge-doc-list">
              {activeDocs.length ? (
                <FileReferenceStrip files={activeDocReferences} variant="knowledge" />
              ) : (
                <div className="knowledge-empty">当前知识库还没有文件</div>
              )}
            </div>

            <div className="knowledge-search">
              <Search size={14} />
              <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索文件内容" />
              {searching && <Loader2 className="spin" size={14} />}
            </div>

            {search?.matches.length ? (
              <div className="knowledge-matches">
                {search.matches.map((match, index) => (
                  <div className="knowledge-match" key={match.chunkId}>
                    <FileReferenceStrip files={[knowledgeMatchReference(match, index)]} variant="knowledge-match" />
                    <p>{match.text}</p>
                  </div>
                ))}
              </div>
            ) : query.trim() && !searching ? (
              <div className="knowledge-empty">没有命中片段</div>
            ) : null}
          </section>
        </div>
      </div>
    </ResizableDrawer>
  );
}
