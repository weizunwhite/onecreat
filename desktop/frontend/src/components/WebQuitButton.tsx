import { useState } from "react";
import { Power } from "lucide-react";
import { app, isWebMode } from "../lib/bridge";
import { confirmDialog } from "../lib/confirm";

// WebQuitButton 是 Web 模式的「退出 OneCreat」入口:桌面版(Wails)有窗口关闭按钮,
// 不需要这个,所以只在 Web 模式渲染。点击 → 二次确认 → 调 App.Quit()(后端优雅保存会话 +
// 关 controller + 停服)→ 显示一个覆盖层告诉用户「已退出,可以关闭此标签」。
//
// 这是本任务里唯一为「退出入口」新增的前端组件(Step 5.2 要求),自成一体,不改其它组件。
export function WebQuitButton() {
  const [quit, setQuit] = useState(false);
  if (!isWebMode()) return null;

  const onQuit = async () => {
    if (!(await confirmDialog({ message: "退出 OneCreat?本地服务会停止,当前会话已自动保存。", confirmText: "退出", danger: true }))) {
      return;
    }
    setQuit(true); // 乐观:后端停服后这个 RPC 可能因连接断开而 reject,忽略即可
    try {
      await app.Quit();
    } catch {
      // 服务已在关闭,连接被切断属预期,不弹错
    }
  };

  return (
    <>
      <button className="chip chip--icon" onClick={() => void onQuit()} title="退出 OneCreat(停止本地服务)">
        <Power size={13} />
      </button>
      {quit && (
        <div
          style={{
            position: "fixed",
            inset: 0,
            zIndex: 9999,
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            flexDirection: "column",
            gap: "12px",
            background: "rgba(0,0,0,0.72)",
            color: "#fff",
            textAlign: "center",
            padding: "24px",
          }}
        >
          <Power size={32} />
          <div style={{ fontSize: "16px", fontWeight: 600 }}>OneCreat 已退出</div>
          <div style={{ fontSize: "13px", opacity: 0.85 }}>本地服务已停止,可以关闭此浏览器标签了。</div>
          <div style={{ fontSize: "12px", opacity: 0.6 }}>要再次使用,重新双击 onecreat-web 即可。</div>
        </div>
      )}
    </>
  );
}
