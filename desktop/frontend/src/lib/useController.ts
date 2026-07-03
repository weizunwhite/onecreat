// useController is the frontend's state machine over the agent's event stream. It
// reduces the flat WireEvent flow (text/reasoning deltas, tool dispatch/result,
// notices, approvals, usage) into a structured transcript the components render,
// and exposes the command surface (send/cancel/approve/…) that calls back into
// the kernel via the bridge. This is the desktop analogue of the chat TUI's
// update loop — same controller, different renderer.

import { useCallback, useEffect, useReducer, useRef } from "react";
import { app, onEvent, onReady } from "./bridge";
import type {
  BalanceInfo,
  ContextInfo,
  EffortInfo,
  HistoryMessage,
  JobView,
  MemoryView,
  Meta,
  QuestionAnswer,
  SessionMeta,
  WireApproval,
  WireAsk,
  WireEvent,
  WireUsage,
} from "./types";

export type ToolStatus = "running" | "done" | "error" | "stopped";

// friendlyTurnError 把内核的死胡同错误文案换成对 B 端客户可操作的提示。旧版网关 token
// 过期时,内核 AuthError 说「在 .env / 环境里更新它,或跑 reasonix setup」—— 走平台账号
// 登录的客户没有 .env token、也没有 setup 向导,这是死路。
// 识别出来换成「登录已过期,请退出后重新登录」(H6 客户端侧;平台侧 TTL/刷新另做)。
function friendlyTurnError(err: string): string {
  if (err.includes("ONECREAT_GATEWAY_TOKEN")) {
    return "登录已过期,请点左下角账号菜单退出后重新登录(长任务超过登录有效期会出现这种情况)。";
  }
  return err;
}

// LiveStream holds the in-flight assistant segment's text/reasoning, kept out of
// `items` so per-token deltas don't rebuild the backlog. It folds back into its
// assistant item on the closing `message` (or at turn end as a fallback).
export type LiveStream = { id: string; text: string; reasoning: string };

export type Item =
  | { kind: "user"; id: string; text: string }
  | { kind: "assistant"; id: string; text: string; reasoning: string; streaming: boolean }
  | { kind: "phase"; id: string; text: string }
  | { kind: "notice"; id: string; level: "info" | "warn"; text: string }
  | {
      kind: "compaction";
      id: string;
      pending: boolean; // true between compaction_started and compaction_done
      trigger: string; // "auto" | "manual"
      messages: number;
      summary: string;
      archive: string;
    }
  | {
      kind: "tool";
      id: string;
      name: string;
      args: string;
      readOnly: boolean;
      status: ToolStatus;
      output?: string;
      error?: string;
      truncated?: boolean;
      parentId?: string; // a sub-agent call nests under the `task` call with this id
    };

interface State {
  items: Item[];
  running: boolean;
  // turnActive is true only while a real model turn is in flight (between
  // turn_started and turn_done). `running` may be set optimistically on send for
  // immediate feedback; turnActive distinguishes that from an actual turn, so a
  // local command that only emits a Notice (e.g. /skill, /compact) clears the
  // optimistic spinner instead of leaving it stuck forever.
  turnActive: boolean;
  approval?: WireApproval;
  ask?: WireAsk;
  usage?: WireUsage;
  context: ContextInfo;
  meta?: Meta;
  // balance is the active provider's wallet readout, refreshed on mount and after
  // each turn; undefined until first fetched, available:false when not configured.
  balance?: BalanceInfo;
  effort?: EffortInfo;
  // jobs are the running background jobs, refreshed on mount, turn end, and on
  // each notice (job start/finish emit notices).
  jobs: JobView[];
  // currentAssistant tracks the in-flight assistant item that text/reasoning
  // deltas accumulate into; cleared at turn boundaries.
  currentAssistant?: string;
  // live is the streaming segment's accumulating text/reasoning, kept out of
  // items so a token only updates this O(1) field, not the whole backlog.
  live?: LiveStream;
  // pendingUser holds a just-sent message whose bubble is deferred until the
  // server's first real packet, so an Esc/Stop before any reply "un-sends" it —
  // restoring the text to the composer with nothing left in the transcript. It's
  // committed by the first packet (or, defensively, at turn end). discardTurn is
  // set on un-send so the cancelled turn's already-buffered events are swallowed
  // until its turn_done settles.
  pendingUser?: string;
  discardTurn?: boolean;
  // sawTurnDone 标记「自本次 reset(切入本标签)以来是否已观察到 turn_done」。切回运行中标签时
  // 事件订阅在 reset 后同步注册,本回合的 turn_done 可能先于 app.Meta() 的 promise 解析到达并
  // 清掉 running;随后陈旧的 meta.running=true 又把 running 置回 true → 后端已 idle 不再发
  // turn_done → spinner 永久转。restoreRunning 用它做门禁(G3)。reset 后为 undefined(falsy)。
  sawTurnDone?: boolean;
  // turnStartAt is the wall-clock ms the current turn began (0 when idle), and
  // turnTokens accumulates the output tokens reported this turn — together they
  // drive the live "thinking… (12s · ↓3.6k tokens)" activity readout. Pure
  // frontend-observed harness state; no model cooperation needed.
  turnStartAt: number;
  turnTokens: number;
  // seq is a monotonic id source so React keys stay stable across re-renders.
  seq: number;
}

