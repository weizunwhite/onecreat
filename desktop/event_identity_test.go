package main

// AR-R08.1:事件流的身份是**会话**,不是标签。
//
// 之前 newEventSink 固定 `NewStamper("", tabID)`:`sessionId` 永远是空的,而 `/new`、
// 恢复历史会话、重建 controller 都不换 stamper —— 于是 sequence 是「这个标签自古以来」
// 的计数。客户端既认不出会话切换,也没法拿 sessionId 做关联,V2 信封里那个字段等于摆设。

import (
	"context"
	"path/filepath"
	"testing"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

// sinkOnTab 造一个挂在标签上的 sink,标签的 controller 指向 sessionPath。
func sinkOnTab(t *testing.T, sessionPath string) (*App, *eventSink) {
	t.Helper()
	tabs := newTabManager()
	a := newBareApp(context.Background(), tabs)
	ctrl := control.New(control.Options{SessionDir: filepath.Dir(sessionPath), SessionPath: sessionPath})
	s := newEventSink(context.Background(), a, "t1")
	tabs.Register(&tabRuntime{id: "t1", ctrl: ctrl, sink: s})
	return a, s
}

func wireOf(t *testing.T, s *eventSink, e event.Event) eventwire.Event {
	t.Helper()
	return s.stamperFor(e).Wire(e)
}

// 回合开始时解析会话身份:sessionId 必须是真实会话,而不是空串。
func TestEventEnvelopeCarriesTheRealSessionID(t *testing.T) {
	dir := t.TempDir()
	_, s := sinkOnTab(t, filepath.Join(dir, "sess-a.jsonl"))

	w := wireOf(t, s, event.Event{Kind: event.TurnStarted})
	if w.SessionID != "sess-a" {
		t.Fatalf("sessionId = %q,应当是真实会话 id —— 空串让客户端无法关联会话", w.SessionID)
	}
	if w.TabID != "t1" {
		t.Fatalf("tabId = %q,want t1", w.TabID)
	}
}

// 换会话 = 换一条流:sequence 从头开始,身份跟着变。悄悄沿用旧编号,客户端会以为
// 中间丢了几百条事件(或者更糟:以为没丢)。
func TestSwitchingSessionStartsANewStream(t *testing.T) {
	dir := t.TempDir()
	a, s := sinkOnTab(t, filepath.Join(dir, "sess-a.jsonl"))

	// 第一条会话上跑几条事件,把序号推上去。
	first := wireOf(t, s, event.Event{Kind: event.TurnStarted})
	for i := 0; i < 5; i++ {
		wireOf(t, s, event.Event{Kind: event.Text, Text: "x"})
	}
	if first.Sequence != 1 {
		t.Fatalf("第一条事件的 sequence 应为 1,拿到 %d", first.Sequence)
	}

	// 换会话(/new、resume、rebuild 都是这个效果)。
	a.tabs.Update("t1", func(rt *tabRuntime, _ bool) {
		rt.ctrl = control.New(control.Options{SessionDir: dir, SessionPath: filepath.Join(dir, "sess-b.jsonl")})
	})

	next := wireOf(t, s, event.Event{Kind: event.TurnStarted})
	if next.SessionID != "sess-b" {
		t.Fatalf("换会话后 sessionId = %q,want sess-b", next.SessionID)
	}
	if next.Sequence != 1 {
		t.Fatalf("换会话应当换一条流、序号从 1 开始,拿到 %d —— 沿用旧编号会让客户端误判丢帧", next.Sequence)
	}
	if next.EventID == first.EventID {
		t.Fatal("两条流的 eventId 不该相同")
	}
}

// 同一个会话内,序号必须连续 —— 换流只在会话真的变了的时候发生。
func TestSequenceStaysGapFreeWithinOneSession(t *testing.T) {
	dir := t.TempDir()
	_, s := sinkOnTab(t, filepath.Join(dir, "sess-a.jsonl"))

	var last uint64
	for turn := 0; turn < 3; turn++ {
		w := wireOf(t, s, event.Event{Kind: event.TurnStarted}) // 每轮都会去解析身份
		if w.Sequence != last+1 {
			t.Fatalf("第 %d 轮:sequence = %d,want %d —— 同一会话不该换流", turn, w.Sequence, last+1)
		}
		last = w.Sequence
		for i := 0; i < 3; i++ {
			last = wireOf(t, s, event.Event{Kind: event.Text}).Sequence
		}
	}
}

// 持久化关闭时没有会话文件,身份就是空的 —— 如实反映,不编一个假 id。
func TestNoPersistenceMeansNoSessionID(t *testing.T) {
	tabs := newTabManager()
	a := newBareApp(context.Background(), tabs)
	s := newEventSink(context.Background(), a, "t1")
	tabs.Register(&tabRuntime{id: "t1", ctrl: control.New(control.Options{}), sink: s})

	if w := wireOf(t, s, event.Event{Kind: event.TurnStarted}); w.SessionID != "" {
		t.Fatalf("没有会话文件时 sessionId 应为空,拿到 %q", w.SessionID)
	}
}
