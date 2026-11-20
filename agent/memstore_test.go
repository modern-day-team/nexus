package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMemoryStoreSameIDSameInstance(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	s1, err := store.Session(ctx, "conv-a")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Session(ctx, "conv-a")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("同一 conversationID 应返回同一实例")
	}
}

func TestMemoryStoreConcurrentSameID(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	const n = 32
	results := make([]Session, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := store.Session(ctx, "race-conv")
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = s
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatal("并发取同一 id 应得到同一实例")
		}
	}
}

func TestMemoryStoreIsolation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	sa, _ := store.Session(ctx, "conv-a")
	sb, _ := store.Session(ctx, "conv-b")

	if err := sa.Append(ctx, schema.UserMessage("hello-a")); err != nil {
		t.Fatal(err)
	}

	msgsB, err := sb.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgsB) != 0 {
		t.Fatalf("不同会话应互相隔离，但 conv-b 看到 %d 条消息", len(msgsB))
	}

	msgsA, _ := sa.Messages(ctx)
	if len(msgsA) != 1 || msgsA[0].Content != "hello-a" {
		t.Fatalf("conv-a 消息异常: %+v", msgsA)
	}
}

func TestMemoryStoreDeleteThenNewInstance(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	s1, _ := store.Session(ctx, "conv-del")
	if err := s1.Append(ctx, schema.UserMessage("old")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "conv-del"); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Session(ctx, "conv-del")
	if err != nil {
		t.Fatal(err)
	}
	if s2 == s1 {
		t.Fatal("Delete 后应返回新实例")
	}
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Delete 后历史应为空，实际 %d 条", len(msgs))
	}
}

func TestMemoryStoreEmptyID(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	if _, err := store.Session(ctx, ""); err == nil {
		t.Fatal("空 conversationID 应报错")
	}
	if err := store.Delete(ctx, ""); err == nil {
		t.Fatal("空 conversationID 删除应报错")
	}
}

func TestMemoryStoreClose(t *testing.T) {
	if err := NewMemoryStore().Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewWithStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	model := &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}}
	opts := Options{ChatModel: model}

	a1, err := NewWithStore(ctx, store, "conv-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := NewWithStore(ctx, store, "conv-2", opts)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a1.Chat(ctx, "第一句"); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.Chat(ctx, "第二句"); err != nil {
		t.Fatal(err)
	}

	h1, err := a1.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := a2.History(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(h1) != 2 || h1[0].Content != "第一句" || h1[1].Content != "hi" {
		t.Fatalf("conv-1 历史异常: %+v", h1)
	}
	if len(h2) != 2 || h2[0].Content != "第二句" || h2[1].Content != "hi" {
		t.Fatalf("conv-2 历史异常: %+v", h2)
	}
}

func TestNewWithStoreNilStore(t *testing.T) {
	if _, err := NewWithStore(context.Background(), nil, "x", Options{}); err == nil {
		t.Fatal("store 为 nil 应报错")
	}
}

func TestNewWithStoreEmptyConversationID(t *testing.T) {
	if _, err := NewWithStore(context.Background(), NewMemoryStore(), "", Options{}); err == nil {
		t.Fatal("空 conversationID 应报错")
	}
}
