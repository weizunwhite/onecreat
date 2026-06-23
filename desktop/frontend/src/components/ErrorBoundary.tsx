import { Component, type ReactNode } from "react";
import { FileText } from "lucide-react";
import { reportCrash, copyText } from "../lib/crash";

export class ErrorBoundary extends Component<{ children: ReactNode }, { crashed: boolean }> {
  state = { crashed: false };

  static getDerivedStateFromError() {
    return { crashed: true };
  }

  componentDidCatch(error: unknown, info: { componentStack?: string | null }) {
    reportCrash("react", error, info.componentStack ?? undefined);
  }

  render() {
    if (!this.state.crashed) return this.props.children;
    return (
      <div className="error-boundary">
        <div className="crash-overlay__panel">
          <div className="crash-overlay__file">
            <strong>render_error.log</strong>
            <small>界面崩溃记录（React 运行时）</small>
          </div>
          <div className="crash-overlay__title">OneCreat 组件渲染异常</div>
          <div className="crash-overlay__body">
            OneCreat 发生组件级异常，当前界面已经切换到降级面板。请查看日志并重启应用继续。
          </div>
          <button
            className="crash-overlay__copy"
            type="button"
            onClick={() => void copyText("OneCreat 组件级异常，当前界面已降级，请重启应用继续。")}
          >
            <FileText size={12} />
            重新上报
          </button>
        </div>
      </div>
    );
  }
}
