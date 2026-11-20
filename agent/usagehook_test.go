package agent

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func newUsageModel() *staticModel {
	return &staticModel{reply: &schema.Message{
		Role:    schema.Assistant,
		Content: "hi",
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
				PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 6},
			},
		},
	}}
}

func wantUsage() *Usage {
	return &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CachedTokens: 6}
}

func assertUsage(t *testing.T, got *Usage) {
	t.Helper()
	if got == nil {
		t.Fatal("hook 收到 nil Usage")
	}
	want := wantUsage()
	if got.PromptTokens != want.PromptTokens ||
		got.CompletionTokens != want.CompletionTokens ||
		got.TotalTokens != want.TotalTokens ||
		got.CachedTokens != want.CachedTokens {
		t.Fatalf("Usage = %+v, want %+v", *got, *want)
	}
}

func newUsageHookAgent(t *testing.T, cm model.ToolCallingChatModel, hook UsageHook) *Agent {
	t.Helper()
	a, err := New(context.Background(), Options{ChatModel: cm, UsageHook: hook})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	return a
}

func TestUsageHookChat(t *testing.T) {
	called := make(chan *Usage, 1)
	a := newUsageHookAgent(t, newUsageModel(), func(_ context.Context, usage *Usage) {
		called <- usage
	})

	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	select {
	case usage := <-called:
		assertUsage(t, usage)
	case <-time.After(2 * time.Second):
		t.Fatal("Chat 未触发 UsageHook")
	}
}

func TestUsageHookChatStreamEvents(t *testing.T) {
	called := make(chan *Usage, 1)
	a := newUsageHookAgent(t, newUsageModel(), func(_ context.Context, usage *Usage) {
		called <- usage
	})

	stream, err := a.ChatStreamEvents(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var doneUsage *Usage
	for {
		ev, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		if ev.Type == EventDone {
			doneUsage = ev.Usage
		}
	}

	select {
	case hookUsage := <-called:
		assertUsage(t, hookUsage)
		if doneUsage == nil {
			t.Fatal("未收到 EventDone.Usage")
		}
		if *hookUsage != *doneUsage {
			t.Fatalf("hook Usage = %+v, EventDone.Usage = %+v", *hookUsage, *doneUsage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ChatStreamEvents 未触发 UsageHook")
	}
}

func TestUsageHookChatStream(t *testing.T) {
	called := make(chan *Usage, 1)
	a := newUsageHookAgent(t, newUsageModel(), func(_ context.Context, usage *Usage) {
		called <- usage
	})

	stream, err := a.ChatStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	// 消费完整流，让后台 goroutine 完成 ConcatMessageStream。
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
	}

	select {
	case usage := <-called:
		assertUsage(t, usage)
	case <-time.After(2 * time.Second):
		t.Fatal("ChatStream 未触发 UsageHook")
	}
}

func TestUsageHookNil(t *testing.T) {
	m := newUsageModel()

	a := newTestAgent(t, m)
	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	stream, err := a.ChatStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	for {
		if _, recvErr := stream.Recv(); recvErr != nil {
			stream.Close()
			break
		}
	}

	evStream, err := a.ChatStreamEvents(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer evStream.Close()
	for {
		if _, recvErr := evStream.Recv(); recvErr != nil {
			break
		}
	}
}

func TestUsageHookSkippedWhenNoMeta(t *testing.T) {
	called := false
	a := newUsageHookAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}},
		func(context.Context, *Usage) { called = true })

	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if called {
		t.Fatal("模型未返回用量时不应触发 UsageHook")
	}
}
