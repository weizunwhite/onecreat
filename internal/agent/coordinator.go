package agent

import (
	"context"
	"fmt"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

// Runner carries out one task turn. Both Agent (single model) and Coordinator
// (two-model) satisfy it, so the CLI stays agnostic to which is in use.
type Runner interface {
	Run(ctx context.Context, input string) error
}

// DefaultPlannerPrompt steers the planner toward concise plans, not execution.
// The platform-consistency clause guards a real dogfood failure: a knowledge-base
// snippet about MaixCAM color detection leaked into an ESP32 task and the planner
// produced a fully off-target MaixCAM plan. The planner has no similarity score to
// gate on, so the guard is a hard prompt constraint instead.
const DefaultPlannerPrompt = `You are the planner in a two-model coding agent.
Given a task, produce a concise, ordered plan for the executor model to carry out.
Do not write full implementations or call tools — outline the steps, which files
to touch, and the key decisions. Keep it short and actionable.
If the task or context contains reference snippets, example code, or knowledge-base
material, only rely on a snippet when its hardware platform/board clearly matches the
task's (e.g. both are ESP32). If a snippet's platform differs from the task's — the
task is ESP32 but the snippet is MaixCAM / Raspberry Pi / a different board or
language — ignore it completely and base the plan solely on the user's actual task.`

// Coordinator runs two models in separate sessions to keep each one's prompt
// prefix cache-stable: a low-frequency planner proposes an approach, then the
// executor (a full tool-using Agent) carries it out. The sessions never mix, so
// neither model's prefix is disturbed by the other's turns.
type Coordinator struct {
	planner        provider.Provider
	plannerSess    *Session
	plannerPricing *provider.Pricing
	executor       *Agent
	temperature    float64
	sink           event.Sink
}

// NewCoordinator wires a planner provider (with its own session) to an executor.
// sink receives the planner's phase/text/usage events; the executor emits its
// own events to its own sink (the CLI wires the same sink into both). A nil
// sink is replaced with event.Discard.
func NewCoordinator(planner provider.Provider, plannerSession *Session, plannerPricing *provider.Pricing, executor *Agent, temperature float64, sink event.Sink) *Coordinator {
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	return &Coordinator{
		planner:        planner,
		plannerSess:    plannerSession,
		plannerPricing: plannerPricing,
		executor:       executor,
		temperature:    temperature,
		sink:           sink,
	}
}

// Run plans with the planner model, then hands the plan to the executor.
func (c *Coordinator) Run(ctx context.Context, input string) error {
	c.sink.Emit(event.Event{Kind: event.TurnStarted})
	c.sink.Emit(event.Event{Kind: event.Phase, Text: c.planner.Name() + " · planning"})
	plan, err := c.plan(ctx, input)
	if err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	c.sink.Emit(event.Event{Kind: event.Phase, Text: c.executor.prov.Name() + " · executing"})
	return c.executor.Run(ctx, formatHandoff(input, plan))
}

// plan streams a plan from the planner (no tools) and appends it to the planner
// session, so that session grows prepend-only and stays cache-friendly.
func (c *Coordinator) plan(ctx context.Context, input string) (string, error) {
	// 不先污染 planner session:用「现有消息 + 本轮 user」组请求,成功拿到 assistant 回复
	// 后再把 user+assistant 一起提交;失败则什么都不留——否则流式中途出错会留下孤立 user,
	// 下次 Run 再 Add user → planner 会话出现连续两条 user(E4)。
	userMsg := provider.Message{Role: provider.RoleUser, Content: input}
	reqMessages := append(append([]provider.Message(nil), c.plannerSess.Messages...), userMsg)

	ch, err := c.planner.Stream(ctx, provider.Request{
		Messages:    reqMessages,
		Temperature: c.temperature,
	})
	if err != nil {
		return "", err
	}

	var text strings.Builder
	var usage *provider.Usage
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
			c.sink.Emit(event.Event{Kind: event.Text, Text: chunk.Text})
		case provider.ChunkUsage:
			usage = chunk.Usage
		case provider.ChunkError:
			return "", chunk.Err
		}
	}
	// Closes the planner's raw text block (no markdown redraw) and prints its
	// usage line, mirroring the old Fprintln + printUsage tail.
	c.sink.Emit(event.Event{Kind: event.Usage, Usage: usage, Pricing: c.plannerPricing})

	plan := text.String()
	c.plannerSess.Add(userMsg)
	c.plannerSess.Add(provider.Message{Role: provider.RoleAssistant, Content: plan})
	return plan, nil
}

func formatHandoff(task, plan string) string {
	return fmt.Sprintf("Task: %s\n\nA planner proposed this approach:\n%s\n\nCarry it out, adapting as needed.", task, plan)
}
