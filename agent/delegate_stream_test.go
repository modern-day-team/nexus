package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// streamFailModel 在流式输出一个分片后报错，模拟流式中途失败。
type streamFailModel struct {
	err error
}

func (m *streamFailModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, m.err
}

func (m *streamFailModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	out, writer := schema.Pipe[*schema.Message](2)
	writer.Send(&schema.Message{Role: schema.Assistant, Content: "部分"}, nil)
	writer.Send(nil, m.err)
	writer.Close()
	return out, nil
}

func (m *streamFailModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// streamToolCall 构造一次委派工具调用的 assistant 消息。
func streamToolCall(name, input string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_" + name,
			Type:     "function",
			Function: schema.FunctionCall{Name: name, Arguments: `{"input":"` + input + `"}`},
		}},
	}
}

func newDelegationStreamTool(t *testing.T, opts DelegationOptions) tool.InvokableTool {
	t.Helper()
	dt, err := NewDelegationStreamTool(opts)
	if err != nil {
		t.Fatalf("构建流式委派工具失败: %v", err)
	}
	return dt
}

func TestNewDelegationStreamToolValidatesOptions(t *testing.T) {
	ctx := context.Background()
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}})

	if _, err := NewDelegationStreamTool(DelegationOptions{Description: "d", SubAgent: sub}); err == nil {
		t.Fatal("Name 为空时应报错")
	}
	if _, err := NewDelegationStreamTool(DelegationOptions{Name: "n", SubAgent: sub}); err == nil {
		t.Fatal("Description 为空时应报错")
	}
	if _, err := NewDelegationStreamTool(DelegationOptions{Name: "n", Description: "d"}); err == nil {
		t.Fatal("SubAgent 为空时应报错")
	}

	info, err := newDelegationStreamTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub}).Info(ctx)
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "delegate" || info.Desc != "委派任务" {
		t.Fatalf("工具信息 = %+v", info)
	}
}

func TestDelegationStreamForwardsEvents(t *testing.T) {
	ctx := context.Background()

	// 子 Agent：两段正文 + Done 携带用量。
	childFinal := &schema.Message{
		Role:    schema.Assistant,
		Content: "任务完成",
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		},
	}
	sub := newTestAgent(t, &scriptedModel{streams: [][]*schema.Message{
		{{Role: schema.Assistant, Content: "正在"}, childFinal},
	}})

	streamTool := newDelegationStreamTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	// 父 Agent：第一轮流式发起委派工具调用，第二轮流式给出最终回答。
	parent := newTestAgent(t,
		&scriptedModel{streams: [][]*schema.Message{
			{streamToolCall("delegate", "帮我写个摘要")},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		streamTool,
	)

	stream, err := parent.ChatStreamEvents(ctx, "use delegate")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var (
		parentText  string
		childTokens []string
		childDone   *Event
		toolEnd     *Event
		sawErr      error
	)
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv 失败: %v", err)
		}
		if ev.Err != nil {
			sawErr = ev.Err
		}
		switch ev.Type {
		case EventToken:
			if ev.Agent == "delegate" {
				childTokens = append(childTokens, ev.Content)
			} else {
				parentText += ev.Content
			}
		case EventToolEnd:
			if ev.Agent == "" && ev.ToolName == "delegate" {
				e := ev
				toolEnd = &e
			}
		case EventDone:
			if ev.Agent == "delegate" {
				e := ev
				childDone = &e
			}
		}
	}

	if sawErr != nil {
		t.Fatalf("父事件流不应有错误: %v", sawErr)
	}
	if strings.Join(childTokens, "") != "正在任务完成" {
		t.Fatalf("透传的子 token = %v", childTokens)
	}
	if childDone == nil {
		t.Fatal("缺少 Agent=delegate 的 EventDone")
	}
	if childDone.Usage == nil || childDone.Usage.TotalTokens != 3 {
		t.Fatalf("透传 EventDone.Usage = %+v, want total 3", childDone.Usage)
	}
	if toolEnd == nil {
		t.Fatal("缺少委派工具的 EventToolEnd")
	}
	if toolEnd.Result != "正在任务完成" {
		t.Fatalf("委派工具结果 = %q, want 正在任务完成", toolEnd.Result)
	}
	if parentText != "final" {
		t.Fatalf("父正文 = %q, want final", parentText)
	}

	// 父会话历史只含 user 与最终 assistant 回复：委派工具的呼应对与
	// 子过程事件都不会写入父历史。
	history, err := parent.History(ctx)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("父历史条数 = %d, want 2: %+v", len(history), history)
	}
	if history[0].Content != "use delegate" || history[1].Content != "final" {
		t.Fatalf("父历史内容 = %+v", history)
	}
	for _, msg := range history {
		if strings.Contains(msg.Content, "正在任务完成") {
			t.Fatalf("子过程内容不应出现在父历史: %+v", msg)
		}
	}

	// 子 Agent 自己的历史写入了一轮（子过程不污染父历史）。
	subHistory, err := sub.History(ctx)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(subHistory) != 2 {
		t.Fatalf("子历史条数 = %d, want 2", len(subHistory))
	}
}

func TestDelegationStreamSubAgentError(t *testing.T) {
	// streamFailModel 先输出一个分片再报错，模拟流式中途失败，
	// 保证子 Agent 先产生 EventError/EventDone 再结束。
	wantErr := errors.New("模型宕机")
	sub := newTestAgent(t, &streamFailModel{err: wantErr})
	dt := newDelegationStreamTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	var events []Event
	ctx := withEmitter(context.Background(), func(ev Event) { events = append(events, ev) })

	_, err := dt.InvokableRun(ctx, `{"input":"任务"}`)
	if err == nil {
		t.Fatal("子 Agent 流式失败时应返回错误")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("错误应包装原始错误: %v", err)
	}

	// 子事件应透传到 emitter，且 Agent 字段为委派工具名。
	var sawErr, sawDone bool
	for _, ev := range events {
		if ev.Agent != "delegate" {
			t.Fatalf("透传事件 Agent = %q, want delegate: %+v", ev.Agent, ev)
		}
		switch ev.Type {
		case EventError:
			sawErr = true
			if !errors.Is(ev.Err, wantErr) {
				t.Fatalf("透传 EventError.Err = %v, want 模型宕机", ev.Err)
			}
		case EventDone:
			sawDone = true
			if ev.Err == nil {
				t.Fatal("透传 EventDone.Err 应非空")
			}
		}
	}
	if !sawErr || !sawDone {
		t.Fatalf("缺少透传的 EventError/EventDone, events = %+v", events)
	}
}

func TestDelegationStreamWithoutEmitter(t *testing.T) {
	ctx := context.Background()

	// 子 Agent 直接用静态模型，父 ctx 上没有 emitter 时退化为普通委派。
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "子任务完成"}})
	dt := newDelegationStreamTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	out, err := dt.InvokableRun(ctx, `{"input":"帮我写个摘要"}`)
	if err != nil {
		t.Fatalf("委派失败: %v", err)
	}
	if out != "子任务完成" {
		t.Fatalf("工具结果 = %q, want 子任务完成", out)
	}
}
