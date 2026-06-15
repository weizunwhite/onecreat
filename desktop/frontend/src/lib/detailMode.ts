// 全局「显示详细度」开关:简洁(默认,给学生/老师看人话)vs 详细(给老师/高年级看
// 原始命令和思考过程)。用模块级 store + useSyncExternalStore,任何组件直接订阅,
// 不必铺 Provider。状态持久化到 localStorage,重启保留。
import { useSyncExternalStore } from "react";

const KEY = "onecreat.detailMode";

let detail: boolean = (() => {
  try {
    return localStorage.getItem(KEY) === "1";
  } catch {
    return false; // 默认简洁模式
  }
})();

const listeners = new Set<() => void>();

export function setDetailMode(v: boolean): void {
  if (v === detail) return;
  detail = v;
  try {
    localStorage.setItem(KEY, v ? "1" : "0");
  } catch {
    /* localStorage 不可用时忽略,仅内存生效 */
  }
  listeners.forEach((l) => l());
}

export function toggleDetailMode(): void {
  setDetailMode(!detail);
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

function getSnapshot(): boolean {
  return detail;
}

// useDetailMode 返回当前是否「详细模式」(true=详细, false=简洁)。
export function useDetailMode(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot);
}
