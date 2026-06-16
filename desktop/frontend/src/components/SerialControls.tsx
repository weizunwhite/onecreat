import { useEffect, useRef, useState } from "react";
import { Plus, Settings2, Trash2 } from "lucide-react";

// 串口交互控件(Phase 3):拖滑块 / 点按钮,直接把变量发给开发板。
// 比 Arduino 串口监视器只能手敲指令方便 —— 调灯亮度、舵机角度、阈值,拖一下就发。
// - 滑块:把当前值套进「指令模板」({v} 代表数值)发出,例如模板 "servo:{v}" → "servo:90"。
// - 按钮:点一下发一条固定指令,例如 "LED:1"。
// 控件定义存 localStorage,关掉面板下次还在;学生可自己加/删/改。

type SliderControl = {
  id: string;
  type: "slider";
  label: string;
  min: number;
  max: number;
  step: number;
  value: number;
  template: string; // 含 {v}
};
type ButtonControl = {
  id: string;
  type: "button";
  label: string;
  command: string;
};
type Control = SliderControl | ButtonControl;

const STORE_KEY = "serialmon.controls.v1";

// 默认给三个示例控件,学生一打开就有得玩,再照着改。
const DEFAULT_CONTROLS: Control[] = [
  { id: "demo-slider", type: "slider", label: "数值", min: 0, max: 255, step: 1, value: 0, template: "{v}" },
  { id: "demo-on", type: "button", label: "开灯", command: "LED:1" },
  { id: "demo-off", type: "button", label: "关灯", command: "LED:0" },
];

function loadControls(): Control[] {
  try {
    const raw = localStorage.getItem(STORE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Control[];
      if (Array.isArray(parsed)) return parsed;
    }
  } catch {
    /* 坏数据就用默认 */
  }
  return DEFAULT_CONTROLS;
}

function newId(): string {
  return "c" + Math.random().toString(36).slice(2, 8);
}