const initialState: State = {
  items: [],
  running: false,
  turnActive: false,
  context: { used: 0, window: 0 },
  jobs: [],
  turnStartAt: 0,
  turnTokens: 0,
  seq: 0,
};

type Action =
  | { type: "event"; e: WireEvent }
  | { type: "user"; text: string }
  | { type: "unsend" }
  | { type: "meta"; meta: Meta }
  | { type: "context"; context: ContextInfo }
  | { type: "balance"; balance: BalanceInfo }
  | { type: "effort"; effort: EffortInfo }
  | { type: "jobs"; jobs: JobView[] }
  | { type: "history"; messages: HistoryMessage[] }
  | { type: "restoreRunning"; running: boolean }
  | { type: "local_notice"; level: "info" | "warn"; text: string }
  | { type: "clearApproval" }
  | { type: "clearAsk" }
  | { type: "reset" };

// sameMessage 判断两个 item 是否是"同一条消息"(role + 去空白文本)。History() 只产出
// user/assistant 文本条目,tool/notice 从不出现在 history 里,故永不与之相等——用于 history
// 回填时的重叠消除(G2)。
function sameMessage(a: Item, b: Item): boolean {
  if (a.kind === "user" && b.kind === "user") return a.text.trim() === b.text.trim();
  if (a.kind === "assistant" && b.kind === "assistant") return a.text.trim() === b.text.trim();
  return false;
}

// ensureAssistant returns the items array containing the active assistant item
// (creating one if the turn hasn't produced text yet), its id, and the next seq.
function ensureAssistant(s: State): { items: Item[]; id: string; seq: number } {
  if (s.currentAssistant) {
    const exists = s.items.some((it) => it.id === s.currentAssistant && it.kind === "assistant");
    if (exists) return { items: s.items, id: s.currentAssistant, seq: s.seq };
  }
  const id = `a${s.seq}`;
  const item: Item = { kind: "assistant", id, text: "", reasoning: "", streaming: true };
  return { items: [...s.items, item], id, seq: s.seq + 1 };
}

// flushPendingUser commits the deferred user bubble into the transcript (a no-op
// when none is pending). Called by the first real packet of a turn, and at turn
// end as a fallback so an error-before-reply or empty turn still shows what the
// user sent.
function flushPendingUser(s: State): State {
  if (s.pendingUser === undefined) return s;
  return {
    ...s,
    seq: s.seq + 1,
    items: [...s.items, { kind: "user", id: `u${s.seq}`, text: s.pendingUser }],
    pendingUser: undefined,
  };
}

// REPLY_KINDS are the event kinds that count as the server's first real reply to
// the current turn — the arrival of any of them commits the deferred user bubble.
// This is an allowlist, not a denylist, on purpose: side-channel events that can
// land between submit and the first token — turn_started, and any kind this build
// doesn't recognize such as the async mcp_surface_ready — must NOT commit the
// bubble, or an Esc un-send right after submit would silently fail to restore it.
const REPLY_KINDS: ReadonlySet<string> = new Set([
  "text",
  "reasoning",
  "message",
  "tool_dispatch",
  "tool_result",
  "tool_progress",
  "usage",
  "notice",
  "phase",
  "compaction_started",
  "compaction_done",
  "approval_request",
  "ask_request",
]);

