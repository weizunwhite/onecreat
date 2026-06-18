import { ResizableDrawer } from "./ResizableDrawer";

// 应用内「使用教程」抽屉:直接嵌入随包带的 guide.html(同一份内容也用作下载页的独立教程)。
export function HelpDrawer({ onClose }: { onClose: () => void }) {
  return (
    <ResizableDrawer onClose={onClose} wide>
      <header className="drawer__head">
        <div className="drawer__title">使用教程</div>
        <button className="chip" onClick={onClose} title="关闭">
          ✕
        </button>
      </header>
      <iframe src="guide.html" title="使用教程" className="help-iframe" />
    </ResizableDrawer>
  );
}