export function SerialControls({
  disabled,
  onSend,
}: {
  disabled: boolean;
  onSend: (text: string, opts?: { silent?: boolean }) => void;
}) {
  const [controls, setControls] = useState<Control[]>(loadControls);
  const [editing, setEditing] = useState(false);
  // 滑块拖动节流:记录每个控件上次发送时刻 + 一个补发定时器,避免拖动刷爆串口。
  const lastSentRef = useRef<Map<string, number>>(new Map());
  const trailingRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());

  useEffect(() => {
    try {
      localStorage.setItem(STORE_KEY, JSON.stringify(controls));
    } catch {
      /* 存不了就算了,不影响使用 */
    }
  }, [controls]);

  const patch = (id: string, next: Partial<SliderControl> & Partial<ButtonControl>) =>
    setControls((cs) => cs.map((x) => (x.id === id ? ({ ...x, ...next } as Control) : x)));
  const remove = (id: string) => setControls((cs) => cs.filter((x) => x.id !== id));
  const addSlider = () =>
    setControls((cs) => [
      ...cs,
      { id: newId(), type: "slider", label: "新滑块", min: 0, max: 255, step: 1, value: 0, template: "{v}" },
    ]);
  const addButton = () => setControls((cs) => [...cs, { id: newId(), type: "button", label: "新按钮", command: "CMD" }]);

  // 把滑块当前值套进模板:有 {v} 就替换,没有就直接拼在后面。
  const fmtCmd = (c: SliderControl, value: number): string =>
    c.template.includes("{v}") ? c.template.split("{v}").join(String(value)) : `${c.template}${value}`;

  // 滑块拖动:先更新显示,再节流发送(~60ms 一次 + 收尾补发),拖动不回显避免刷屏。
  const onSlide = (c: SliderControl, value: number) => {
    patch(c.id, { value });
    if (disabled) return;
    const cmd = fmtCmd(c, value);
    const now = Date.now();
    const last = lastSentRef.current.get(c.id) ?? 0;
    const prev = trailingRef.current.get(c.id);
    if (prev) clearTimeout(prev);
    if (now - last >= 60) {
      lastSentRef.current.set(c.id, now);
      onSend(cmd, { silent: true });
    } else {
      trailingRef.current.set(
        c.id,
        setTimeout(() => {
          lastSentRef.current.set(c.id, Date.now());
          onSend(cmd, { silent: true });
        }, 60),
      );
    }
  };

  return (
    <div className="serialmon__controls">
      <div className="serialmon__controls-head">
        <span className="serialmon__controls-title">交互控件</span>
        <small>拖滑块 / 点按钮 → 实时发给开发板</small>
        <div className="serialmon__controls-spacer" />
        <button className="chip chip--sm" onClick={() => setEditing((v) => !v)} title={editing ? "完成编辑" : "添加 / 修改 / 删除控件"}>
          <Settings2 size={12} /> {editing ? "完成" : "编辑"}
        </button>
      </div>

      <div className="serialmon__controls-grid">
        {controls.map((c) => {
          if (editing) {
            return (
              <div className="serialmon__ctrl serialmon__ctrl--edit" key={c.id}>
                <div className="serialmon__ctrl-editrow">
                  <input
                    className="serialmon__ctrl-name"
                    value={c.label}
                    onChange={(e) => patch(c.id, { label: e.target.value })}
                    placeholder="名称"
                  />
                  <button className="serialmon__ctrl-del" onClick={() => remove(c.id)} title="删除这个控件">
                    <Trash2 size={13} />
                  </button>
                </div>
                {c.type === "slider" ? (
                  <div className="serialmon__ctrl-fields">
                    <label>
                      最小
                      <input type="number" value={c.min} onChange={(e) => patch(c.id, { min: Number(e.target.value) })} />
                    </label>
                    <label>
                      最大
                      <input type="number" value={c.max} onChange={(e) => patch(c.id, { max: Number(e.target.value) })} />
                    </label>
                    <label className="serialmon__ctrl-tmpl">
                      指令模板
                      <input
                        value={c.template}
                        onChange={(e) => patch(c.id, { template: e.target.value })}
                        placeholder="{v} 代表数值，如 servo:{v}"
                      />
                    </label>
                  </div>
                ) : (
                  <label className="serialmon__ctrl-fields serialmon__ctrl-tmpl">
                    指令
                    <input value={c.command} onChange={(e) => patch(c.id, { command: e.target.value })} placeholder="如 LED:1" />
                  </label>
                )}
              </div>
            );
          }
          if (c.type === "slider") {
            return (
              <div className="serialmon__ctrl serialmon__ctrl--slider" key={c.id}>
                <div className="serialmon__ctrl-top">
                  <span>{c.label}</span>
                  <strong>{c.value}</strong>
                </div>
                <input
                  type="range"
                  min={c.min}
                  max={c.max}
                  step={c.step}
                  value={c.value}
                  disabled={disabled}
                  onChange={(e) => onSlide(c, Number(e.target.value))}
                />
                <div className="serialmon__ctrl-sub">
                  发送 <code>{fmtCmd(c, c.value)}</code>
                </div>
              </div>
            );
          }
          return (
            <button
              className="serialmon__ctrl serialmon__ctrl--btn"
              key={c.id}
              disabled={disabled}
              onClick={() => onSend(c.command)}
              title={`发送 ${c.command}`}
            >
              {c.label}
            </button>
          );
        })}
      </div>

      {editing && (
        <div className="serialmon__controls-add">
          <button className="chip chip--sm" onClick={addSlider}>
            <Plus size={12} /> 加滑块
          </button>
          <button className="chip chip--sm" onClick={addButton}>
            <Plus size={12} /> 加按钮
          </button>
          <small>提示:滑块「指令模板」里用 {"{v}"} 代表当前数值,例如 <code>servo:{"{v}"}</code> 拖到 90 就发 <code>servo:90</code>。</small>
        </div>
      )}
    </div>
  );
}
