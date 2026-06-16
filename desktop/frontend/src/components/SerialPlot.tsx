import { useEffect, useRef, type MutableRefObject } from "react";

// 串口实时曲线(Phase 2):把每行串口数据里的数字画成滚动曲线,像 Arduino 串口绘图器
// 但更清楚 —— 自动缩放 Y 轴、多路彩色曲线、右上角图例显示每条的当前值。
// 数据由 SerialMonitor 解析好后通过 framesRef 传进来,version 变了就重画(rAF 已节流)。

// 一帧 = 某一行解析出的若干「名字→数值」。labeled 形如 temp:23.5;裸数字按位置记成 ch1/ch2。
export type SerialFrame = { i: number; values: Record<string, number> };

// 多路曲线配色(够区分、在深浅色主题下都还行)。
const PLOT_COLORS = ["#5b9cff", "#ff7a59", "#34c3a6", "#d9a441", "#b18cff", "#ec6a9c", "#7fce5a", "#4fd0e0"];

export function SerialPlot({ framesRef, version }: { framesRef: MutableRefObject<SerialFrame[]>; version: number }) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  // 颜色按曲线「首次出现顺序」稳定分配,避免每次重画跳色。
  const colorRef = useRef<Map<string, string>>(new Map());

  const colorOf = (name: string): string => {
    let c = colorRef.current.get(name);
    if (!c) {
      c = PLOT_COLORS[colorRef.current.size % PLOT_COLORS.length];
      colorRef.current.set(name, c);
    }
    return c;
  };

  // 把整块画一遍:坐标框 → Y 刻度网格 → 各路折线 → 图例。读 ref,所以永远画最新数据。
  const draw = () => {
    const canvas = canvasRef.current;
    const wrap = wrapRef.current;
    if (!canvas || !wrap) return;
    const cssW = wrap.clientWidth;
    const cssH = wrap.clientHeight;
    if (cssW === 0 || cssH === 0) return;
    const dpr = window.devicePixelRatio || 1;
    if (canvas.width !== Math.round(cssW * dpr) || canvas.height !== Math.round(cssH * dpr)) {
      canvas.width = Math.round(cssW * dpr);
      canvas.height = Math.round(cssH * dpr);
    }
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    // 用当前主题色,深浅色都自适应。
    const rootStyle = getComputedStyle(document.documentElement);
    const fg = rootStyle.getPropertyValue("--fg").trim() || "#ddd";
    const faint = rootStyle.getPropertyValue("--fg-faint").trim() || "#888";
    const border = rootStyle.getPropertyValue("--border").trim() || "#333";

    const frames = framesRef.current;
    const padL = 46;
    const padR = 12;
    const padT = 12;
    const padB = 20;
    const plotW = cssW - padL - padR;
    const plotH = cssH - padT - padB;
    if (plotW <= 0 || plotH <= 0) return;

    // 坐标框
    ctx.strokeStyle = border;
    ctx.lineWidth = 1;
    ctx.strokeRect(padL, padT, plotW, plotH);

    if (frames.length < 2) {
      ctx.fillStyle = faint;
      ctx.font = "12px system-ui, sans-serif";
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      ctx.fillText("等待数据…(串口每行里的数字会自动画成曲线)", cssW / 2, cssH / 2);
      return;
    }

    // 收集所有曲线名字(保持首次出现顺序)。
    const names: string[] = [];
    for (const f of frames) {
      for (const k of Object.keys(f.values)) {
        if (!names.includes(k)) names.push(k);
      }
    }

    // Y 轴范围:取窗口内所有数值的 min/max,留 8% 余量,常数线也不至于贴边。
    let yMin = Infinity;
    let yMax = -Infinity;
    for (const f of frames) {
      for (const k of names) {
        const v = f.values[k];
        if (typeof v === "number" && isFinite(v)) {
          if (v < yMin) yMin = v;
          if (v > yMax) yMax = v;
        }
      }
    }
    if (!isFinite(yMin) || !isFinite(yMax)) return;
    if (yMin === yMax) {
      yMin -= 1;
      yMax += 1;
    }
    const yPad = (yMax - yMin) * 0.08;
    yMin -= yPad;
    yMax += yPad;

    const x0 = frames[0].i;
    const x1 = frames[frames.length - 1].i;
    const xRange = Math.max(1, x1 - x0);
    const sx = (i: number) => padL + ((i - x0) / xRange) * plotW;
    const sy = (v: number) => padT + (1 - (v - yMin) / (yMax - yMin)) * plotH;

    // Y 刻度 + 横向网格(顶/中/底三条)。
    ctx.font = "10px system-ui, sans-serif";
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    for (const t of [0, 0.5, 1]) {
      const v = yMax - t * (yMax - yMin);
      const yy = padT + t * plotH;
      ctx.fillStyle = faint;
      ctx.fillText(fmtNum(v), padL - 6, yy);
      ctx.strokeStyle = border;
      ctx.globalAlpha = 0.45;
      ctx.beginPath();
      ctx.moveTo(padL, yy);
      ctx.lineTo(padL + plotW, yy);
      ctx.stroke();
      ctx.globalAlpha = 1;
    }

    // 各路折线
    ctx.lineWidth = 1.5;
    ctx.lineJoin = "round";
    for (const name of names) {
      ctx.strokeStyle = colorOf(name);
      ctx.beginPath();
      let started = false;
      for (const f of frames) {
        const v = f.values[name];
        if (typeof v !== "number" || !isFinite(v)) {
          started = false; // 缺值断开,不连过去
          continue;
        }
        const X = sx(f.i);
        const Y = sy(v);
        if (!started) {
          ctx.moveTo(X, Y);
          started = true;
        } else {
          ctx.lineTo(X, Y);
        }
      }
      ctx.stroke();
    }

    // 图例(左上角):色块 + 名字 + 当前值。
    ctx.textAlign = "left";
    ctx.textBaseline = "middle";
    ctx.font = "11px system-ui, sans-serif";
    let lx = padL + 8;
    const ly = padT + 11;
    for (const name of names) {
      const last = lastValue(frames, name);
      const label = last == null ? name : `${name}: ${fmtNum(last)}`;
      ctx.fillStyle = colorOf(name);
      ctx.fillRect(lx, ly - 4, 9, 9);
      lx += 13;
      ctx.fillStyle = fg;
      ctx.fillText(label, lx, ly);
      lx += ctx.measureText(label).width + 16;
    }
  };

  // 数据变了(version 自增)就重画。
  useEffect(() => {
    draw();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version]);

  // 容器尺寸变化(窗口缩放 / 面板首次布局)也要重画。
  useEffect(() => {
    const ro = new ResizeObserver(() => draw());
    if (wrapRef.current) ro.observe(wrapRef.current);
    draw();
    return () => ro.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="serialmon__plot" ref={wrapRef}>
      <canvas ref={canvasRef} />
    </div>
  );
}

// 数值显示:大数不要小数,小数留两位,够看就行。
function fmtNum(v: number): string {
  if (Math.abs(v) >= 1000) return v.toFixed(0);
  if (Math.abs(v) >= 100) return v.toFixed(1);
  return v.toFixed(2);
}

// 取某条曲线最近一个有效值(给图例显示当前值)。
function lastValue(frames: SerialFrame[], name: string): number | null {
  for (let i = frames.length - 1; i >= 0; i--) {
    const v = frames[i].values[name];
    if (typeof v === "number" && isFinite(v)) return v;
  }
  return null;
}
