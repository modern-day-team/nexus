package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// ctxKey 是 customizer 注入 ctx 的自定义 value 键。
type ctxKey struct{}

// newCtxProbeTool 构建一个从 ctx 读取注入值的探针工具，结果写入 got。
func newCtxProbeTool(got *string) (tool.BaseTool, error) {
	return toolutils.InferTool("ctxprobe", "返回 ctx 中的注入值",
		func(ctx context.Context, _ struct{}) (string, error) {
			v, _ := ctx.Value(ctxKey{}).(string)
			*got = v
			return v, nil
		})
}

func TestContextCustomizerInjectsValueIntoToolExecution(t *testing.T) {
	// 工具从 ctx 取 customizer 注入的 value 并作为结果返回，验证
	// customizer 返回的 ctx 贯穿到工具执行。
	var got string
	impl, err := newCtxProbeTool(&got)
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}

	called := false
	a, err := New(context.Background(), Options{
		ChatModel: &scriptedModel{streams: [][]*schema.Message{{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "call_1", Type: "function",
				Function: schema.FunctionCall{Name: "ctxprobe", Arguments: `{}`},
			}}},
		}, {
			{Role: schema.Assistant, Content: "done"},
		}}},
		Tools: []tool.BaseTool{impl},
		ContextCustomizer: func(ctx context.Context, ev TraceEvent) context.Context {
			called = true
			if ev.Kind != TraceTurnStart {
				t.Errorf("customizer 收到事件 = %v, want %v", ev.Kind, TraceTurnStart)
			}
			return context.WithValue(ctx, ctxKey{}, "customized")
		},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}

	stream, err := a.ChatStreamEvents(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	// 工具在后台 goroutine 执行，须消费到 EventDone 再断言。
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			t.Fatalf("事件流提前结束: %v", rerr)
		}
		if ev.Type == EventDone {
			break
		}
	}
	if !called {
		t.Fatalf("ContextCustomizer 未被调用")
	}
	if got != "customized" {
		t.Fatalf("工具收到 ctx 值 = %q, want customized", got)
	}
}

func TestContextCustomizerReceivesModelName(t *testing.T) {
	var gotModel string
	a, err := New(context.Background(), Options{
		ChatModel: &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}},
		ModelName: "glm-5",
		ContextCustomizer: func(ctx context.Context, ev TraceEvent) context.Context {
			gotModel = ev.Model
			return ctx
		},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	if _, err := a.ChatStream(context.Background(), "hi"); err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	if gotModel != "glm-5" {
		t.Fatalf("customizer 收到 Model = %q, want glm-5", gotModel)
	}
}

func TestNilContextCustomizerIsNoop(t *testing.T) {
	// nil customizer 走原路径，行为不变（能正常完成轮次即视为零开销跳过）。
	a, err := New(context.Background(), Options{
		ChatModel: &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "ok"}},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	msg, err := a.Chat(context.Background(), "hi")
	if err != nil || msg.Content != "ok" {
		t.Fatalf("Chat = %q, %v", msg.Content, err)
	}
}