function applyEvent(s: State, e: WireEvent): State {
  // After an un-send, swallow the cancelled turn's still-buffered events so no
  // orphan assistant/tool bubble appears; its turn_done clears the discard.
  if (s.discardTurn) {
    if (e.kind === "turn_done") return { ...s, discardTurn: false, running: false, turnActive: false, currentAssistant: undefined, live: undefined, sawTurnDone: true };
    // 管理命令(#note QuickAdd、/tree /branch /switch 及 managementNotice 类)只发 notice、
    // 永不发 turn_done。没有真回合在飞(!turnActive)时,notice 就是它们的终结事件:必须同样清
    // discardTurn,否则 discardTurn 永久 true、applyEvent 吞光后续事件、send() 把消息全 park 掉,
    // 本标签后续消息静默消失(G1)。真回合撤回后 turnActive 为 true,继续吞到 turn_done,不被
    // mid-turn notice 提前解除(避免退回 0da04238 的孤儿气泡)。
    if (e.kind === "notice" && !s.turnActive) return { ...s, discardTurn: false };
    return s;
  }
  // The first real reply packet means the server replied — commit the deferred
  // user bubble before rendering it. turn_done is handled in its own case.
  if (s.pendingUser !== undefined && REPLY_KINDS.has(e.kind)) {
    s = flushPendingUser(s);
  }
  switch (e.kind) {
    case "turn_started":
      return { ...s, running: true, turnActive: true, currentAssistant: undefined, turnStartAt: Date.now(), turnTokens: 0 };

    case "text":
    case "reasoning": {
      // ensureAssistant appends the placeholder once per segment (items changes
      // then); subsequent tokens only grow `live`, leaving items' ref untouched.
      const { items, id, seq } = ensureAssistant(s);
      const delta = e.text ?? e.reasoning ?? "";
      const base = s.live?.id === id ? s.live : { id, text: "", reasoning: "" };
      const live =
        e.kind === "text" ? { ...base, text: base.text + delta } : { ...base, reasoning: base.reasoning + delta };
      return { ...s, items, live, currentAssistant: id, seq };
    }

    case "message": {
      const { items, id, seq } = ensureAssistant(s);
      const next = items.map((it) =>
        it.kind === "assistant" && it.id === id
          ? {
              ...it,
              text: e.text ?? s.live?.text ?? it.text,
              reasoning: e.reasoning ?? s.live?.reasoning ?? it.reasoning,
              streaming: false,
            }
          : it,
      );
      return { ...s, items: next, live: undefined, currentAssistant: undefined, seq };
    }

    case "tool_dispatch": {
      const t = e.tool;
      if (!t) return s;
      const id = t.id || `tool${s.seq}`;
      // A call streams two dispatches: an early partial one (name only, so the
      // card shows at once) and a full one (with args) when it completes. Merge
      // by id — update the existing card rather than appending a duplicate.
      const idx = s.items.findIndex((it) => it.kind === "tool" && it.id === id);
      if (idx >= 0) {
        const next = [...s.items];
        const it = next[idx];
        if (it.kind === "tool") {
          next[idx] = { ...it, name: t.name, args: t.args ? t.args : it.args, readOnly: t.readOnly };
        }
        return { ...s, items: next };
      }
      const item: Item = {
        kind: "tool",
        id,
        name: t.name,
        args: t.args ?? "",
        readOnly: t.readOnly,
        status: "running",
        parentId: t.parentId,
      };
      return { ...s, seq: s.seq + 1, items: [...s.items, item] };
    }

    case "tool_result": {
      const t = e.tool;
      if (!t) return s;
      const next = [...s.items];
      // Match the dispatched card by id; if the kernel omitted one, fall back to
      // the most recent still-running tool.
      let idx = t.id ? next.findIndex((it) => it.kind === "tool" && it.id === t.id) : -1;
      if (idx < 0) {
        for (let i = next.length - 1; i >= 0; i--) {
          const cand = next[i];
          if (cand.kind === "tool" && cand.status === "running") {
            idx = i;
            break;
          }
        }
      }
      if (idx >= 0) {
        const it = next[idx];
        if (it.kind === "tool") {
          next[idx] = {
            ...it,
            status: t.err ? "error" : "done",
            output: t.output,
            error: t.err,
            truncated: t.truncated,
          };
        }
      }
      return { ...s, items: next };
    }

    case "tool_progress": {
      const t = e.tool;
      if (!t?.id) return s;
      const idx = s.items.findIndex((it) => it.kind === "tool" && it.id === t.id);
      if (idx < 0) return s;
      const next = [...s.items];
      const it = next[idx];
      if (it.kind === "tool") next[idx] = { ...it, output: (it.output ?? "") + (t.output ?? "") };
      return { ...s, items: next };
    }

    case "usage": {
      const used = e.usage && s.context.window ? e.usage.promptTokens : s.context.used;
      // Usage arrives once per model step; sum the output across steps for the
      // turn's running token tally.
      const turnTokens = s.turnTokens + (e.usage?.completionTokens ?? 0);
      return { ...s, usage: e.usage, context: { ...s.context, used }, turnTokens };
    }

    case "notice":
      // A Notice with no real turn in flight means a local command (e.g. /skill,
      // /compact) produced output without starting a turn — clear the optimistic
      // spinner so it doesn't read seconds forever. Mid-turn notices keep running.
      return {
        ...s,
        running: s.turnActive ? s.running : false,
        seq: s.seq + 1,
        items: [...s.items, { kind: "notice", id: `n${s.seq}`, level: e.level ?? "info", text: e.text ?? "" }],
      };

    case "phase":
      return {
        ...s,
        seq: s.seq + 1,
        items: [...s.items, { kind: "phase", id: `p${s.seq}`, text: e.text ?? "" }],
      };

    case "compaction_started":
      // Drop a pending card the moment the summarizer starts, so the user sees
      // the pass is running rather than a frozen window.
      return {
        ...s,
        seq: s.seq + 1,
        items: [
          ...s.items,
          {
            kind: "compaction",
            id: `c${s.seq}`,
            pending: true,
            trigger: e.compaction?.trigger ?? "",
            messages: 0,
            summary: "",
            archive: "",
          },
        ],
      };

    case "compaction_done": {
      const c = e.compaction;
      // An aborted pass (no summary) drops the pending placeholder; the
      // accompanying notice explains why. Otherwise fill the last pending card.
      const idx = [...s.items].reverse().findIndex((it) => it.kind === "compaction" && it.pending);
      const at = idx < 0 ? -1 : s.items.length - 1 - idx;
      if (!c?.summary) {
        const items = at < 0 ? s.items : s.items.filter((_, i) => i !== at);
        return { ...s, running: s.turnActive ? s.running : false, items };
      }
      const filled: Item = {
        kind: "compaction",
        id: at < 0 ? `c${s.seq}` : (s.items[at] as Extract<Item, { kind: "compaction" }>).id,
        pending: false,
        trigger: c.trigger ?? "",
        messages: c.messages ?? 0,
        summary: c.summary,
        archive: c.archive ?? "",
      };
      const items = at < 0 ? [...s.items, filled] : s.items.map((it, i) => (i === at ? filled : it));
      return { ...s, running: s.turnActive ? s.running : false, seq: s.seq + 1, items };
    }

    case "approval_request":
      return { ...s, approval: e.approval };

    case "ask_request":
      return { ...s, ask: e.ask };

    case "turn_done": {
      // A turn that ended while its bubble was still deferred (an error before any
      // reply, or an empty turn) was really sent — commit it so it isn't lost. A
      // user-cancel before any reply takes the un-send path instead (discardTurn).
      if (s.pendingUser !== undefined) s = flushPendingUser(s);
      // The turn is over, so nothing more will arrive: fold any residual live
      // segment (a turn that errored before its closing `message`) back into its
      // item, freeze a streaming assistant, and settle any tool still "running"
      // (e.g. a call interrupted by cancel, which never gets a result) to "stopped".
      const finalized = s.items.map((it) => {
        if (it.kind === "assistant" && s.live && it.id === s.live.id)
          return { ...it, text: s.live.text, reasoning: s.live.reasoning, streaming: false };
        if (it.kind === "assistant" && it.streaming) return { ...it, streaming: false };
        if (it.kind === "tool" && it.status === "running") return { ...it, status: "stopped" as const };
        return it;
      });
      const items: Item[] = e.err
        ? [...finalized, { kind: "notice", id: `e${s.seq}`, level: "warn", text: friendlyTurnError(e.err) }]
        : finalized;
      return { ...s, items, live: undefined, running: false, turnActive: false, currentAssistant: undefined, approval: undefined, ask: undefined, seq: s.seq + 1, sawTurnDone: true };
    }
    // An unrecognized event kind (e.g. one the kernel added but this build's wire
    // map doesn't name yet) must not collapse state to undefined — ignore it.
    default:
      return s;
  }
}

