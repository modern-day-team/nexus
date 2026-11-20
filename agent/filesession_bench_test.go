package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// BenchmarkFileSessionAppend 测量长会话中持续追加消息的开销。
// JSONL 追加写应与历史长度无关（O(新增条数)）。
func BenchmarkFileSessionAppend(b *testing.B) {
	for _, historyLen := range []int{10, 100, 500} {
		b.Run("history_"+string(rune('0'+historyLen/100))+"x"+string(rune('0'+historyLen%100)), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "bench.jsonl")
			s, err := NewFileSession(path)
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			msg := schema.UserMessage("benchmark message with some content to be realistic")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Append(ctx, msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFileSessionMessages 测量读取历史的开销（内存缓存，无磁盘 IO）。
func BenchmarkFileSessionMessages(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.jsonl")
	s, err := NewFileSession(path)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		if err := s.Append(ctx, schema.UserMessage("filler")); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Messages(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
