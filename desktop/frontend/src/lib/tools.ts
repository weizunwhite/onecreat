// Per-tool presentation helpers. The kernel forwards every tool call the same way
// (name + raw-JSON args + output); these turn that generic payload into the
// recognizable one-liner, inline diff, and collapsed outcome each tool deserves —
// the recognizable "card" vocabulary the desktop uses. Kept pure (no React, no
// highlight.js) so ToolCard stays a renderer and the main bundle stays light.

import { diffLines } from "./diff";
import { t } from "./i18n";
import { extToLang } from "./lang";
import type { DictKey } from "../locales/en";

export interface ToolDiff {
  original: string;
  modified: string;
  lang: string;
  path?: string;
  label?: string; // multi_edit labels each step ("edit 1", …)
}

function parse(args: string): Record<string, unknown> {
  try {
    return JSON.parse(args) as Record<string, unknown>;
  } catch {
    return {};
  }
}

function str(a: Record<string, unknown>, key: string): string {
  return typeof a[key] === "string" ? (a[key] as string) : "";
}

// subjectOf pulls the most informative one-liner out of a call's args — the
// command for bash, the pattern for search, the path for file tools, the
// description for a sub-task — so the collapsed row reads at a glance.
export function subjectOf(name: string, args: string): string {
  const a = parse(args);
  switch (name) {
    case "bash":
      return str(a, "command");
    case "grep":
    case "glob":
      return str(a, "pattern") || str(a, "path");
    case "web_fetch":
      return str(a, "url");
    case "task":
      return str(a, "description") || str(a, "prompt");
    case "remember":
      return str(a, "name") || str(a, "description");
    case "todo_write":
    case "exit_plan_mode":
      return ""; // these get dedicated cards, not a subject line
    default:
      return str(a, "path") || str(a, "file_path");
  }
}

// friendlyLabel 把工具调用翻译成一句「人话」动作短语,给非技术用户看(简洁模式标题)。
// 例:bash 跑 `pio run -t upload` → 「烧录到开发板」。返回空串表示没有对应人话,
// 调用方回退到原始工具名。覆盖内置工具 + 硬件 MCP 工具(mcp__hardware__*)。
export function friendlyLabel(name: string, args: string): string {
  const a = parse(args);
  // 先按工具名里的关键词归类(硬件 MCP 工具名形如 mcp__hardware__arduino_upload)
  const n = name.toLowerCase();
  if (/upload|flash|烧录/.test(n)) return t("tool.do.upload");
  if (/compile|build/.test(n)) return t("tool.do.compile");
  if (/monitor|serial/.test(n)) return t("tool.do.monitor");
  if (/detect/.test(n)) return t("tool.do.detectHw");
  if (/scaffold/.test(n)) return t("tool.do.scaffold");
  if (/install/.test(n)) return t("tool.do.install");
  if (n.includes("mcp__hardware__") || n.startsWith("arduino_") || n.startsWith("esp_")) {
    return t("tool.do.hardware");
  }
  switch (name) {
    case "read_file":
      return t("tool.do.read");
    case "write_file":
      return t("tool.do.write");
    case "edit_file":
    case "multi_edit":
      return t("tool.do.edit");
    case "grep":
    case "glob":
      return t("tool.do.search");
    case "ls":
      return t("tool.do.list");
    case "web_fetch":
      return t("tool.do.web");
    case "task":
      return t("tool.do.task");
    case "remember":
      return t("tool.do.remember");
    case "bash":
      return bashLabel(str(a, "command"));
  }
  return "";
}

// bashLabel 看 bash 命令内容,归到具体动作(编译/烧录/安装/Git/找板子/跑命令)。
function bashLabel(cmd: string): string {
  const c = cmd.toLowerCase();
  if (/(-t\s+upload|arduino-cli\s+upload|esptool|write_flash|idf\.py.*flash|\bupload\b)/.test(c)) {
    return t("tool.do.upload");
  }
  if (/(pio\s+run|arduino-cli\s+compile|idf\.py\s+build|\bcmake\b|\bmake\b|\bcompile\b)/.test(c)) {
    return t("tool.do.compile");
  }
  if (/(pip\s+install|lib\s+install|core\s+install|npm\s+install|pnpm\s+(install|add)|brew\s+install|apt(-get)?\s+install)/.test(c)) {
    return t("tool.do.install");
  }
  if (/(^|\s)git(\s|$)/.test(c)) return t("tool.do.git");
  if (/(\/dev\/cu\.|\/dev\/tty|monitor|\bscreen\b|usbserial)/.test(c)) return t("tool.do.findBoard");
  return t("tool.do.bash");
}

// friendlySubject 给简洁模式用:文件类工具显示文件名(去路径),搜索显示关键词,
// bash/硬件等返回空串(原始命令藏进「详情」,不在标题暴露看不懂的命令)。
export function friendlySubject(name: string, args: string): string {
  const a = parse(args);
  switch (name) {
    case "read_file":
    case "write_file":
    case "edit_file":
    case "multi_edit": {
      const p = str(a, "path") || str(a, "file_path");
      if (!p) return "";
      const base = p.split("/").pop();
      return base || p;
    }
    case "grep":
    case "glob":
      return str(a, "pattern");
    default:
      return "";
  }
}

