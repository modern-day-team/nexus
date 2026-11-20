package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFileSessionAppendAndMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s, err := NewFileSession(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.Append(ctx, schema.UserMessage("你好"), schema.AssistantMessage("你好！", nil)); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "你好" || msgs[1].Content != "你好！" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	// Messages 返回副本，修改不影响会话。
	msgs[0].Content = "modified"
	msgs2, _ := s.Messages(ctx)
	if msgs2[0].Content != "你好" {
		t.Fatalf("Messages should return a copy, got %+v", msgs2[0])
	}
}

func TestFileSessionPersistAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	ctx := context.Background()

	s1, err := NewFileSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Append(ctx, schema.UserMessage("记住我")); err != nil {
		t.Fatal(err)
	}

	s2, err := NewFileSession(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "记住我" {
		t.Fatalf("session not persisted: %+v", msgs)
	}
}

func TestFileSessionClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s, err := NewFileSession(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.Append(ctx, schema.UserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty after clear, got %+v", msgs)
	}
}

func TestFileSessionRoundTripToolCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s, err := NewFileSession(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	want := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call_1", Function: schema.FunctionCall{Name: "now", Arguments: `{}`}},
	})
	if err := s.Append(ctx, want); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("tool calls lost: %+v", msgs)
	}
	got := msgs[0].ToolCalls[0]
	if got.ID != "call_1" || got.Function.Name != "now" {
		t.Fatalf("unexpected tool call: %+v", got)
	}
}

func TestFileSessionInvalidPath(t *testing.T) {
	if _, err := NewFileSession(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}
