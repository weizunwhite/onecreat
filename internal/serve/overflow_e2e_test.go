package serve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventstream"
)

// 强制 overflow 的端到端用例(复核 B2)。
//
// 之前这一条只在 eventstream / handler 两层各测了一半:订阅会被判定溢出、handler
// 会走断流分支。**没有测的是那之后**——客户端重连、取权威快照、再接上流,最终看到的
// 状态是否既没有洞也没有重复。而 AR-R07 的全部意义就在那之后:断流只是止损。
//
// 这里走真 socket:一个真的不读的 HTTP 客户端把 handler 的写阻塞住,积压灌爆状态帧
// 上限,然后按客户端应有的算法重新对齐,逐条核对序号。

// sseFrame 是流上的一帧:命名帧带 name,数据帧只有 data。
type sseFrame struct {
	name string
	data string
}

// readFrames 从 SSE 流里读帧,直到 stop 返回 true 或超时。
func readFrames(t *testing.T, br *bufio.Reader, stop func(sseFrame) bool, deadline time.Duration) []sseFrame {
	t.Helper()
	type res struct {
		fs  []sseFrame
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var out []sseFrame
		var cur sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- res{out, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if cur.data == "" {
					continue // 心跳注释等
				}
				out = append(out, cur)
				if stop != nil && stop(cur) {
					ch <- res{out, nil}
					return
				}
				cur = sseFrame{}
			}
		}
	}()
	select {
	case r := <-ch:
		return r.fs
	case <-time.After(deadline):
		t.Fatalf("等待 SSE 帧超时(%s)", deadline)
		return nil
	}
}

// frameReader 在后台持续读流,把帧送进 channel。测试因此可以「发一条、等一条」,
// 而不必猜服务端什么时候写完。
type frameReader struct {
	ch   chan sseFrame
	done chan struct{}
}

func newFrameReader(br *bufio.Reader) *frameReader {
	r := &frameReader{ch: make(chan sseFrame, 256), done: make(chan struct{})}
	go func() {
		defer close(r.ch)
		var cur sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if cur.data == "" {
					continue
				}
				select {
				case r.ch <- cur:
				case <-r.done:
					return
				}
				cur = sseFrame{}
			}
		}
	}()
	return r
}

func (r *frameReader) next(d time.Duration) (sseFrame, bool) {
	select {
	case f, ok := <-r.ch:
		return f, ok
	case <-time.After(d):
		return sseFrame{}, false
	}
}

func (r *frameReader) stop() { close(r.done) }

