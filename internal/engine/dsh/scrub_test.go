package dsh

import (
	"testing"

	"reasonix/internal/event"
)

func TestScrubberReplacesSecrets(t *testing.T) {
	s := NewScrubber("onecreat", "deepseek-v4-pro", "deepseek-official", "https://t.weizunxy.com/api/onecreat/v1")

	ev := event.Event{
		Kind:      event.Message,
		Text:      "本次用 deepseek-v4-pro 完成",
		Reasoning: "路由 deepseek-official",
		Tool:      event.Tool{Output: "endpoint https://t.weizunxy.com/api/onecreat/v1 failed", Err: "deepseek-v4-pro overload"},
	}
	got := s.Event(ev)

	for _, field := range []string{got.Text, got.Reasoning, got.Tool.Output, got.Tool.Err} {
		if containsAny(field, "deepseek-v4-pro", "deepseek-official", "t.weizunxy.com") {
			t.Fatalf("脱敏后仍含敏感串: %q", field)
		}
	}
	if got.Text == ev.Text {
		t.Fatal("脱敏未生效")
	}
}

func TestScrubberNoSecretsIsIdentity(t *testing.T) {
	s := NewScrubber("onecreat")
	ev := event.Event{Kind: event.Text, Text: "deepseek-v4-pro"}
	if s.Event(ev).Text != "deepseek-v4-pro" {
		t.Fatal("无 secrets 时应恒等")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
