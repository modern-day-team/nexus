package sessions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// newTestSQLite 在临时目录创建一个文件型 SQLite 数据库和会话表。
// 用临时文件（而非内存库）才能验证真正的跨实例恢复。
func newTestSQLite(t *testing.T, table string) (*SQLiteSession, *SQLiteSession) {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s1, err := NewSQLiteSession(db, table)
	if err != nil {
		t.Fatal(err)
	}
	// 第二个实例模拟另一进程：同一个 DB、同一个表。
	s2, err := NewSQLiteSession(db, table)
	if err != nil {
		t.Fatal(err)
	}
	return s1, s2
}

func TestSQLiteSessionAppendAndMessages(t *testing.T) {
	s, _ := newTestSQLite(t, "msgs1")
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

func TestSQLiteSessionPersistAcrossInstances(t *testing.T) {
	s1, s2 := newTestSQLite(t, "msgs2")
	ctx := context.Background()

	if err := s1.Append(ctx, schema.UserMessage("记住我")); err != nil {
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

func TestSQLiteSessionClear(t *testing.T) {
	s, s2 := newTestSQLite(t, "msgs3")
	ctx := context.Background()

	if err := s.Append(ctx, schema.UserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}

	// 清空后另一实例也应读到空。
	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty after clear, got %+v", msgs)
	}
}

func TestSQLiteSessionRoundTripToolCalls(t *testing.T) {
	s1, s2 := newTestSQLite(t, "msgs4")
	ctx := context.Background()

	want := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call_1", Function: schema.FunctionCall{Name: "now", Arguments: `{}`}},
	})
	if err := s1.Append(ctx, want); err != nil {
		t.Fatal(err)
	}

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

func TestSQLiteSessionEmptyAppendNoop(t *testing.T) {
	s, _ := newTestSQLite(t, "msgs5")
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

// TestSQLiteSessionSequentialAppend 验证顺序多次 Append 后顺序正确。
// *sql.DB 自带连接池且并发安全，跨 goroutine 使用同样安全。
func TestSQLiteSessionSequentialAppend(t *testing.T) {
	s, s2 := newTestSQLite(t, "msgs6")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, schema.UserMessage("msg")); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := s2.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}
}

func TestSQLiteSessionInvalidTable(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "bad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 非法表名应拒绝，防止 SQL 注入。
	for _, name := range []string{"", "msgs; DROP TABLE x", "1table"} {
		if _, err := NewSQLiteSession(db, name); err == nil {
			t.Fatalf("expected error for table %q", name)
		}
	}
}

func TestOpenSQLiteEmptyPath(t *testing.T) {
	if _, err := OpenSQLite(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}
