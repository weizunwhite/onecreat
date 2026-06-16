import { useCallback, useEffect, useRef, useState } from "react";
import { AlignLeft, Eraser, LineChart, Plug, RefreshCw, Send, Sliders, Unplug, X } from "lucide-react";
import { app, onSerialClosed, onSerialData } from "../lib/bridge";
import { SerialPlot, type SerialFrame } from "./SerialPlot";
import { SerialControls } from "./SerialControls";

// 串口监视器:常驻连接 + 波特率选择 + 实时滚动 + 发送框(Phase 1),
// 文本/曲线切换的实时数据可视化(Phase 2),拖滑块/点按钮发变量的交互控件(Phase 3)。
// 数据通过 bridge 的 serial:data 事件流进来;发送走 app.SerialWrite。

const BAUD_RATES = [9600, 19200, 38400, 57600, 74880, 115200, 230400, 460800, 921600];
const MAX_CHARS = 200000; // 显示缓冲上限,超出从头丢,防止长跑内存涨爆
const MAX_FRAMES = 400; // 曲线窗口:只保留最近 400 个采样点(滚动显示)

// 发送时追加的行结束符(对应 Arduino IDE 的「换行/回车/两者/无」)。
const LINE_ENDINGS: { key: string; label: string; value: string }[] = [
  { key: "nl", label: "换行 (NL)", value: "\n" },
  { key: "cr", label: "回车 (CR)", value: "\r" },
  { key: "crnl", label: "回车换行 (CRLF)", value: "\r\n" },
  { key: "none", label: "无", value: "" },
];

