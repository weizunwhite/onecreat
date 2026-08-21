package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventstream"
)

// AR-R07:慢客户端积压过多状态帧会被断开,但断开只是止损 —— 它并不知道自己错过了
// 什么。错过的可能是 ApprovalRequest(审批永远不出现,agent 卡在没人看得见的提示上)
// 或 TurnDone(UI 永远转圈)。V2 的 sequence 让"有洞"可被发现,但发现之后得有地方
// 对齐,否则这个能力等于没用。
//
// 这一组用例锁住那个"对齐的地方":一份权威快照,以及流上足够客户端判断的元信息。

func newTestServer(t *testing.T) (*httptest.Server, *Broadcaster, *control.Controller) {
	t.Helper()
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Runner: fakeRunner{got: make(chan string, 8)}, Sink: bc})
	srv := httptest.NewServer(New(ctrl, bc).Handler())
	t.Cleanup(srv.Close)
	return srv, bc, ctrl
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// 快照必须**同时**带 transcript 和那些"只在事件里出现过一次"的状态。只给 transcript
// 正是原来 /history 的缺口 —— 客户端拿它对不齐审批和 running。
func TestSnapshotCarriesEverythingNeededToResync(t *testing.T) {
	srv, _, _ := newTestServer(t)
	snap := getJSON(t, srv.URL+"/snapshot")
	for _, key := range []string{
		"schemaVersion", "streamId", "sequence",
		"history", "running", "plan", "pendingApprovals", "pendingAsks",
	} {
		if _, ok := snap[key]; !ok {
			t.Errorf("快照缺少 %q —— 客户端就是靠这些字段回到真相的", key)
		}
	}
	// 空集合必须是 [] 而不是 null,否则 JS 的 for...of 抛 TypeError(E9)。
	for _, key := range []string{"history", "pendingApprovals", "pendingAsks"} {
		if _, ok := snap[key].([]any); !ok {
			t.Errorf("%q 应编码成数组,拿到 %T", key, snap[key])
		}
	}
}

// 快照带的 sequence 必须跟得上已发出的事件 —— 它是"这份状态截止到第几号"的凭据。
func TestSnapshotSequenceTracksTheStream(t *testing.T) {
	srv, bc, _ := newTestServer(t)
	if got := getJSON(t, srv.URL+"/snapshot")["sequence"].(float64); got != 0 {
		t.Fatalf("还没发过事件,sequence 应为 0,拿到 %v", got)
	}
	for i := 0; i < 3; i++ {
		bc.Emit(event.Event{Kind: event.Notice, Text: "x"})
	}
	if got := getJSON(t, srv.URL+"/snapshot")["sequence"].(float64); got != 3 {
		t.Fatalf("发了 3 条事件后 sequence 应为 3,拿到 %v", got)
	}
}

// 开流帧与断流帧的**内容**是协议契约,单独钉死。端到端去逼真实的 SSE 溢出是时序
// 相关的(handler 一直在排空订阅,客户端读不读由 socket 缓冲决定),那样的用例只会
// 变成一个偶尔挂住的测试 —— 不如把契约本身测准。
func TestStreamReadyFrameAnnouncesWhereTheClientJoined(t *testing.T) {
	name, data := parseSSEFrame(t, streamReadyFrame("sess:tab", 42))
	if name != "stream_ready" {
		t.Fatalf("开流第一帧应是 stream_ready,拿到 %q", name)
	}
	var ready struct {
		StreamID string `json:"streamId"`
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(data), &ready); err != nil {
		t.Fatalf("不是合法 JSON:%v(%s)", err, data)
	}
	if ready.StreamID != "sess:tab" || ready.Sequence != 42 {
		t.Fatalf("开流帧内容不对:%+v", ready)
	}
}

func TestStreamResetPointsAtTheAuthoritativeSnapshot(t *testing.T) {
	name, data := parseSSEFrame(t, streamResetFrame(99))
	if name != "stream_reset" {
		t.Fatalf("断流帧应是 stream_reset,拿到 %q", name)
	}
	var reset struct {
		Reason     string `json:"reason"`
		ResyncFrom string `json:"resyncFrom"`
		Sequence   uint64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(data), &reset); err != nil {
		t.Fatalf("不是合法 JSON:%v(%s)", err, data)
	}
	if reset.ResyncFrom != "/snapshot" {
		t.Errorf("必须指向 /snapshot —— /history 只有 transcript,对不齐审批与 running,拿到 %q", reset.ResyncFrom)
	}
	if reset.Sequence != 99 {
		t.Errorf("必须带上断开时的 sequence,拿到 %d", reset.Sequence)
	}
	if reset.Reason == "" {
		t.Error("应说明为什么被断开")
	}
}

// 真的把订阅灌到溢出,确认 handler 会走到断流分支而不是静默继续 —— 这一半在
// eventstream 那层是确定的,不依赖 socket 时序。
func TestOverflowedSubscriberIsDisconnectedNotSilentlyContinued(t *testing.T) {
	_, bc, _ := newTestServer(t)
	sub := bc.Subscribe()
	defer bc.Unsubscribe(sub)
	for i := 0; i < eventstream.DefaultLimits.Durable*2+16; i++ {
		bc.Emit(event.Event{Kind: event.Notice, Text: "state"})
	}
	if !sub.Overflowed() {
		t.Fatal("状态帧灌爆之后订阅应被判定溢出 —— 否则 handler 永远不会告诉客户端要重新对齐")
	}
}

// parseSSEFrame 拆一条 SSE 命名帧。
func parseSSEFrame(t *testing.T, frame string) (name, data string) {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if name == "" || data == "" {
		t.Fatalf("不是一条完整的 SSE 命名帧:%q", frame)
	}
	return name, data
}