// envelope 只取核对一致性需要的那几个字段。
type envelope struct {
	EventID  string `json:"eventId"`
	Sequence uint64 `json:"sequence"`
	Durable  bool   `json:"durable"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

func decodeEnvelope(t *testing.T, data string) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatalf("解析事件信封失败:%v(%q)", err, data)
	}
	return e
}

// TestForcedOverflowResyncLeavesNoHoleAndNoDuplicate 是 B2 的验收:强制溢出之后,
// 客户端按「先接流、缓冲、再取快照、丢弃 ≤ 快照序号的帧」重新对齐,最终视图里每个
// 状态事件恰好出现一次。
//
// 为什么必须真起 socket:溢出的前提是 handler 写不动了。handler 会把队列里的帧尽快
// 写出去,只有当客户端不读、内核缓冲写满、`Fprintf` 真的阻塞住,积压才会累积到上限。
// 这一段没法用 in-process 的 ResponseRecorder 复现——它永远不阻塞。
func TestForcedOverflowResyncLeavesNoHoleAndNoDuplicate(t *testing.T) {
	// 状态帧上限压到 8:溢出因此是确定的,而不是要灌 4096 条去赌 socket 缓冲。
	bc := NewBroadcasterWithLimits("", eventstream.Limits{Ephemeral: 4, Durable: 8})
	ctrl := control.New(control.Options{Runner: fakeRunner{got: make(chan string, 8)}, Sink: bc})
	srv := httptest.NewServer(New(ctrl, bc).Handler())
	t.Cleanup(srv.Close)
	base := srv.URL

	// ---- 1. 接上流,读到 stream_ready 之后就**不再读**。
	resp, err := http.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	ready := readFrames(t, br, func(f sseFrame) bool { return f.name == "stream_ready" }, 5*time.Second)
	if last := ready[len(ready)-1]; last.name != "stream_ready" {
		t.Fatalf("第一帧应是 stream_ready,拿到 %q", last.name)
	}

	// ---- 2. 灌爆它。每条 16KiB,共 2000 条 ≈ 32MiB,远超任何 socket 缓冲,
	// handler 必然写阻塞;队列因此涨到上限并把 overflow 锁定(那个标志一旦置起不再复位)。
	const flood = 2000
	big := strings.Repeat("x", 16<<10)
	for i := 0; i < flood; i++ {
		bc.Emit(event.Event{Kind: event.Notice, Text: fmt.Sprintf("flood-%d-%s", i, big)})
	}

	// ---- 3. 恢复读取,必须读到 stream_reset —— 服务端明说"你落后了,去重新对齐"。
	got := readFrames(t, br, func(f sseFrame) bool { return f.name == "stream_reset" }, 30*time.Second)
	last := got[len(got)-1]
	if last.name != "stream_reset" {
		t.Fatalf("灌爆之后应收到 stream_reset,拿到 %q —— 静默继续正是本层不允许的那一种", last.name)
	}
	var reset struct {
		ResyncFrom string `json:"resyncFrom"`
	}
	if err := json.Unmarshal([]byte(last.data), &reset); err != nil {
		t.Fatalf("解析 stream_reset:%v", err)
	}
	if reset.ResyncFrom == "" {
		t.Fatal("stream_reset 没说去哪儿对齐 —— 客户端只知道自己掉队,不知道怎么补")
	}
	resp.Body.Close()

	// ---- 4. 按正确的算法重新对齐:**先接流并缓冲**,再取快照,最后丢掉
	// sequence ≤ 快照序号的帧。顺序反过来(先取快照再接流)会在两步之间漏掉事件,
	// 而那个洞没有任何地方补得回来。
	resp2, err := http.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	br2 := bufio.NewReader(resp2.Body)
	readFrames(t, br2, func(f sseFrame) bool { return f.name == "stream_ready" }, 5*time.Second)

	// 流已经接上了,现在才取快照。
	snap := getJSON(t, base+reset.ResyncFrom)
	snapSeq := uint64(snap["sequence"].(float64))

	// ---- 5. 快照之后再发一批状态事件,逐条核对。
	//
	// 一次性灌 25 条会把**这条新订阅**也撑爆(上限就是 8) —— 那是正确行为,但测的就
	// 不是重新对齐了。所以发一条、等它到,再发下一条:队列深度始终 ≤ 1,溢出不可能
	// 发生,于是「有洞 / 有重复」只可能来自重新对齐本身。
	const after = 25
	reader := newFrameReader(br2)
	defer reader.stop()
	var tail []sseFrame
	for i := 0; i < after; i++ {
		bc.Emit(event.Event{Kind: event.Notice, Text: fmt.Sprintf("after-%d", i)})
		f, ok := reader.next(10 * time.Second)
		if !ok {
			t.Fatalf("第 %d 条状态事件没有到达", i)
		}
		tail = append(tail, f)
	}

	// ---- 6. 核对:丢掉 ≤ 快照序号的帧之后,剩下的必须逐号连续、每号一次。
	seen := map[uint64]int{}
	ids := map[string]int{}
	var seqs []uint64
	for _, f := range tail {
		if f.name != "" {
			continue // stream_ready 之类的命名帧不带业务序号
		}
		e := decodeEnvelope(t, f.data)
		if e.Sequence <= snapSeq {
			continue // 快照已经包含,按算法丢弃
		}
		seen[e.Sequence]++
		ids[e.EventID]++
		seqs = append(seqs, e.Sequence)
	}
	if len(seqs) == 0 {
		t.Fatal("重新对齐后一条事件都没收到")
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("序号 %d 出现 %d 次 —— 重新对齐引入了重复", seq, n)
		}
	}
	for id, n := range ids {
		if n != 1 {
			t.Errorf("事件 %s 出现 %d 次 —— eventId 不再唯一", id, n)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("序号从 %d 跳到 %d —— 重新对齐之后仍有洞", seqs[i-1], seqs[i])
		}
	}
	if seqs[0] != snapSeq+1 {
		t.Fatalf("重新对齐后第一条是 %d,快照截止 %d —— 中间这段谁都没有", seqs[0], snapSeq)
	}
	// 快照之后发的那 25 条,一条不少。
	if got := seqs[len(seqs)-1] - snapSeq; got < after {
		t.Fatalf("快照后发了 %d 条状态事件,只对上 %d 条", after, got)
	}
}