// 把一行串口文本解析成一帧曲线数据:
// 1) 优先找「名字:数值」或「名字=数值」对(如 temp:23.5 hum:60)→ 多路有名字的曲线;
// 2) 没有命名对时,退而提取所有裸数字按位置记成 ch1/ch2…(如 "23.5 60" 或 "#1646")。
// 解析不出数字就返回 null(那一行不画)。
const LABELED_RE = /([A-Za-z_][A-Za-z0-9_]*)\s*[:=]\s*(-?\d+(?:\.\d+)?)/g;
const NUMBER_RE = /-?\d+(?:\.\d+)?/g;
function parseFrame(line: string, idx: number): SerialFrame | null {
  const values: Record<string, number> = {};
  let labeled = false;
  LABELED_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = LABELED_RE.exec(line)) !== null) {
    values[m[1]] = parseFloat(m[2]);
    labeled = true;
  }
  if (!labeled) {
    NUMBER_RE.lastIndex = 0;
    let i = 0;
    let n: RegExpExecArray | null;
    while ((n = NUMBER_RE.exec(line)) !== null) {
      i += 1;
      values["ch" + i] = parseFloat(n[0]);
    }
  }
  if (Object.keys(values).length === 0) return null;
  return { i: idx, values };
}

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
  const [viewMode, setViewMode] = useState<"text" | "plot">("text"); // Phase 2:文本 / 曲线
  const [controlsOpen, setControlsOpen] = useState(false); // Phase 3:交互控件面板
  const [frameVersion, setFrameVersion] = useState(0); // 自增触发曲线重画(rAF 已节流)

  const bufRef = useRef("");
  const pendingRef = useRef(false);
  const viewRef = useRef<HTMLDivElement | null>(null);
  const unsubsRef = useRef<(() => void)[]>([]);
  const lineBufRef = useRef(""); // 行解析的残片缓冲(凑不满一行先存着)
  const framesRef = useRef<SerialFrame[]>([]); // 曲线数据帧滚动窗口
  const frameIdxRef = useRef(0); // 采样点序号(曲线 X 轴)

  // 文本 + 曲线 的统一刷新:rAF 节流,扛得住高频数据不卡。
  const flush = useCallback(() => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    requestAnimationFrame(() => {
      pendingRef.current = false;
      setText(bufRef.current);
      setFrameVersion((v) => v + 1);
    });
  }, []);

  // 写入纯文本(状态提示、本地回显):只进文本缓冲,不解析成曲线(否则 "115200" 之类会污染曲线)。
  const appendNote = useCallback(
    (chunk: string) => {
      bufRef.current = (bufRef.current + chunk).slice(-MAX_CHARS);
      flush();
    },
    [flush],
  );

  // 写入真实串口数据:既进文本缓冲,也按整行解析成曲线帧。
  const append = useCallback(
    (chunk: string) => {
      bufRef.current = (bufRef.current + chunk).slice(-MAX_CHARS);
      lineBufRef.current += chunk;
      if (lineBufRef.current.length > 8192) lineBufRef.current = lineBufRef.current.slice(-8192); // 防超长无换行行撑爆
      const parts = lineBufRef.current.split("\n");
      lineBufRef.current = parts.pop() ?? ""; // 最后一段是残片,留到下次
      for (const raw of parts) {
        const frame = parseFrame(raw, frameIdxRef.current);
        if (frame) {
          framesRef.current.push(frame);
          frameIdxRef.current += 1;
        }
      }
      if (framesRef.current.length > MAX_FRAMES) {
        framesRef.current = framesRef.current.slice(-MAX_FRAMES);
      }
      flush();
    },
    [flush],
  );

  const resetData = useCallback(() => {
    bufRef.current = "";
    lineBufRef.current = "";
    framesRef.current = [];
    frameIdxRef.current = 0;
  }, []);

  const refreshPorts = useCallback(async () => {
    const list = await app.SerialPorts().catch((): string[] => []);
    setPorts(list);
    setPort((cur) => cur || (initialPort && list.includes(initialPort) ? initialPort : list[0] ?? ""));
  }, [initialPort]);

  useEffect(() => {
    void refreshPorts();
  }, [refreshPorts]);

  // 自动滚到底(文本视图、开了自动滚动时)。
  useEffect(() => {
    if (viewMode === "text" && autoscroll && viewRef.current) {
      viewRef.current.scrollTop = viewRef.current.scrollHeight;
    }
  }, [text, autoscroll, viewMode]);

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
      resetData(); // 新连接清空历史曲线/文本
      appendNote(`— 已连接 ${port} @ ${baud} —\n`);
      unsubsRef.current.push(onSerialData(append));
      unsubsRef.current.push(
        onSerialClosed((reason) => {
          setConnected(false);
          teardown();
          appendNote(`\n— 串口断开:${reason} —\n`);
        }),
      );
      setConnected(true);
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  }, [port, baud, append, appendNote, resetData, teardown]);

  const disconnect = useCallback(async () => {
    teardown();
    await app.SerialClose().catch(() => {});
    setConnected(false);
    appendNote(`\n— 已断开 —\n`);
  }, [teardown, appendNote]);

  // 统一发送:发送框和交互控件都走这里。silent=true 不回显(滑块拖动时避免刷屏)。
  const sendRaw = useCallback(
    async (payload: string, opts?: { silent?: boolean }) => {
      if (!connected) return;
      const ending = LINE_ENDINGS.find((e) => e.key === endingKey)?.value ?? "\n";
      const r = await app.SerialWrite(payload + ending);
      if (!r.ok) {
        setError(r.error ?? "发送失败");
        return;
      }
      if (!opts?.silent) appendNote(`» ${payload}\n`); // 本地回显自己发的(» 前缀区分)
    },
    [connected, endingKey, appendNote],
  );

  const send = useCallback(async () => {
    if (!connected || input === "") return;
    await sendRaw(input);
    setInput("");
  }, [connected, input, sendRaw]);

  const clear = useCallback(() => {
    resetData();
    setText("");
    setFrameVersion((v) => v + 1);
  }, [resetData]);

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
          {/* Phase 2:文本 / 曲线 视图切换 */}
          <div className="serialmon__seg">
            <button className={viewMode === "text" ? "is-on" : ""} onClick={() => setViewMode("text")} title="文本视图">
              <AlignLeft size={12} /> 文本
            </button>
            <button className={viewMode === "plot" ? "is-on" : ""} onClick={() => setViewMode("plot")} title="曲线视图(把数字画成实时曲线)">
              <LineChart size={12} /> 曲线
            </button>
          </div>
          {/* Phase 3:交互控件开关 */}
          <button
            className={`chip chip--icon ${controlsOpen ? "chip--on" : ""}`}
            onClick={() => setControlsOpen((v) => !v)}
            title="交互控件:拖滑块 / 点按钮把变量发给开发板"
          >
            <Sliders size={13} />
          </button>
          <label className="serialmon__check">
            <input type="checkbox" checked={autoscroll} onChange={(e) => setAutoscroll(e.target.checked)} />
            自动滚动
          </label>
          <button className="chip chip--icon" onClick={clear} title="清屏">
            <Eraser size={13} />
          </button>
        </div>

        {error && <div className="serialmon__error">{error}</div>}

        {viewMode === "plot" ? (
          <SerialPlot framesRef={framesRef} version={frameVersion} />
        ) : (
          <div className="serialmon__view" ref={viewRef}>
            {text || <span className="serialmon__placeholder">连接后这里实时显示串口数据…</span>}
          </div>
        )}

        {controlsOpen && <SerialControls disabled={!connected} onSend={sendRaw} />}

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