// 前端那一半也要有守卫。CI 目前不跑 frontend(复核的 AR-R13),而 AR-R07 的价值恰恰
// 一半在服务端、一半在客户端:服务端把 stream_reset 发出去了,客户端不监听,等于没修。
// 这里用嵌入的 index.html 做最低限度的接线检查 —— 不是渲染测试,是"这几根线还在吗"。
func TestEmbeddedUIWiresResync(t *testing.T) {
	ui := string(indexHTML)
	for _, want := range []string{
		`addEventListener('stream_reset'`, // 服务端说"你落后了"时,客户端得听见
		`addEventListener('stream_ready'`, // 重连后要知道自己从第几号接上
		`fetch('/snapshot')`,              // 对齐必须回到权威状态,不是只重放 transcript
		"pendingApprovals",                // 丢掉的审批要重新显示
		"pendingAsks",
		"sequence", // 断层检测
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("内置 UI 少了 %q —— 服务端把 stream_reset 发出去了,客户端不接就等于没修", want)
		}
	}
	// 首屏与重对齐必须是同一条路径:两份重建逻辑必然漂移。
	if strings.Contains(ui, "fetch('/history')") {
		t.Error("内置 UI 仍在用 /history 重建界面;它只有 transcript,对不齐 running 与待办审批")
	}
}

// 重新对齐期间到达的帧必须缓冲后重放(复核 B2 做端到端用例时查出来的洞)。
//
// `resync()` 是异步的:`fetch('/snapshot')` 在飞的时候 `onmessage` 还在照常渲染,
// 而拿到快照后第一件事是 `renderHistory` —— 它清空整个 log。于是那个窗口里到达的帧
// 被画出来又被抹掉,而快照是更早的一份、并不包含它们。`lastSeq` 还会被回退到快照的
// 序号,所以连断层检测都不会再触发。
//
// 最坏情况正是 AR-R07 要防的那一种:丢掉的是 `turn_done`,快照里 running 仍为 true,
// 之后又没有新事件 —— 界面永远转圈,而且没有任何东西会去纠正它。
//
// 说清楚这条守卫是什么:它是**接线检查**,不是行为测试。这个文件是内嵌的单文件 UI,
// 没有构建步骤也不进 desktop/frontend 那条 CI;真正的行为证明在服务端那侧的
// TestForcedOverflowResyncLeavesNoHoleAndNoDuplicate —— 它证明协议支持「缓冲—取快照
// —按序号丢弃」这套算法,这里则确认客户端确实照这套算法接线了。
func TestEmbeddedUIBuffersFramesWhileResyncing(t *testing.T) {
	ui := string(indexHTML)
	for _, want := range []string{
		"resyncBuf", // 缓冲区本身
		"if(resyncing){ resyncBuf=resyncBuf||[];", // 取快照期间:入缓冲,不渲染
		"resyncBufFull=true",                      // 缓冲满了不静默丢弃
		"resync('对齐期间积压过多')",                      // ……改成再对齐一次
		"buf.forEach(e=>{",                        // 快照应用完之后重放
		"e.sequence<=lastSeq)return;",             // 快照已经包含的丢弃 —— 否则就是重复
		"function applyEvent(e)",                  // 实时与重放共用一条应用路径
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("内置 UI 少了 %q —— 取快照期间到达的状态帧会被 renderHistory 抹掉,而快照里没有它们", want)
		}
	}
	// onmessage 不得再自己写一遍 switch:两份应用逻辑必然漂移。
	if strings.Count(ui, "case 'turn_done':") != 1 {
		t.Error("turn_done 的处理出现了不止一处 —— 实时帧与重放必须共用 applyEvent")
	}
}

// AR-R02 的验收有三句,这是第三句的后半:
//
//	「HTTP 返回明确 409/422;**Desktop/CLI 禁用并显示原因**;后端校验不能依赖 UI。」
//
// 后端已经按 422/409 拒绝了,`/snapshot` 也带上了 `capabilities`。但只到这一步的话,
// 用户仍然会看到一个可点的 "New Session",点下去撞一个 422,而错误只出现在响应体里 ——
// 界面没有任何变化。这不是"体验不够好":用户会以为自己点错了,反复再点。
//
// 所以入口要**禁用并说明原因**。同时必须说清另一半:UI **不是**那道门 —— 后端的
// requireCap 在改任何状态之前独立校验,把这里全部注释掉也不会放行任何操作。
func TestEmbeddedUIDisablesUnsupportedEntries(t *testing.T) {
	ui := string(indexHTML)
	for _, want := range []string{
		"s.capabilities",    // 从权威快照读能力,而不是自己猜
		"applyCapabilities", // 一处集中处理,不是四个按钮各写一遍
		"aria-disabled",     // 真的禁用,不只是变灰
		"is-unsupported",    // 视觉状态
		"title=",            // 说明原因 —— 光禁用等于"莫名其妙点不动"
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("内置 UI 少了 %q —— 做不到的入口仍然可点,用户只能靠撞 422 才知道", want)
		}
	}
	// 四个依赖能力的入口都要挂上:漏一个就是那一个仍然可点。
	for _, id := range []string{"btn-new", "btn-compact", "btn-rewind", "btn-tree"} {
		if !strings.Contains(ui, "'"+id+"'") {
			t.Errorf("能力映射里没有 %s", id)
		}
	}
	// 后端才是门。UI 里不得出现"因为禁用了所以不用校验"这类假设 —— 这条靠的是
	// serve 侧的 422 用例,这里只确认能力表的来源是快照(服务端),不是本地推断。
	if strings.Contains(ui, "capabilities={") && !strings.Contains(ui, "s.capabilities") {
		t.Error("能力表是前端自己编的,不是服务端给的")
	}
}
