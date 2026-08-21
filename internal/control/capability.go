package control

// 引擎能力的**执行**(AR-R02)。
//
// Plan 12 让引擎声明自己支持什么,但只是声明:`Fork`/`Branch`/`Rewind`/`NewSession`
// 这些原语照旧直接重写 native executor 的 `agent.Session`。对一个会话日志不在
// OneCreat 这边的引擎(dsh 的 transcript 在 sidecar 里),那意味着:本地影子会话被
// 改了、前端收到"成功"、而引擎那边的上下文纹丝不动。用户看到的成功是假的。
//
// 声明而不执行,比不声明更糟 —— 它让人以为这件事已经被处理了。
//
// 这里的规矩只有一条:**在任何状态被修改之前**检查,失败就返回类型化错误。半路
// 失败会留下"native 影子改了一半、引擎一无所知"的局面,那是最难查的一类 bug。

import (
	"reasonix/internal/engine"
	"reasonix/internal/event"
)

// Supports 报告当前引擎是否支持某项能力。前端用它来隐藏/禁用入口 —— 但后端仍然
// 独立校验,UI 不是安全边界。
func (c *Controller) Supports(cap engine.Capability) bool {
	return engine.Supports(c.engine, cap)
}

// EngineName 返回当前引擎的名字("native" / "dsh" / …),用于错误信息与会话记录。
func (c *Controller) EngineName() string { return engine.NameOf(c.engine) }

// requireCap 是所有会话操作的第一道关:不支持就返回 *engine.UnsupportedError,
// 且**保证还没有碰过任何状态**。
func (c *Controller) requireCap(operation string, cap engine.Capability) error {
	if c.engine == nil {
		return nil // 未装配引擎(部分测试路径):维持既有行为,不在这里新增失败模式
	}
	return engine.Require(c.engine, operation, cap)
}

// noticeUnsupported 把"这个引擎干不了这件事"作为 Notice 发给前端。用于那些没有
// 错误返回值的入口 —— 静默不做事正是本次要消灭的形态。
func (c *Controller) noticeUnsupported(err error) {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: err.Error()})
}
