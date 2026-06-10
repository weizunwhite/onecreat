import type { CSSProperties } from "react";
import { Activity, FileText } from "lucide-react";
import { extractFileReferences, fileRemarkFor, type FileReference } from "../lib/fileRemarks";
import { useT } from "../lib/i18n";
import type { Todo } from "../lib/tools";
import { FileReferenceStrip } from "./FileReferenceStrip";

const HARDWARE_CONTEXT_FILES = [
  "hardware_manifest.json",
  "docs/wiring.md",
  "docs/verification.md",
  "docs/board_profile.md",
  "tests/hardware_checklist.md",
  "tests/hardware_evidence.jsonl",
  "src/main.cpp",
  "platformio.ini",
];

const hardwareKeywords = [
  "hardware",
  "arduino",
  "esp32",
  "esp-idf",
  "platformio",
  "board",
  "port",
  "serial",
  "mcp__hardware",
  "硬件",
  "板卡",
  "端口",
  "串口",
  "烧录",
  "接线",
];

function todoText(todo: Todo): string {
  return todo.status === "in_progress" && todo.activeForm ? todo.activeForm : todo.content;
}

function isHardwareTask(text: string): boolean {
  const lower = text.toLowerCase();
  return hardwareKeywords.some((keyword) => lower.includes(keyword.toLowerCase()));
}

function pushUnique(files: FileReference[], seen: Set<string>, file: FileReference) {
  const key = file.path.toLowerCase();
  if (seen.has(key)) return;
  seen.add(key);
  files.push(file);
}

function contextFiles(todos: Todo[]): FileReference[] {
  const allText = todos.map((todo) => [todo.content, todo.activeForm].filter(Boolean).join("\n")).join("\n");
  const active = todos.find((todo) => todo.status === "in_progress");
  const activeText = active ? [active.content, active.activeForm].filter(Boolean).join("\n") : "";
  const seen = new Set<string>();
  const files: FileReference[] = [];

  for (const file of extractFileReferences(activeText, 6)) {
    pushUnique(files, seen, file);
  }
  for (const file of extractFileReferences(allText, 8)) {
    pushUnique(files, seen, file);
  }
  if (isHardwareTask(allText)) {
    for (const path of HARDWARE_CONTEXT_FILES) {
      pushUnique(files, seen, { path, remark: fileRemarkFor(path) || "硬件上下文" });
    }
  }
  return files.slice(0, 8);
}

function TaskContextFile({
  activeText,
  progressText,
}: {
  activeText: string;
  progressText: string;
}) {
  const t = useT();
  return (
    <div className="task-context-file" title={`task_context.md · ${t("taskContext.fileRemark")}`}>
      <FileText size={13} />
      <span className="task-context-file__body">
        <span className="task-context-file__label">{t("taskContext.file")}</span>
        <span className="task-context-file__line">
          <strong>task_context.md</strong>
          <small>{t("taskContext.fileRemark")}</small>
        </span>
        <span className="task-context-file__meta">
          {progressText}
          {activeText && <code>{activeText}</code>}
        </span>
      </span>
    </div>
  );
}

export function TaskContextBar({
  todos,
  onOpenFile,
}: {
  todos: Todo[];
  onOpenFile?: (path: string) => void;
}) {
  const t = useT();
  if (todos.length === 0) return null;

  const done = todos.filter((todo) => todo.status === "completed").length;
  const active = todos.find((todo) => todo.status === "in_progress") ?? todos.find((todo) => todo.status !== "completed") ?? todos[0];
  const activeText = todoText(active);
  const progress = Math.round((done / todos.length) * 100);
  const progressText = `${done}/${todos.length}`;
  const files = contextFiles(todos);
  const hardware = isHardwareTask(todos.map(todoText).join("\n"));

  return (
    <section className={`task-context${hardware ? " task-context--hardware" : ""}`}>
      <div className="task-context__head">
        <span className="task-context__label">
          <Activity size={13} />
          {t("taskContext.title")}
        </span>
        <span className="task-context__count">{progressText}</span>
        <span className="task-context__progress" aria-hidden="true">
          <span style={{ "--task-context-progress": `${progress}%` } as CSSProperties} />
        </span>
      </div>

      <div className="task-context__body">
        <TaskContextFile activeText={activeText} progressText={progressText} />
        {files.length > 0 && (
          <div className="task-context__files">
            <FileReferenceStrip files={files} variant="context" onOpenFile={onOpenFile} />
          </div>
        )}
      </div>
    </section>
  );
}
