package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"
)

// newTestRedis 启动一个本地 miniredis（无需真实 Redis）并返回客户端。
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return client, mr
}

func TestRedisSessionAppendAndMessages(t *testing.T) {
	client, _ := newTestRedis(t)
	s := NewRedisSession(client, "chat1", 0)
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
}

func TestRedisSessionPersistAcrossInstances(t *testing.T) {
	client, _ := newTestRedis(t)
	ctx := context.Background()

	// 第一个实例写入，第二个实例（模拟另一进程）同 key 读取。
	s1 := NewRedisSession(client, "chat2", 0)
	if err := s1.Append(ctx, schema.UserMessage("记住我")); err != nil {
		t.Fatal(err)
	}

	s2 := NewRedisSession(client, "chat2", 0)
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "记住我" {
		t.Fatalf("session not persisted: %+v", msgs)
	}
}

func TestRedisSessionClear(t *testing.T) {
	client, _ := newTestRedis(t)
	s := NewRedisSession(client, "chat3", 0)
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

func TestRedisSessionTTL(t *testing.T) {
	client, mr := newTestRedis(t)
	s := NewRedisSession(client, "chat4", 5*time.Minute)
	ctx := context.Background()

	if err := s.Append(ctx, schema.UserMessage("hi")); err != nil {
		t.Fatal(err)
	}

	// 用 miniredis 的 TTL 检查 API 验证过期时间已设置。
	ttl := mr.TTL("nexus:session:chat4")
	if ttl <= 0 || ttl > 5*time.Minute {
		t.Fatalf("unexpected TTL: %v", ttl)
	}
}

func TestRedisSessionEmptyAppendNoop(t *testing.T) {
	client, _ := newTestRedis(t)
	s := NewRedisSession(client, "chat5", 0)
	ctx := context.Background()

	if err := s.Append(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty, got %+v", msgs)
	}
}

func TestRedisSessionRoundTripToolCalls(t *testing.T) {
	client, _ := newTestRedis(t)
	s := NewRedisSession(client, "chat6", 0)
	ctx := context.Background()

	want := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call_1", Function: schema.FunctionCall{Name: "now", Arguments: `{}`}},
	})
	if err := s.Append(ctx, want); err != nil {
		t.Fatal(err)
	}

	// 跨实例读回，验证 JSON 往返不丢 ToolCalls。
	s2 := NewRedisSession(client, "chat6", 0)
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("tool calls lost: %+v", msgs)
	}
	got := msgs[0].ToolCalls[0]
	if got.ID != "call_1" || got.Function.Name != "now" || got.Function.Arguments != `{}` {
		t.Fatalf("unexpected tool call: %+v", got)
	}
}

func TestRedisSessionMessagesReturnCopy(t *testing.T) {
	client, _ := newTestRedis(t)
	s := NewRedisSession(client, "chat7", 0)
	ctx := context.Background()

	if err := s.Append(ctx, schema.UserMessage("原始")); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 修改读回的消息不应影响存储内容。
	msgs[0].Content = "modified"
	msgs2, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msgs2[0].Content != "原始" {
		t.Fatalf("Messages should be decoded from storage, got %+v", msgs2[0])
	}
}
