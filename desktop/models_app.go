package main

// 模型与「思考强度」的 transport facade:列可选模型、读写 effort。
// 换模型本身是标签运行时的事(tabRuntimeService.SetModel),这里只有视图与配置。

import (
	"fmt"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/event"
)

// ModelInfo is one (provider, model) the bottom switcher can pick. Ref ("provider/
// model") is what SetModel takes; Provider/Model are for display.
type ModelInfo struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Current  bool   `json:"current"`
}

type EffortInfo struct {
	Supported bool     `json:"supported"`
	Current   string   `json:"current"`
	Default   string   `json:"default"`
	Levels    []string `json:"levels"`
}

// Models flattens the configured providers into their (provider, model) pairs —
// the switcher's options — marking the active one. A vendor with a `models` list
// yields one entry per model, all sharing the same endpoint/key. Unconfigured
// providers are skipped. Result is non-nil: the frontend reads .length, so a nil
// slice (JSON null) would crash the switcher on an empty list.
func (a *App) Models() []ModelInfo {
	if gatewayActive() {
		return []ModelInfo{}
	}
	active, _ := a.tabs.View("")
	curModel := active.model
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	out := []ModelInfo{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		for _, m := range p.ModelList() {
			ref := p.Name + "/" + m
			out = append(out, ModelInfo{Ref: ref, Provider: p.Name, Model: m, Current: ref == curModel})
		}
	}
	return out
}

func (a *App) Effort() EffortInfo {
	entry, err := a.currentProviderEntry()
	if err != nil {
		return EffortInfo{Current: "auto", Levels: []string{}}
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		return EffortInfo{Supported: false, Current: "auto", Default: cap.Default, Levels: []string{}}
	}
	return EffortInfo{Supported: true, Current: config.EffortDisplay(entry), Default: cap.Default, Levels: cap.Levels}
}

func (a *App) SetEffort(level string) error {
	ctrl := a.activeCtrl()
	if ctrl != nil && ctrl.Running() {
		return fmt.Errorf("finish or cancel the current turn before changing effort")
	}
	entry, err := a.currentProviderEntry()
	if err != nil {
		return err
	}
	effort, err := config.NormalizeEffort(entry, level)
	if err != nil {
		return err
	}
	return a.applyConfigChange(func(cfg *config.Config) error {
		if _, ok := cfg.Provider(entry.Name); !ok {
			if err := cfg.UpsertProvider(*entry); err != nil {
				return err
			}
		}
		if entry.Kind == "anthropic" && effort != "" && entry.Thinking == "" {
			if err := cfg.SetProviderThinking(entry.Name, "adaptive"); err != nil {
				return err
			}
		}
		return cfg.SetProviderEffort(entry.Name, effort)
	})
}

func (a *App) notice(text string) {
	// 活动标签的 sink 由 tabs 在自己的锁内解析(M3:/effort 命令经 Submit→
	// runEffortCommand 调到这里,与 tab 生命周期方法并发,裸读指针是数据竞态)。
	sink := a.tabs.Sink("")
	if sink != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text})
	}
}

func (a *App) runEffortCommand(input string) {
	entry, err := a.currentProviderEntry()
	if err != nil {
		a.notice("effort: " + err.Error())
		return
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		a.notice(fmt.Sprintf("effort is not configurable for %s", entry.Name))
		return
	}
	args := strings.Fields(input)
	if len(args) < 2 {
		a.notice(fmt.Sprintf("effort for %s: %s (default: %s; options: %s)", entry.Name, config.EffortDisplay(entry), cap.Default, strings.Join(cap.Levels, "|")))
		return
	}
	if len(args) > 2 {
		a.notice("usage: /effort " + strings.Join(cap.Levels, "|"))
		return
	}
	effort, err := config.NormalizeEffort(entry, args[1])
	if err != nil {
		a.notice(err.Error())
		return
	}
	if err := a.SetEffort(args[1]); err != nil {
		a.notice("effort: " + err.Error())
		return
	}
	display := effort
	if display == "" {
		display = "auto"
	}
	a.notice(fmt.Sprintf("effort for %s set to %s", entry.Name, display))
}

func (a *App) currentProviderEntry() (*config.ProviderEntry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	v, _ := a.tabs.View("")
	ref := v.model
	if strings.TrimSpace(ref) == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", ref)
	}
	return entry, nil
}
