package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// manyChunkModel 一次流式输出 n 个正文分片，模拟长回复。
type manyChunkModel struct {
	n int
}

func (m *manyChunkModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
}

func (m *manyChunkModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := make([]*schema.Message, m.n)
	for i := range chunks {
		chunks[i] = &schema.Message{Role: schema.Assistant, Content: "x"}
	}
	return schema.StreamReaderFromArray(chunks), nil
}

func (m *manyChunkModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// BenchmarkChatStreamEventsThroughput 测量事件从产生到消费的全链路吞吐
// （回归基准：事件转发路径的改动不应显著恶化该值）。
func BenchmarkChatStreamEventsThroughput(b *testing.B) {
	a, err := New(context.Background(), Options{ChatModel: &manyChunkModel{n: 1000}})
	if err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, err := a.ChatStreamEvents(ctx, "bench")
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := stream.Recv(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
		stream.Close()
	}
}