function reducer(s: State, a: Action): State {
  switch (a.type) {
    case "user":
      // Defer the bubble (see pendingUser): it lands in the transcript only once
      // the server replies, so an Esc before then can un-send it cleanly.
      return {
        ...s,
        running: true,
        turnStartAt: Date.now(),
        turnTokens: 0,
        pendingUser: a.text,
        discardTurn: false,
      };
    case "unsend":
      // Esc/Stop before any reply: drop the deferred bubble and mark the turn
      // discarded so its trailing events are swallowed. The composer restores the
      // text from cancel()'s return value.
      return { ...s, pendingUser: undefined, discardTurn: true, running: false, live: undefined };
    case "meta":
      return { ...s, meta: a.meta };
    case "context":
      return { ...s, context: a.context };
    case "balance":
      return { ...s, balance: a.balance };
    case "effort":
      return { ...s, effort: a.effort };
    case "jobs":
      return { ...s, jobs: a.jobs };
    case "restoreRunning":
      // 只「向上」恢复 running(切回正在跑的标签),不从这里强制清 running——避免与
      // 切标签后已流入的 turn_started/turn_done live 事件打架(A3)。若本次加载后已观察到
      // turn_done(回合在 Meta() 解析前就结束、running 已被清),则不得再被陈旧的 meta.running
      // 置回 true——后端已 idle 不会再发 turn_done,否则 spinner 永久转、Composer 永久禁用(G3)。
      return a.running && !s.sawTurnDone ? { ...s, running: true, turnActive: true } : s;
    case "history": {
      // Only user/assistant turns with visible text or assistant reasoning — never
      // the system prompt or tool-result messages.
      const visible = a.messages.filter(
        (m) =>
          (m.role === "user" && m.content.trim() !== "") ||
          (m.role === "assistant" && (m.content.trim() !== "" || (m.reasoning ?? "").trim() !== "")),
      );
      const historyItems: Item[] = visible.map((m, i) =>
        m.role === "user"
          ? { kind: "user", id: `h${i}`, text: m.content }
          : { kind: "assistant", id: `h${i}`, text: m.content, reasoning: m.reasoning ?? "", streaming: false },
      );
      // 切回一个正在跑的标签时,reset 后订阅已先流入 live 事件(items 非空,是【本回合】
      // 正在流式的助手片段)。History() 是过去的回合(含本回合的用户消息)——把它接在前面。
      if (s.items.length > 0) {
        // 重叠消除(G2):若本回合在 History() 解析【之前】就结束并落库,History() 也带回同一条
        // 助手回复,而 live 已把它落成 item → 直接前置会渲染两次。从 history 尾部 vs live 头部找
        // 最大重叠段(role+文本),剔掉 live 中已在 history 的前缀,只保留 live 独有的尾巴(history
        // 快照之后新流入的),保证每条消息恰好一次、历史完整——无论回合先于/晚于 History() 结束。
        let overlap = 0;
        const maxK = Math.min(historyItems.length, s.items.length);
        for (let k = 1; k <= maxK; k++) {
          let matched = true;
          for (let j = 0; j < k; j++) {
            if (!sameMessage(historyItems[historyItems.length - k + j], s.items[j])) {
              matched = false;
              break;
            }
          }
          if (matched) overlap = k;
        }
        return { ...s, items: [...historyItems, ...s.items.slice(overlap)], seq: s.seq + visible.length };
      }
      return { ...s, items: historyItems, seq: s.seq + visible.length };
    }
    case "local_notice":
      return {
        ...s,
        running: false,
        turnActive: false,
        seq: s.seq + 1,
        items: [...s.items, { kind: "notice", id: `n${s.seq}`, level: a.level, text: a.text }],
      };
    case "clearApproval":
      return { ...s, approval: undefined };
    case "clearAsk":
      return { ...s, ask: undefined };
    case "reset":
      // Background jobs and the balance are session-scoped (the controller and its
      // job manager survive a new-session rotation), so carry them across a reset.
      return { ...initialState, meta: s.meta, context: { ...s.context, used: 0 }, balance: s.balance, effort: s.effort, jobs: s.jobs };
    case "event":
      return applyEvent(s, a.e);
    default:
      return s;
  }
}

