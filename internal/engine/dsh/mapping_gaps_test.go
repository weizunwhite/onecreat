package dsh

// AR-R08.2 / AR-R08.3。Map 是个纯函数 —— 这两条不需要一个跑得起来的 dsh,
// 我此前把它们和 AR-R05 一起归进「被协议阻断」是分类错了。

import (
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/event"
)

func rawEvent(t *testing.T, typ string, data any) RawSessionEvent {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return RawSessionEvent{Type: typ, Data: b}
}

// AR-R08.2:结果必须能和它的调用对上号。
//
// 一轮里可以有多个并行工具调用。ToolResult 不带 callId,前端就只能靠「到达顺序」猜 ——
// 而 dsh 的结果本来就可能乱序到达。猜错的后果不是显示错位那么轻:一个失败的结果会被
// 挂到另一个调用的卡片上,用户据此判断哪一步出了问题。
func TestToolResultCarriesItsCallCorrelation(t *testing.T) {
	raw := rawEvent(t, EvToolResult, map[string]any{
		"callId": "call-7",
		"name":   "bash",
		"message": map[string]any{
			"content": []map[string]string{{"type": "text", "text": "ok"}},
		},
	})

	evs := Map(raw)
	if len(evs) != 1 {
		t.Fatalf("事件数 = %d", len(evs))
	}
	if got := evs[0].Tool.ID; got != "call-7" {
		t.Fatalf("ToolResult 的 call ID = %q —— 没有它,结果就对不回是哪一次调用", got)
	}
	if got := evs[0].Tool.Name; got != "bash" {
		t.Fatalf("ToolResult 的工具名 = %q", got)
	}
	if got := evs[0].Tool.Output; got != "ok" {
		t.Fatalf("输出 = %q", got)
	}
}

// AR-R08.3:durable 状态事件不得**默认静默**丢弃。
//
// 原来的 default 分支直接 return nil,连 EvTodoWrite 这个就声明在几行之上的常量都吞掉。
// 静默是这里最坏的形态:用户看着模型写了一份计划,界面上什么都没有,而日志里也没有。
func TestDurableStateEventsAreNotSilentlyDropped(t *testing.T) {
	for _, typ := range []string{EvTodoWrite, "step/start", "step/complete", "compaction/start"} {
		evs := Map(rawEvent(t, typ, map[string]any{"anything": 1}))
		if len(evs) == 0 {
			t.Errorf("%s 被静默丢弃 —— 至少要降级成一条可见的 Notice", typ)
			continue
		}
		if evs[0].Kind != event.Notice {
			continue // 已经有真映射,更好
		}
		if !strings.Contains(evs[0].Text, typ) {
			t.Errorf("%s 的降级通知里没提到是哪个事件:%q", typ, evs[0].Text)
		}
	}
}

// 网关红线不能被上面那条放宽:含真实 provider/model 的两类**必须**继续静默丢弃,
// 绝不能因为「不许静默丢弃」就把它们降级成一条带类型名的通知。
func TestGatewayLeakEventsStayDropped(t *testing.T) {
	for _, typ := range []string{EvRequestHeader, EvRequestContext} {
		if evs := Map(rawEvent(t, typ, map[string]any{"model": "real-model-name"})); len(evs) != 0 {
			t.Fatalf("%s 必须一个事件都不产出,却拿到 %+v", typ, evs)
		}
	}
}

// 完全未知的类型不该往 UI 上灌通知(可能是高频的新增流式事件),但也不能当没看见。
func TestUnknownEventTypeDoesNotSpamTheUI(t *testing.T) {
	if evs := Map(rawEvent(t, "some/brand-new-stream-thing", map[string]any{})); len(evs) != 0 {
		t.Fatalf("未知类型不该产出前端事件,却拿到 %+v", evs)
	}
}
