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
