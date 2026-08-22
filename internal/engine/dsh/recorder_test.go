package dsh

// 测试用的注入桩:把真的 evidence.Ledger / tool.Registry 包成引擎层收的注入函数。
//
// 非测试源码里不允许出现这两个 import(A14 守卫 engine/boundary_test.go 只扫非测试
// 文件),生产代码里的等价物在 internal/boot/dshengine.go 的 dshRecorder /
// dshToolInvoker —— 两边形状必须一致,不然测试测的就不是跑起来的那条路径。

import (
	"context"
	"encoding/json"
	"fmt"

	"reasonix/internal/evidence"
	"reasonix/internal/tool"
)

// testRecorder 把一个真账本包成 Recorder,等价于 boot.dshRecorder。
func testRecorder(led *evidence.Ledger) Recorder {
	return Recorder{
		Reset: led.Reset,
		ToolCall: func(name string, args json.RawMessage, success, readOnly bool) {
			led.Record(evidence.ReceiptFromToolCall(name, args, success, readOnly))
		},
		Todos: func(raw json.RawMessage) {
			var items []evidence.TodoItem
			if err := json.Unmarshal(raw, &items); err != nil {
				return
			}
			led.Record(evidence.Receipt{ToolName: "todo_write", Success: true, Todos: items})
		},
	}
}

// testBuiltinInvoker 在内置工具里按名字找并执行,等价于 boot.dshToolInvoker。
func testBuiltinInvoker(led *evidence.Ledger) ToolInvoker {
	return func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		for _, t := range tool.Builtins() {
			if t.Name() != name {
				continue
			}
			if led != nil {
				ctx = evidence.WithLedger(ctx, led)
			}
			out, err := t.Execute(ctx, args)
			if led != nil {
				led.Record(evidence.ReceiptFromToolCall(name, args, err == nil, true))
			}
			return out, err
		}
		return "", fmt.Errorf("OneCreat 侧没有名为 %s 的工具", name)
	}
}
