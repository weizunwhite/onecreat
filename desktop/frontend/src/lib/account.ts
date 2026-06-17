import { useSyncExternalStore } from "react";
import type { AccountSession } from "./types";

// 全局账号会话 store(像 detailMode 那样):App 拉取后写进来,任何组件用 useSession/useCan
// 读取,不用层层透传 props。session=null 表示"还没拉到"(加载中)。

let session: AccountSession | null = null;
const listeners = new Set<() => void>();

export function setSessionStore(s: AccountSession | null): void {
  session = s;
  listeners.forEach((l) => l());
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}
function getSnapshot(): AccountSession | null {
  return session;
}

export function useSession(): AccountSession | null {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// useCan 返回一个判定函数:超管(isAdmin)拥有全部;否则看 permissions 里有没有这个 key。
export function useCan(): (key: string) => boolean {
  const s = useSession();
  return (key: string) => !!s && (s.isAdmin || s.permissions.includes(key));
}
