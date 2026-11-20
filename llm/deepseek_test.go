package llm

import (
	"context"
	"testing"
)

func TestNewDeepSeekRequiresAPIKey(t *testing.T) {
	if _, err := NewDeepSeek(context.Background(), DeepSeekConfig{}); err == nil {
		t.Fatal("expected error for empty APIKey")
	}
}

func TestNewDeepSeekDefaults(t *testing.T) {
	cm, err := NewDeepSeek(context.Background(), DeepSeekConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if cm == nil {
		t.Fatal("expected non-nil model")
	}
}

func TestNewDeepSeekThinkingOptions(t *testing.T) {
	on := true
	off := false
	for _, cfg := range []DeepSeekConfig{
		{APIKey: "sk-test", Thinking: &on, ReasoningEffort: "high"},
		{APIKey: "sk-test", Thinking: &off},
	} {
		if _, err := NewDeepSeek(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
}
