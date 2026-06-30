import { useSyncExternalStore } from "react";

// 应用内确认 / 提示弹窗。Wails 的 WKWebView 默认不实现 window.confirm/alert/prompt ——
// confirm 直接返回 false、alert 无反应(行内重命名不用 window.prompt 也是同一原因)。所以
// 一切「确定吗 / 提示」都必须走应用内 UI,否则像退出、删除这类操作会静默失效。
// 用法:任何组件 `await confirmDialog(...)` 拿布尔结果;<ConfirmHost/> 在 App 顶层渲染一次。

export interface ConfirmOptions {
  message: string;
  title?: string;
  confirmText?: string;
  cancelText?: string;
  danger?: boolean; // 危险操作(删除 / 退出):确认按钮用警示色
  alert?: boolean; // true = 只显示一个确认按钮(纯提示,不需要「取消」)
}

interface Pending extends ConfirmOptions {
  resolve: (ok: boolean) => void;
}

let pending: Pending | null = null;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());

// confirmDialog 弹出确认框:resolve(true)=确认,resolve(false)=取消 / 点遮罩。传字符串等价于
// { message }。同一时刻只保留一个,新的弹窗会把旧的当作取消处理。
export function confirmDialog(opts: ConfirmOptions | string): Promise<boolean> {
  const o = typeof opts === "string" ? { message: opts } : opts;
  return new Promise<boolean>((resolve) => {
    if (pending) pending.resolve(false);
    pending = { ...o, resolve };
    emit();
  });
}

// alertDialog 是单按钮提示(替代 window.alert)。
export function alertDialog(message: string, confirmText = "知道了"): Promise<boolean> {
  return confirmDialog({ message, confirmText, alert: true });
}

function settle(ok: boolean) {
  const p = pending;
  pending = null;
  emit();
  p?.resolve(ok);
}

const subscribe = (cb: () => void) => {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
};
const getSnapshot = () => pending;

export function ConfirmHost() {
  const p = useSyncExternalStore(subscribe, getSnapshot);
  if (!p) return null;
  return (
    <div className="confirm-overlay" onClick={() => settle(false)}>
      <div className="confirm-box" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        {p.title && <div className="confirm-box__title">{p.title}</div>}
        <div className="confirm-box__msg">{p.message}</div>
        <div className="confirm-box__actions">
          {!p.alert && (
            <button className="confirm-box__btn" onClick={() => settle(false)}>
              {p.cancelText ?? "取消"}
            </button>
          )}
          <button
            className={`confirm-box__btn confirm-box__btn--primary${p.danger ? " confirm-box__btn--danger" : ""}`}
            onClick={() => settle(true)}
            autoFocus
          >
            {p.confirmText ?? "确定"}
          </button>
        </div>
      </div>
    </div>
  );
}
