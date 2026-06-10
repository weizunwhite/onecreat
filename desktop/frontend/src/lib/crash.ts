// Last-resort crash surface: a React render error with no boundary unmounts the
// whole tree (blank window), and global errors/rejections leave no trace either.

const CRASH_FILE = "crash_report.log";

// copyText 安全复制:优先 navigator.clipboard,被拒/不可用时回退到 textarea+execCommand,
// 内部吞掉所有错误,绝不抛出未捕获的 Promise 异常(否则会触发全局崩溃屏)。返回是否成功。
export function copyText(text: string): Promise<boolean> {
  const fallback = (): boolean => {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch {
      return false;
    }
  };
  try {
    const p = navigator.clipboard?.writeText(text);
    if (p && typeof p.then === "function") {
      return p.then(() => true).catch(() => fallback());
    }
  } catch {
    /* navigator.clipboard 抛同步异常时也走回退 */
  }
  return Promise.resolve(fallback());
}

function paint(text: string) {
  let host = document.getElementById("crash-overlay");
  if (!host) {
    host = document.createElement("div");
    host.id = "crash-overlay";
    document.body.appendChild(host);
  }
  host.className = "crash-overlay";
  const panel = document.createElement("div");
  panel.className = "crash-overlay__panel";
  const file = document.createElement("div");
  file.className = "crash-overlay__file";
  const fileName = document.createElement("strong");
  fileName.textContent = CRASH_FILE;
  const fileRemark = document.createElement("small");
  fileRemark.textContent = "前端崩溃日志";
  file.append(fileName, fileRemark);

  const title = document.createElement("div");
  title.className = "crash-overlay__title";
  title.textContent = "onecreat 运行时异常";
  const body = document.createElement("pre");
  body.className = "crash-overlay__body";
  body.textContent = text;
  const copy = document.createElement("button");
  copy.className = "crash-overlay__copy";
  copy.textContent = "复制错误日志";
  copy.onclick = () => {
    void copyText(text).then((ok) => {
      copy.textContent = ok ? "已复制" : "复制失败,请手动选择";
    });
  };
  panel.append(file, title, body, copy);
  host.replaceChildren(panel);
}

function format(label: string, err: unknown, extra?: string): string {
  const e = err as { message?: string; stack?: string } | null;
  const detail = e?.stack || e?.message || String(err);
  return [`[${label}]`, detail, extra?.trim()].filter(Boolean).join("\n\n");
}

export function reportCrash(label: string, err: unknown, extra?: string) {
  paint(format(label, err, extra));
}

export function installGlobalCrashHandlers() {
  window.addEventListener("error", (e) => reportCrash("window.error", e.error ?? e.message));
  window.addEventListener("unhandledrejection", (e) => reportCrash("unhandledrejection", e.reason));
}
