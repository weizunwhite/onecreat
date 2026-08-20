package control

// Submit 是命令路由:一行原始输入进来,决定它是斜杠命令、"#" 记忆速记、还是一轮
// 普通对话(先解析 @ 引用再发)。它是「简单前端只需要一个入口」的那个入口 ——
// HTTP/SSE 服务和桌面都只把用户敲的那行原样交过来。
//
// 它本身不实现任何领域动作:每条分支都落到某个已经存在的原语上(Compact / NewSession /
// memory.QuickAdd / 斜杠分发 / 一轮 turn),这里只负责认出该走哪条。

import (
	"context"
	"log/slog"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/memory"
)

// Submit is the one-call entry for a simple frontend: it takes raw user input
// and does everything — slash-command dispatch, @-reference expansion, plan-mode
// composition — emitting all output as events. The HTTP/SSE server uses this so
// a browser client only POSTs the typed line.
//
// Slash commands route to the matching primitive: /compact and /new run their
// session op and emit a Notice; /mcp__server__prompt and custom /commands
// resolve to a turn; an unknown slash emits a Notice. Anything else is a normal
// turn with its @-references resolved first.
func (c *Controller) Submit(input string) {
	trimmed := strings.TrimSpace(input)
	switch {
	case trimmed == "/compact" || strings.HasPrefix(trimmed, "/compact "):
		focus := strings.TrimSpace(strings.TrimPrefix(trimmed, "/compact"))
		go func() {
			if err := c.Compact(context.Background(), focus); err != nil {
				c.notice("compaction failed: " + err.Error())
			} else {
				c.notice("compacted")
				if err := c.session.Save(); err != nil {
					slog.Warn("controller: snapshot after compact", "err", err)
				}
			}
		}()
	case trimmed == "/new":
		go func() {
			if err := c.NewSession(); err != nil {
				c.notice("new session failed: " + err.Error())
			} else {
				c.notice("new session")
			}
		}()
	case strings.HasPrefix(trimmed, "#"):
		// "#<note>" quick-adds a memory line — same shortcut as the chat TUI, so
		// the desktop and HTTP frontends (which route raw input through Submit)
		// get it for free. It never starts a model turn.
		note := strings.TrimSpace(trimmed[1:])
		if note == "" {
			c.notice("nothing to remember")
			return
		}
		if path, err := c.memory.QuickAdd(memory.ScopeProject, note); err != nil {
			c.notice("memory: " + err.Error())
		} else {
			c.notice("remembered → " + path)
		}
	case strings.HasPrefix(trimmed, "/mcp__"):
		c.turn.Guarded(func(ctx context.Context) error {
			sent, found, err := c.MCPPrompt(ctx, trimmed)
			if err != nil {
				return err
			}
			if !found {
				c.notice("unknown command: " + trimmed)
				return nil
			}
			return c.runTurnWithRaw(ctx, sent, sent)
		})
	case strings.HasPrefix(trimmed, "/"):
		if ref, ok := FileRefLine(trimmed); ok {
			c.runRefTurn(ref)
			return
		}
		// Read-only management verbs (/model /memory /skill /hooks /mcp) emit a
		// listing Notice, so Submit-based frontends (desktop, HTTP) get them with
		// no extra wiring. (The chat TUI handles these itself with richer output.)
		fields := strings.Fields(trimmed)
		switch fields[0] {
		case "/tree":
			c.notice(c.BranchTreeText())
			return
		case "/branch":
			args := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			if turn, name, fromTurn, err := ParseBranchTarget(args); err != nil {
				c.notice(err.Error())
			} else if fromTurn {
				if _, err := c.ForkNamed(turn-1, name); err != nil {
					c.notice(err.Error())
				}
			} else {
				if _, err := c.Branch(name); err != nil {
					c.notice(err.Error())
				}
			}
			return
		case "/switch":
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			if _, err := c.SwitchBranch(ref); err != nil {
				c.notice(err.Error())
			}
			return
		}
		if c.managementNotice(trimmed) {
			return
		}
		// A custom command wins over a skill of the same name; both resolve to a
		// turn. (Built-in slash verbs like /compact are handled above.)
		if sent, ok := c.CustomCommand(trimmed); ok {
			c.turn.Guarded(func(ctx context.Context) error {
				return c.runTurnWithRaw(ctx, sent, sent)
			})
			return
		}
		if sent, ok := c.RunSkill(trimmed); ok {
			c.turn.Guarded(func(ctx context.Context) error {
				return c.runTurnWithRaw(ctx, sent, sent)
			})
			return
		}
		c.notice("unknown command: " + trimmed)
	default:
		c.runRefTurn(input)
	}
}

// runRefTurn resolves a line's @references into a context block and starts a
// turn with it prepended (or the raw line when nothing resolved).
func (c *Controller) runRefTurn(input string) {
	c.turn.Guarded(func(ctx context.Context) error {
		block, errs := c.ResolveRefs(ctx, input)
		for _, e := range errs {
			c.notice(e)
		}
		sent := input
		if block != "" {
			sent = "Referenced context:\n\n" + block + "\n\n" + input
		}
		return c.runTurnWithRaw(ctx, sent, input)
	})
}

// notice emits an informational Notice event.
func (c *Controller) notice(text string) {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text})
}
