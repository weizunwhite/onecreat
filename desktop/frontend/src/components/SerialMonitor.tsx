import { useCallback, useEffect, useRef, useState } from "react";
import { Eraser, Plug, RefreshCw, Send, Unplug, X } from "lucide-react";
import { app, onSerialClosed, onSerialData } from "../lib/bridge";

// 串口监视器(Phase 1):常驻连接 + 波特率选择 + 实时滚动 + 发送框。
// 数据通过 bridge 的 serial:data 事件流进来;发送走 app.SerialWrite。
// 后续 Phase 2(实时曲线)/Phase 3(滑块控件)会在这个面板里继续加。

const BAUD_RATES = [9600, 19200, 38400, 57600, 74880, 115200, 230400, 460800, 921600];
const MAX_CHARS = 200000; // 显示缓冲上限,超出从头丢,防止长跑内存涨爆

// 发送时追加的行结束符(对应 Arduino IDE 的「换行/回车/两者/无」)。
const LINE_ENDINGS: { key: string; label: string; value: string }[] = [
  { key: "nl", label: "换行 (NL)", value: "\n" },
  { key: "cr", label: "回车 (CR)", value: "\r" },
  { key: "crnl", label: "回车换行 (CRLF)", value: "\r\n" },
  { key: "none", label: "无", value: "" },
];

export function SerialMonitor({ initialPort, onClose }: { initialPort?: string; onClose: () => void }) {
  const [ports, setPorts] = useState<string[]>([]);
  const [port, setPort] = useState(initialPort ?? "");
  const [baud, setBaud] = useState(115200);
  const [connected, setConnected] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [input, setInput] = useState("");
  const [endingKey, setEndingKey] = useState("nl");
  const [autoscroll, setAutoscroll] = useState(true);

  const bufRef = useRef("");
  const pendingRef = useRef(false);
  const viewRef = useRef<HTMLDivElement | null>(null);
  const unsubsRef = useRef<(() => void)[]>([]);

  // 把一段文本追加进显示缓冲;用 rAF 节流刷新,扛得住高频数据不卡。
  const append = useCallback((chunk: string) => {
    bufRef.current = (bufRef.current + chunk).slice(-MAX_CHARS);
    if (!pendingRef.current) {
      pendingRef.current = true;
      requestAnimationFrame(() => {
        pendingRef.current = false;
        setText(bufRef.current);
      });
    }
  }, []);

  const refreshPorts = useCallback(async () => {
    const list = await app.SerialPorts().catch((): string[] => []);
    setPorts(list);
    setPort((cur) => cur || (initialPort && list.includes(initialPort) ? initialPort : list[0] ?? ""));
  }, [initialPort]);

  useEffect(() => {
    void refreshPorts();
  }, [refreshPorts]);

  // 自动滚到底(开了自动滚动时)。
  useEffect(() => {
    if (autoscroll && viewRef.current) {
      viewRef.current.scrollTop = viewRef.current.scrollHeight;
    }
  }, [text, autoscroll]);

  const teardown = useCallback(() => {
    unsubsRef.current.forEach((u) => u());
    unsubsRef.current = [];
  }, []);

  const connect = useCallback(async () => {
    if (!port) {
      setError("请先选择串口");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const r = await app.SerialOpen(port, baud);
      if (!r.ok) {
        setError(r.error ?? "打开串口失败");
        return;
      }
      append(`\n— 已连接 ${port} @ ${baud} —\n`);
      unsubsRef.current.push(onSerialData(append));
      unsubsRef.current.push(
        onSerialClosed((reason) => {
          setConnected(false);
          teardown();
          append(`\n— 串口断开:${reason} —\n`);
        }),
      );
      setConnected(true);
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [port, baud, append, teardown]);

  const disconnect = useCallback(async () => {
    teardown();
    await app.SerialClose().catch(() => {});
    setConnected(false);
    append(`\n— 已断开 —\n`);
  }, [teardown, append]);

  const send = useCallback(async () => {
    if (!connected || input === "") return;
    const ending = LINE_ENDINGS.find((e) => e.key === endingKey)?.value ?? "\n";
    const r = await app.SerialWrite(input + ending);
    if (!r.ok) {
      setError(r.error ?? "发送失败");
      return;
    }
    append(`» ${input}\n`); // 本地回显自己发的(» 前缀区分)
    setInput("");
  }, [connected, input, endingKey, append]);

  const clear = useCallback(() => {
    bufRef.current = "";
    setText("");
  }, []);

  // 关闭面板时务必断开,别留个野连接占着串口。
  useEffect(() => {
    return () => {
      unsubsRef.current.forEach((u) => u());
      unsubsRef.current = [];
      void app.SerialClose().catch(() => {});
    };
  }, []);

  return (
    <div className="serialmon-backdrop" onClick={onClose}>
      <div className="serialmon" onClick={(e) => e.stopPropagation()}>
        <div className="serialmon__head">
          <span className="serialmon__title">串口监视器</span>
          <span className={`serialmon__status serialmon__status--${connected ? "on" : "off"}`}>
            {connected ? `● 已连接 ${baud}` : "○ 未连接"}
          </span>
          <div className="serialmon__head-spacer" />
          <button className="chip chip--icon" onClick={onClose} title="关闭">
            <X size={14} />
          </button>
        </div>

        <div className="serialmon__toolbar">
          <label className="serialmon__field">
            <span>串口</span>
            <select value={port} disabled={connected} onChange={(e) => setPort(e.target.value)}>
              {ports.length === 0 && <option value="">未检测到串口</option>}
              {ports.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <button className="chip chip--icon" disabled={connected} onClick={() => void refreshPorts()} title="刷新串口列表">
            <RefreshCw size={13} />
          </button>
          <label className="serialmon__field">
            <span>波特率</span>
            <select value={baud} disabled={connected} onChange={(e) => setBaud(Number(e.target.value))}>
              {BAUD_RATES.map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </select>
          </label>
          {connected ? (
            <button className="chip serialmon__connect serialmon__connect--on" disabled={busy} onClick={() => void disconnect()}>
              <Unplug size={13} /> 断开
            </button>
          ) : (
            <button className="chip serialmon__connect" disabled={busy} onClick={() => void connect()}>
              <Plug size={13} /> 连接
            </button>
          )}
          <div className="serialmon__toolbar-spacer" />
          <label className="serialmon__check">
            <input type="checkbox" checked={autoscroll} onChange={(e) => setAutoscroll(e.target.checked)} />
            自动滚动
          </label>
          <button className="chip chip--icon" onClick={clear} title="清屏">
            <Eraser size={13} />
          </button>
        </div>

        {error && <div className="serialmon__error">{error}</div>}

        <div className="serialmon__view" ref={viewRef}>
          {text || <span className="serialmon__placeholder">连接后这里实时显示串口数据…</span>}
        </div>

        <div className="serialmon__send">
          <input
            className="serialmon__input"
            value={input}
            placeholder={connected ? "输入要发给开发板的内容,回车发送" : "连接后可发送"}
            disabled={!connected}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void send();
            }}
          />
          <select className="serialmon__ending" value={endingKey} onChange={(e) => setEndingKey(e.target.value)} title="行结束符">
            {LINE_ENDINGS.map((le) => (
              <option key={le.key} value={le.key}>
                {le.label}
              </option>
            ))}
          </select>
          <button className="chip serialmon__sendbtn" disabled={!connected || input === ""} onClick={() => void send()}>
            <Send size={13} /> 发送
          </button>
        </div>
      </div>
    </div>
  );
}
