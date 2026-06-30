// 内置文件夹选择器的"承诺式打开"单例。任何地方调 openFolderPicker() 弹出 app 内的
// 选择面板,用户选好/取消后 resolve 出路径("" = 取消)。FolderPicker 组件在根部渲染
// 一次并订阅这里。这样就不用走 macOS 原生对话框(它在隐藏标题栏窗口下会跑到窗口后面)。

let resolver: ((path: string) => void) | null = null;
let openListener: ((open: boolean, startPath: string) => void) | null = null;

export function openFolderPicker(start = ""): Promise<string> {
  // 若上一个还没关就先取消它,避免悬挂的 Promise。
  if (resolver) {
    const prev = resolver;
    resolver = null;
    prev("");
  }
  return new Promise((resolve) => {
    resolver = resolve;
    openListener?.(true, start);
  });
}

// 组件用:订阅打开/关闭请求;返回取消订阅函数。
export function subscribeFolderPicker(cb: (open: boolean, startPath: string) => void): () => void {
  openListener = cb;
  return () => {
    if (openListener === cb) openListener = null;
  };
}

// 组件用:用户选好(传路径)或取消(传 "")时调用。
export function resolveFolderPicker(path: string): void {
  const r = resolver;
  resolver = null;
  openListener?.(false, "");
  r?.(path);
}
