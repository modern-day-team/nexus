package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

type staticModel struct {
	reply *schema.Message
}

func (m *staticModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.reply, nil
}

func (m *staticModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.reply}), nil
}

func (m *staticModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type scriptedModel struct {
	streams [][]*schema.Message
	idx     int
}

func (m *scriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := []*schema.Message{{Role: schema.Assistant, Content: "done"}}
	if m.idx < len(m.streams) {
		chunks = m.streams[m.idx]
		m.idx++
	}
	return schema.StreamReaderFromArray(chunks), nil
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type echoInput struct {
	Text string `json:"text" jsonschema:"required" jsonschema_description:"要回显的文本"`
}

func newEchoTool(t *testing.T) tool.BaseTool {
	t.Helper()
	echo, err := toolutils.InferTool("echo", "回显输入文本", func(_ context.Context, in echoInput) (string, error) {
		return in.Text, nil
	})
	if err != nil {
		t.Fatalf("构建 echo 工具失败: %v", err)
	}
	return echo
}

func newTestAgent(t *testing.T, cm model.ToolCallingChatModel, tools ...tool.BaseTool) *Agent {
	t.Helper()
	a, err := New(context.Background(), Options{ChatModel: cm, Tools: tools})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	return a
}

func TestChatWritesHistoryInOrder(t *testing.T) {
	a := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hello"}})

	reply, err := a.Chat(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if reply.Content != "hello" {
		t.Fatalf("回复内容 = %q, want hello", reply.Content)
	}

	history, err := a.History(context.Background())
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("历史条数 = %d, want 2", len(history))
	}
	if history[0].Role != schema.User || history[0].Content != "hi" {
		t.Fatalf("第 1 条历史 = %+v", history[0])
	}
	if history[1].Role != schema.Assistant || history[1].Content != "hello" {
		t.Fatalf("第 2 条历史 = %+v", history[1])
	}
}

func TestChatStreamEventsToolFlow(t *testing.T) {
	final := &schema.Message{
		Role:    schema.Assistant,
		Content: "final",
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
		},
	}
	toolCall := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"hi"}`},
		}},
	}

	a := newTestAgent(t,
		&scriptedModel{streams: [][]*schema.Message{{toolCall}, {final}}},
		newEchoTool(t),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var (
		types      []EventType
		text       string
		toolEndRes string
		done       *Event
	)
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv 失败: %v", err)
		}
		types = append(types, ev.Type)
		switch ev.Type {
		case EventToken:
			text += ev.Content
		case EventToolEnd:
			toolEndRes = ev.Result
		case EventDone:
			e := ev
			done = &e
		}
	}

	if done == nil {
		t.Fatal("缺少 EventDone")
	}
	if types[len(types)-1] != EventDone {
		t.Fatalf("最后一个事件 = %s, want done", types[len(types)-1])
	}
	if done.Err != nil {
		t.Fatalf("Done.Err = %v", done.Err)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 8 {
		t.Fatalf("Done.Usage = %+v, want total 8", done.Usage)
	}
	if toolEndRes != "hi" {
		t.Fatalf("工具结果 = %q, want hi", toolEndRes)
	}
	if text != "final" {
		t.Fatalf("正文 = %q, want final", text)
	}

	history, err := a.History(context.Background())
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("历史条数 = %d, want 2", len(history))
	}
	if history[1].Content != "final" || len(history[1].ToolCalls) != 0 {
		t.Fatalf("assistant 历史 = %+v", history[1])
	}
}

type slowModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *slowModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	close(m.started)
	<-m.release
	return &schema.Message{Role: schema.Assistant, Content: "slow"}, nil
}

func (m *slowModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "slow"}}), nil
}

func (m *slowModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestResetWaitsForActiveTurn(t *testing.T) {
	sm := &slowModel{started: make(chan struct{}), release: make(chan struct{})}
	a := newTestAgent(t, sm)

	chatDone := make(chan error, 1)
	go func() {
		_, err := a.Chat(context.Background(), "hi")
		chatDone <- err
	}()

	select {
	case <-sm.started:
	case <-time.After(time.Second):
		t.Fatal("Chat 未开始")
	}

	resetDone := make(chan error, 1)
	go func() {
		resetDone <- a.Reset(context.Background())
	}()

	select {
	case <-resetDone:
		t.Fatal("Reset 不应在轮次结束前返回")
	case <-time.After(20 * time.Millisecond):
	}

	close(sm.release)

	if err := <-chatDone; err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	select {
	case err := <-resetDone:
		if err != nil {
			t.Fatalf("Reset 失败: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reset 未在轮次结束后返回")
	}

	history, err := a.History(context.Background())
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("Reset 后历史条数 = %d, want 0", len(history))
	}
}

func TestMemorySession(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySession()

	if err := s.Append(ctx, schema.UserMessage("a"), schema.AssistantMessage("b", nil)); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	msgs, err := s.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages 失败: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "a" || msgs[1].Content != "b" {
		t.Fatalf("消息 = %+v", msgs)
	}

	if err := s.Clear(ctx); err != nil {
		t.Fatalf("Clear 失败: %v", err)
	}
	msgs, err = s.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages 失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Clear 后消息条数 = %d, want 0", len(msgs))
	}
}
