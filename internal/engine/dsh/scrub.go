package dsh

import (
	"strings"

	"reasonix/internal/event"
)

// Scrubber 是网关红线的兜底防线:把任何残留的真实 provider/model 名、网关 URL 从
// 流向前端的事件文本里替换成占位符。第一道防线是 mapper.go 直接丢弃 request/header
// 与 request/context;Scrubber 负责万一有别的路径夹带敏感串时兜底。
//
// 敏感串从运行时注入(真实模型名从网关/配置来),绝不硬编码进本仓库。
type Scrubber struct {
	// secrets 是要擦除的敏感子串(真实 provider 路由名、真实 model id、网关 base URL 等)。
	secrets []string
	// mask 是替换后的占位符(如 "onecreat")。
	mask string
}

// NewScrubber 构造脱敏器。空串会被忽略;secrets 为空时 Scrub 是恒等变换。
func NewScrubber(mask string, secrets ...string) *Scrubber {
	if mask == "" {
		mask = "onecreat"
	}
	var s []string
	for _, x := range secrets {
		if strings.TrimSpace(x) != "" {
			s = append(s, x)
		}
	}
	return &Scrubber{secrets: s, mask: mask}
}

// Text 擦除单个字符串里的所有敏感子串。诊断文本(sidecar stderr、错误体)喂给
// 用户之前必须过它 —— dsh 的错误体会带厂商品牌名与 base URL。
func (s *Scrubber) Text(in string) string {
	if in == "" || len(s.secrets) == 0 {
		return in
	}
	out := in
	for _, sec := range s.secrets {
		if sec == "" {
			continue
		}
		out = strings.ReplaceAll(out, sec, s.mask)
	}
	return out
}

// Event 返回一个所有文本字段都已脱敏的事件副本。用于喂 sink 前的最后一步。
func (s *Scrubber) Event(e event.Event) event.Event {
	if len(s.secrets) == 0 {
		return e
	}
	e.Text = s.Text(e.Text)
	e.Reasoning = s.Text(e.Reasoning)
	e.Tool.Args = s.Text(e.Tool.Args)
	e.Tool.Output = s.Text(e.Tool.Output)
	e.Tool.Err = s.Text(e.Tool.Err)
	return e
}