// useController 绑定到一个标签(tabId):订阅该标签的事件通道、加载它的会话。切换
// tabId(用户切标签)时会重置 transcript 并重新加载目标标签的 Meta/History。命令
// (send/cancel/approve…)打到后端「当前活动标签」——App 在切换时已调 SetActiveTab,
// 所以 activeTabId 与后端活动标签一致,命令自然作用到本标签。
export function useController(tabId: string) {
  const [state, dispatch] = useReducer(reducer, initialState);
  // A live mirror of state for event-handler callbacks (useCallback closures are
  // pinned to the first render); cancel() reads it to decide un-send vs. cancel.
  const stateRef = useRef(state);
  stateRef.current = state;
  // A message the user re-sent while a just-un-sent turn was still winding down on
  // the backend (c.running not yet cleared). Submitting it immediately would hit
  // runGuarded's "a turn is already in flight" guard and be silently dropped, so we
  // park it here and flush it on the cancelled turn's turn_done (backend now idle).
  const pendingResendRef = useRef<{ display: string; submit: string } | null>(null);
  const doSubmitRef = useRef<(display: string, submit: string) => void>(() => {});

  // loadSessionData fetches Meta, ContextUsage, and History — called on mount
  // and again when agent:ready fires (boot.Build completed in the background).
  const loadSessionData = useCallback(async () => {
    try {
      const meta = await app.Meta();
      dispatch({ type: "meta", meta });
      // 切回一个仍在跑的标签:从后端真值恢复 running,否则 reset 后 running=false 会让
      // UI 误显示空闲(无 spinner、Composer 可再次提交、按钮守卫失效)(A3)。
      dispatch({ type: "restoreRunning", running: !!meta.running });
      dispatch({ type: "context", context: await app.ContextUsage() });
      dispatch({ type: "effort", effort: await app.Effort() });
      const history = await app.History();
      if (history && history.length) dispatch({ type: "history", messages: history });
      // 切回标签:后台审批/ask 事件在该标签无人订阅时已发出,重订阅不会重放——主动补查
      // 当前未应答的提示并补显弹窗(promptMu 保证至多一个未应答)(A2)。
      const pending = await app.PendingPrompts(tabId);
      if (pending.approvals?.length) {
        dispatch({ type: "event", e: { kind: "approval_request", approval: pending.approvals[0] } as WireEvent });
      } else if (pending.asks?.length) {
        dispatch({ type: "event", e: { kind: "ask_request", ask: pending.asks[0] } as WireEvent });
      }
    } catch {
      // Bound methods unavailable (pre-startup / build error) — ignore; Meta's
      // startupErr surfaces the reason once it's reachable.
    }
  }, [tabId]);

  useEffect(() => {
    // 切到这个标签时,先清掉上一个标签的 transcript,再订阅本标签通道并加载它的会话。
    dispatch({ type: "reset" });
    // 丢掉可能残留的待发消息:它属于上一个标签,绝不能被本标签的 turn_done 冲走(串台)。
    pendingResendRef.current = null;
    const off = onEvent(tabId, (e) => {
      dispatch({ type: "event", e });
      // The gauge's denominator (window) and post-turn prompt size come from the
      // kernel, not the stream — refresh once a turn settles. The wallet balance
      // moves with spend, so refresh it on the same boundary.
      if (e.kind === "turn_done") {
        app
          .ContextUsage()
          .then((context) => dispatch({ type: "context", context }))
          .catch(() => {});
        app
          .Balance()
          .then((balance) => dispatch({ type: "balance", balance }))
          .catch(() => {});
        app
          .Effort()
          .then((effort) => dispatch({ type: "effort", effort }))
          .catch(() => {});
        // A resend parked during the un-send window: the cancelled turn is now
        // fully done (backend running cleared), so submit it for real. Use the
        // unguarded doSubmit — the guard's discardTurn state hasn't re-rendered yet.
        const queued = pendingResendRef.current;
        if (queued) {
          pendingResendRef.current = null;
          doSubmitRef.current(queued.display, queued.submit);
        }
      }
      // Background jobs start/finish via notices and bound around a turn, so
      // refresh the running set on both — keeps the status-bar count live.
      if (e.kind === "turn_done" || e.kind === "notice") {
        app
          .Jobs()
          .then((jobs) => dispatch({ type: "jobs", jobs }))
          .catch(() => {});
      }
    });

    // When boot.Build completes asynchronously, the Go side emits agent:ready.
    // Re-fetch session data so the UI reflects the now-available controller.
    const offReady = onReady(tabId, () => {
      void loadSessionData();
      app
        .Balance()
        .then((balance) => dispatch({ type: "balance", balance }))
        .catch(() => {});
      app
        .Jobs()
        .then((jobs) => dispatch({ type: "jobs", jobs }))
        .catch(() => {});
      app
        .Effort()
        .then((effort) => dispatch({ type: "effort", effort }))
        .catch(() => {});
    });

    // Initial load — picks up the pre-build Meta (ready=false) and, if the
    // build already finished, the full session.
    void loadSessionData();

    // Wallet balance is a network call — fetch it independently so it never delays
    // the transcript/meta load (and is a no-op readout when not configured).
    app
      .Balance()
      .then((balance) => dispatch({ type: "balance", balance }))
      .catch(() => {});
    app
      .Effort()
      .then((effort) => dispatch({ type: "effort", effort }))
      .catch(() => {});
    app
      .Jobs()
      .then((jobs) => dispatch({ type: "jobs", jobs }))
      .catch(() => {});

    return () => {
      off();
      offReady();
    };
  }, [loadSessionData, tabId]);

  // doSubmit renders the optimistic bubble and fires the request unconditionally.
  const doSubmit = useCallback((displayText: string, submitText: string) => {
    dispatch({ type: "user", text: displayText });
    const display = displayText.trim();
    const submit = submitText.trim();
    const call = display !== submit ? app.SubmitDisplay(display, submit) : app.Submit(submit);
    call.catch(() => {});
  }, []);
  doSubmitRef.current = doSubmit;

  const send = useCallback(
    (displayText: string, submitText = displayText) => {
      // 只在有【真回合】在飞时(turnActive)才 park:那时后端 c.running 仍为 true,立刻重发会被
      // runGuarded 的"a turn is already in flight"丢弃(0da04238 的防丢),park 后由该回合的
      // turn_done 冲刷。管理命令(只发 notice、无 turn_done)或后端已 idle 时 turnActive 为 false:
      // 直接 doSubmit 自愈——doSubmit 的 user action 会清 discardTurn,不会像旧代码那样永久卡死
      // (G1)。注:unsend reducer 已把 running 置 false,所以这里必须用 turnActive 而非 running。
      if (stateRef.current.discardTurn && stateRef.current.turnActive) {
        pendingResendRef.current = { display: displayText, submit: submitText };
        return;
      }
      doSubmit(displayText, submitText);
    },
    [doSubmit],
  );

  const notice = useCallback((text: string, level: "info" | "warn" = "info") => {
    dispatch({ type: "local_notice", level, text });
  }, []);

  // cancel aborts the in-flight turn. If the server hasn't replied yet (the user
  // bubble is still deferred), it instead "un-sends" the message and returns its
  // text so the composer can restore it; otherwise it returns undefined.
  const cancel = useCallback((): string | undefined => {
    const cur = stateRef.current;
    if (cur.running && cur.pendingUser !== undefined) {
      const text = cur.pendingUser;
      dispatch({ type: "unsend" });
      app.Cancel().catch(() => {});
      return text;
    }
    app.Cancel().catch(() => {});
    return undefined;
  }, []);

  // 审批/问答/门控都按 tabId 路由到「事件来源标签」的 controller——后台标签的审批必须
  // 答到它自己的 controller,而不是当前活动标签(A2/A8)。
  const approve = useCallback((id: string, allow: boolean, session: boolean) => {
    dispatch({ type: "clearApproval" });
    app.Approve(tabId, id, allow, session).catch(() => {});
  }, [tabId]);

  // answerQuestion resolves an ask_request with the user's per-question picks.
  const answerQuestion = useCallback((id: string, answers: QuestionAnswer[]) => {
    dispatch({ type: "clearAsk" });
    app.AnswerQuestion(tabId, id, answers).catch(() => {});
  }, [tabId]);

  const setPlan = useCallback((on: boolean) => {
    app.SetPlanMode(tabId, on).catch(() => {});
  }, [tabId]);

  // setCoach 设置会话级协作模式 persona(空串=默认)。
  const setCoach = useCallback((preamble: string) => {
    app.SetCoachMode(tabId, preamble).catch(() => {});
  }, [tabId]);

  // setBypass toggles YOLO mode (auto-approve every tool call this session).
  const setBypass = useCallback((on: boolean) => {
    app.SetBypass(tabId, on).catch(() => {});
  }, [tabId]);

  const newSession = useCallback(async () => {
    await app.NewSession().catch(() => {});
    dispatch({ type: "reset" });
  }, []);

  // Session history: list saved sessions (the panel fetches on open), and resume
  // one — the model/folder are unchanged, only the transcript is swapped.
  const listSessions = useCallback((): Promise<SessionMeta[]> => {
    return app.ListSessions().catch(() => []);
  }, []);

  const resumeSession = useCallback(async (path: string) => {
    const messages = await app.ResumeSession(path).catch(() => [] as HistoryMessage[]);
    dispatch({ type: "reset" });
    if (messages.length) dispatch({ type: "history", messages });
    app.ContextUsage().then((context) => dispatch({ type: "context", context })).catch(() => {});
  }, []);

  const previewSession = useCallback((path: string): Promise<HistoryMessage[]> => {
    return app.PreviewSession(path).catch(() => []);
  }, []);

  // Manage saved sessions: delete one, or give it a custom name (""=clear). Both
  // only touch on-disk state; the caller re-fetches the list to reflect the change.
  const deleteSession = useCallback((path: string) => {
    return app.DeleteSession(path).catch(() => {});
  }, []);

  const renameSession = useCallback((path: string, title: string) => {
    return app.RenameSession(path, title).catch(() => {});
  }, []);

  // refreshMeta re-pulls the model label, gauge, and cwd — used by the Settings
  // panel after a change that rebuilds the controller (model/provider/sandbox/…).
  const refreshMeta = useCallback(async () => {
    try {
      dispatch({ type: "meta", meta: await app.Meta() });
      dispatch({ type: "context", context: await app.ContextUsage() });
      dispatch({ type: "effort", effort: await app.Effort() });
    } catch {
      /* ignore */
    }
  }, []);

  const refreshWorkspaceState = useCallback(async (path: string): Promise<string> => {
    if (path) {
      dispatch({ type: "reset" });
      try {
        dispatch({ type: "meta", meta: await app.Meta() });
        dispatch({ type: "context", context: await app.ContextUsage() });
        dispatch({ type: "effort", effort: await app.Effort() });
      } catch {
        /* ignore */
      }
    }
    return path;
  }, []);

  // Workspace: open a folder chooser and switch to that project. On a pick the
  // backend rebuilds the controller (new model/config) with a fresh session, so
  // reset and refresh meta/context. Returns the chosen path ("" if cancelled).
  // doSwitchWorkspace 切到指定文件夹,失败时把后端原因显示出来(而不是静默吞掉)——
  // 例如「开着多个任务标签时不允许切文件夹」这种守卫,吞掉就表现为「选了没反应/选不了」。
  const doSwitchWorkspace = useCallback(
    async (path: string): Promise<string> => {
      try {
        const next = await app.SwitchWorkspace(path);
        return refreshWorkspaceState(next);
      } catch (e) {
        notice("切换文件夹失败:" + String((e as Error)?.message ?? e), "warn");
        return refreshWorkspaceState("");
      }
    },
    [refreshWorkspaceState, notice],
  );

  // 注:文件夹选择 + 多标签确认/关闭都在 App.tsx 的 switchFolder 里处理(那里有 tabs 状态),
  // 这里只暴露纯粹的「切到指定目录」。
  const switchWorkspace = useCallback(
    (path: string): Promise<string> => doSwitchWorkspace(path),
    [doSwitchWorkspace],
  );

  const compact = useCallback(() => {
    app.Compact().catch(() => {});
  }, []);

  // setModel switches the active model (the backend carries the conversation into
  // the new model's session); refresh the header/gauge to reflect the new label.
  const setModel = useCallback(async (name: string) => {
    await app.SetModel(name).catch(() => {});
    try {
      dispatch({ type: "meta", meta: await app.Meta() });
      dispatch({ type: "context", context: await app.ContextUsage() });
      dispatch({ type: "effort", effort: await app.Effort() });
    } catch {
      /* ignore */
    }
  }, []);

  const setEffort = useCallback(async (level: string) => {
    await app.SetEffort(level).catch(() => {});
    try {
      dispatch({ type: "meta", meta: await app.Meta() });
      dispatch({ type: "context", context: await app.ContextUsage() });
      dispatch({ type: "effort", effort: await app.Effort() });
    } catch {
      /* ignore */
    }
  }, []);

  // Memory panel actions. fetchMemory re-reads the loaded snapshot; remember and
  // saveDoc mutate then return so the caller can re-fetch to reflect the change.
  const fetchMemory = useCallback((): Promise<MemoryView> => {
    return app.Memory().catch(
      () => ({ docs: [], facts: [], scopes: [], storeDir: "", available: false }),
    );
  }, []);

  const remember = useCallback(async (scope: string, note: string) => {
    await app.Remember(scope, note).catch(() => {});
  }, []);

  const forget = useCallback(async (name: string) => {
    await app.Forget(name).catch(() => {});
  }, []);

  const saveDoc = useCallback(async (path: string, body: string) => {
    await app.SaveDoc(path, body).catch(() => {});
  }, []);

  // rewind restores the session to the start of a turn (scope "code" |
  // "conversation" | "both"), then reloads the transcript from the truncated
  // history so the view reflects the rewound state (unlike the CLI, the desktop
  // can re-render).
  const rewind = useCallback(async (turn: number, scope: string) => {
    // "fork" branches into a new session; "summ-*" compress the log; the rest
    // restore in place. All keep code intact (except the code/both restores).
    if (scope === "fork") {
      await app.Fork(turn).catch(() => {});
    } else if (scope === "summ-from") {
      await app.SummarizeFrom(turn).catch(() => {});
    } else if (scope === "summ-upto") {
      await app.SummarizeUpTo(turn).catch(() => {});
    } else {
      await app.Rewind(turn, scope).catch(() => {});
    }
    const messages = await app.History().catch(() => [] as HistoryMessage[]);
    dispatch({ type: "reset" });
    if (messages.length) dispatch({ type: "history", messages });
    app
      .ContextUsage()
      .then((context) => dispatch({ type: "context", context }))
      .catch(() => {});
  }, []);

  return {
    state,
    send,
    notice,
    cancel,
    approve,
    answerQuestion,
    setPlan,
    setCoach,
    setBypass,
    newSession,
    listSessions,
    resumeSession,
    previewSession,
    deleteSession,
    renameSession,
    refreshMeta,
    switchWorkspace,
    compact,
    rewind,
    setModel,
    setEffort,
    fetchMemory,
    remember,
    forget,
    saveDoc,
  };
}