// diffsFor returns the before/after pairs a writer tool's card renders inline:
// edit_file is one pair, write_file is an all-add (empty original), multi_edit is
// one pair per step. Returns [] for non-writers, so the card folds args/output
// away instead.
export function diffsFor(name: string, args: string): ToolDiff[] {
  const a = parse(args);
  const path = str(a, "path") || str(a, "file_path");
  const lang = extToLang(path);
  if (name === "edit_file") {
    if (typeof a.old_string === "string" && typeof a.new_string === "string") {
      return [{ original: a.old_string, modified: a.new_string, lang, path }];
    }
  }
  if (name === "write_file" && typeof a.content === "string") {
    return [{ original: "", modified: a.content, lang, path }];
  }
  if (name === "multi_edit" && Array.isArray(a.edits)) {
    const out: ToolDiff[] = [];
    (a.edits as unknown[]).forEach((e, i) => {
      const step = e as Record<string, unknown>;
      if (typeof step?.old_string === "string" && typeof step?.new_string === "string") {
        out.push({ original: step.old_string, modified: step.new_string, lang, path, label: `edit ${i + 1}` });
      }
    });
    return out;
  }
  return [];
}

export type TodoStatus = "pending" | "in_progress" | "completed";

export interface Todo {
  content: string;
  status: TodoStatus | string;
  activeForm?: string;
  level?: number; // 0 = phase, 1 = sub-step of the phase above it
}

// parseTodos pulls the task list out of a todo_write call's args.
export function parseTodos(args: string): Todo[] {
  try {
    const a = JSON.parse(args) as { todos?: Todo[] };
    return Array.isArray(a.todos) ? a.todos : [];
  } catch {
    return [];
  }
}

function plusMinus(original: string, modified: string): { add: number; del: number } {
  let add = 0;
  let del = 0;
  for (const r of diffLines(original, modified)) {
    if (r.type === "add") add++;
    else if (r.type === "del") del++;
  }
  return { add, del };
}

// lineCount counts lines, ignoring a single trailing newline so "a\n" reads as 1.
function lineCount(s: string): number {
  if (!s) return 0;
  const t = s.endsWith("\n") ? s.slice(0, -1) : s;
  return t === "" ? 0 : t.split("\n").length;
}

function nonEmptyLines(s: string): number {
  return s.split("\n").filter((l) => l.trim() !== "").length;
}

// countOf renders a localized "N <noun>" using the singular/plural key pair (zh
// collapses both to one form). Lives here, not the dict, so the counted phrasing
// stays a translation concern.
function countOf(n: number, one: DictKey, other: DictKey): string {
  return t(n === 1 ? one : other, { n });
}

// summarize derives the one-line outcome shown under a finished card (the "⎿"
// secondary line) — counts from the args for writers, from the output for
// readers. "" means there's nothing worth a summary line.
export function summarize(name: string, args: string, output?: string, error?: string): string {
  if (error) return "";
  const a = parse(args);
  switch (name) {
    case "write_file":
      return countOf(lineCount(str(a, "content")), "tool.lineOne", "tool.lineOther");
    case "edit_file": {
      if (typeof a.old_string === "string" && typeof a.new_string === "string") {
        const { add, del } = plusMinus(a.old_string, a.new_string);
        return `+${add} -${del}`;
      }
      return "";
    }
    case "multi_edit": {
      const edits = Array.isArray(a.edits) ? (a.edits as Record<string, unknown>[]) : [];
      let add = 0;
      let del = 0;
      for (const e of edits) {
        if (typeof e?.old_string === "string" && typeof e?.new_string === "string") {
          const pm = plusMinus(e.old_string, e.new_string);
          add += pm.add;
          del += pm.del;
        }
      }
      return `${countOf(edits.length, "tool.editOne", "tool.editOther")} · +${add} -${del}`;
    }
  }

  if (!output) return "";
  switch (name) {
    case "read_file": {
      if (output.startsWith("(empty file)")) return t("tool.emptyFile");
      const arrows = (output.match(/→/g) || []).length;
      return countOf(arrows || lineCount(output), "tool.lineOne", "tool.lineOther");
    }
    case "grep":
      return countOf(nonEmptyLines(output), "tool.matchOne", "tool.matchOther");
    case "glob":
      return countOf(nonEmptyLines(output), "tool.fileOne", "tool.fileOther");
    case "ls":
      return countOf(nonEmptyLines(output), "tool.entryOne", "tool.entryOther");
    case "web_fetch":
      return output.split("\n", 1)[0].slice(0, 80);
    case "bash":
      return output.trim() === "" ? t("tool.noOutput") : countOf(lineCount(output), "tool.lineOne", "tool.lineOther");
    default:
      return "";
  }
}
