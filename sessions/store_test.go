package sessions

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestRedisStoreMultiSessionIsolation(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewRedisStore(client, 0)
	defer store.Close()
	ctx := context.Background()

	s1, err := store.Session(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Session(ctx, "conv-2")
	if err != nil {
		t.Fatal(err)
	}

	if err := s1.Append(ctx, schema.UserMessage("one")); err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(ctx, schema.UserMessage("two")); err != nil {
		t.Fatal(err)
	}

	// 同 id 再次获取应读写一致。
	s1b, err := store.Session(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	msgs1, err := s1b.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs1) != 1 || msgs1[0].Content != "one" {
		t.Fatalf("conv-1 消息异常: %+v", msgs1)
	}

	msgs2, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 1 || msgs2[0].Content != "two" {
		t.Fatalf("conv-2 消息异常: %+v", msgs2)
	}
}

func TestRedisStoreDelete(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewRedisStore(client, 0)
	defer store.Close()
	ctx := context.Background()

	s, err := store.Session(ctx, "conv-del")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, schema.UserMessage("bye")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "conv-del"); err != nil {
		t.Fatal(err)
	}

	// 删除后重新获取，应得到空会话。
	s2, err := store.Session(ctx, "conv-del")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("删除后应无历史，实际 %d 条", len(msgs))
	}
}

func TestRedisStoreTTL(t *testing.T) {
	client, mr := newTestRedis(t)
	ttl := 10 * time.Second
	store := NewRedisStore(client, ttl)
	defer store.Close()
	ctx := context.Background()

	s, err := store.Session(ctx, "conv-ttl")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, schema.UserMessage("tick")); err != nil {
		t.Fatal(err)
	}

	got, err := client.TTL(ctx, redisKeyPrefix+"conv-ttl").Result()
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 || got > ttl {
		t.Fatalf("TTL 未按预期设置: %v", got)
	}

	// 用 miniredis 快进时间验证过期。
	mr.FastForward(11 * time.Second)
	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("过期后应无历史，实际 %d 条", len(msgs))
	}
}

func TestRedisStoreInvalidID(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewRedisStore(client, 0)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Session(ctx, ""); err == nil {
		t.Fatal("空 id 应报错")
	}
	if _, err := store.Session(ctx, "bad id"); err == nil {
		t.Fatal("含空白字符的 id 应报错")
	}
	if err := store.Delete(ctx, ""); err == nil {
		t.Fatal("空 id 删除应报错")
	}
}

func TestSQLiteStoreMultiSessionIsolation(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	s1, err := store.Session(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Session(ctx, "conv-2")
	if err != nil {
		t.Fatal(err)
	}

	if err := s1.Append(ctx, schema.UserMessage("one"), schema.AssistantMessage("r1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(ctx, schema.UserMessage("two")); err != nil {
		t.Fatal(err)
	}

	msgs1, err := s1.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs1) != 2 || msgs1[0].Content != "one" {
		t.Fatalf("conv-1 消息异常: %+v", msgs1)
	}

	msgs2, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 1 || msgs2[0].Content != "two" {
		t.Fatalf("conv-2 消息异常: %+v", msgs2)
	}
}

func TestSQLiteStoreDelete(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	s, err := store.Session(ctx, "conv-del")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, schema.UserMessage("bye")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "conv-del"); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Session(ctx, "conv-del")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("删除后应无历史，实际 %d 条", len(msgs))
	}
}

func TestSQLiteStoreInvalidID(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Session(context.Background(), ""); err == nil {
		t.Fatal("空 id 应报错")
	}
	if _, err := store.Session(context.Background(), "bad id"); err == nil {
		t.Fatal("含空白字符的 id 应报错")
	}
}

func TestSQLiteStoreCleanup(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	idle := 200 * time.Millisecond
	interval := 50 * time.Millisecond
	store, err := NewSQLiteStore(db, WithCleanupInterval(interval), WithIdleDuration(idle))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	s1, err := store.Session(ctx, "conv-old")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Session(ctx, "conv-new")
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Append(ctx, schema.UserMessage("old")); err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(ctx, schema.UserMessage("new")); err != nil {
		t.Fatal(err)
	}

	// 把 conv-old 的 last_active 改旧、conv-new 改到未来，验证清理只命中闲置会话。
	cut := float64(time.Now().Add(-2 * idle).Unix())
	if _, err := db.Exec(
		`UPDATE nexus_sessions SET last_active = CASE conversation_id
			WHEN 'conv-old' THEN ?
			WHEN 'conv-new' THEN ?
		 ELSE last_active END`, cut, float64(time.Now().Add(time.Hour).Unix())); err != nil {
		t.Fatal(err)
	}

	// 等待后台清理执行。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM nexus_sessions WHERE conversation_id = 'conv-old'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	msgs1, _ := s1.Messages(ctx)
	if len(msgs1) != 0 {
		t.Fatalf("闲置会话应被清理，实际 %d 条", len(msgs1))
	}
	msgs2, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 1 {
		t.Fatalf("活跃会话不应被清理，实际 %d 条", len(msgs2))
	}

	// Close 应停止后台 goroutine。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil { // 重复 Close 安全
		t.Fatal(err)
	}
}

func TestSQLiteStoreNilDB(t *testing.T) {
	if _, err := NewSQLiteStore(nil); err == nil {
		t.Fatal("db 为 nil 应报错")
	}
}

func TestValidateConversationID(t *testing.T) {
	for _, ok := range []string{"a", "conv-1", "user:123", "中文会话", "a.b_c-d"} {
		if err := validateConversationID(ok); err != nil {
			t.Fatalf("合法 id %q 不应报错: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "a b", "a\nb", "a\tb", string(make([]byte, 300))} {
		if err := validateConversationID(bad); err == nil {
			t.Fatalf("非法 id %q 应报错", bad)
		}
	}
}
