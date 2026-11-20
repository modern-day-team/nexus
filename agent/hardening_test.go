package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// flakyModel 前 failTimes 次调用返回错误，之后成功。
type flakyModel struct {
	failTimes  int
	calls      int
	reply      *schema.Message
	failStream bool
}

func (m *flakyModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls++
	if m.calls <= m.failTimes {
		return nil, errors.New("429 rate limited")
	}
	return m.reply, nil
}

func (m *flakyModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++
	if m.calls <= m.failTimes && m.failStream {
		return nil, errors.New("503 unavailable")
	}
	return schema.StreamReaderFromArray([]*schema.Message{m.reply}), nil
}

func (m *flakyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestRetryingModelRecovers(t *testing.T) {
	inner := &flakyModel{failTimes: 2, reply: &schema.Message{Role: schema.Assistant, Content: "ok"}}
	m := newRetryingModel(inner, &RetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    2 * time.Millisecond,
	})

	reply, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "ok" {
		t.Fatalf("reply = %q, want ok", reply.Content)
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3", inner.calls)
	}
}

func TestRetryingModelGivesUp(t *testing.T) {
	inner := &flakyModel{failTimes: 99, reply: &schema.Message{Role: schema.Assistant, Content: "ok"}}
	m := newRetryingModel(inner, &RetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    2 * time.Millisecond,
	})

	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3", inner.calls)
	}
}

func TestRetryingModelStreamRecovers(t *testing.T) {
	inner := &flakyModel{failTimes: 1, failStream: true, reply: &schema.Message{Role: schema.Assistant, Content: "ok"}}
	m := newRetryingModel(inner, &RetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    2 * time.Millisecond,
	})

	stream, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	msg, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if msg != nil && msg.Content != "ok" {
		t.Fatalf("content = %q, want ok", msg.Content)
	}
}

func TestRetryDisabled(t *testing.T) {
	inner := &flakyModel{failTimes: 1, reply: &schema.Message{Role: schema.Assistant, Content: "ok"}}
	m := newRetryingModel(inner, nil)
	if m != inner {
		t.Fatal("nil options should return inner model unchanged")
	}
}

func TestTruncateCompactor(t *testing.T) {
	c := NewTruncateCompactor(6, 2)
	if c == nil {
		t.Fatal("expected non-nil compactor")
	}
	ctx := context.Background()

	// 构造超限历史：user/assistant 交替，中间夹一组工具调用。
	msgs := []*schema.Message{
		schema.UserMessage("任务说明（需保留）"),
		schema.AssistantMessage("收到", nil),
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			schema.UserMessage("问题"),
			schema.AssistantMessage("回答", nil),
		)
	}

	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 6+1 { // MaxMessages + 截断说明
		t.Fatalf("压缩后条数 = %d, 超过上限", len(out))
	}
	if out[0].Content != "任务说明（需保留）" {
		t.Fatalf("头部消息丢失: %q", out[0].Content)
	}
	// 尾段必须从 user 边界开始，且说明消息在中间。
	foundNotice := false
	for i, m := range out {
		if strings.Contains(m.Content, "已因超出上下文限制被截断") {
			foundNotice = true
			if m.Role != schema.User {
				t.Fatalf("说明消息应为 user 角色，第 %d 条不是", i)
			}
			break
		}
	}
	if !foundNotice {
		t.Fatal("缺少截断说明消息")
	}
	// 未超限时应原样返回。
	same, err := c.Compact(ctx, msgs[:5])
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != 5 {
		t.Fatalf("未超限历史被压缩: %d", len(same))
	}
}

func TestNewTruncateCompactorInvalid(t *testing.T) {
	if NewTruncateCompactor(0, 0) != nil {
		t.Fatal("MaxMessages<=0 应返回 nil")
	}
	if c := NewTruncateCompactor(4, 100); c.KeepHead != 2 {
		t.Fatalf("KeepHead = %d, want 2（不超过一半）", c.KeepHead)
	}
}

func TestToolApprovalDeny(t *testing.T) {
	echo := newEchoTool(t)
	var asked string
	wrapped := applyApproval([]tool.BaseTool{echo}, func(ctx context.Context, call schema.ToolCall) error {
		asked = call.Function.Name
		return errors.New("不允许")
	})
	invokable, ok := wrapped[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("wrapped tool should remain invokable")
	}
	out, err := invokable.InvokableRun(context.Background(), `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("拒绝不应返回错误: %v", err)
	}
	if !strings.Contains(out, deniedToolPrefix) {
		t.Fatalf("工具结果应包含拒绝说明: %q", out)
	}
	if asked != "echo" {
		t.Fatalf("审批询问的工具 = %q, want echo", asked)
	}
}

func TestToolApprovalAllow(t *testing.T) {
	echo := newEchoTool(t)
	wrapped := applyApproval([]tool.BaseTool{echo}, func(context.Context, schema.ToolCall) error {
		return nil
	})
	invokable := wrapped[0].(tool.InvokableTool)
	out, err := invokable.InvokableRun(context.Background(), `{"text":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi" {
		t.Fatalf("放行后工具结果 = %q, want hi", out)
	}
}

func TestApplyApprovalNil(t *testing.T) {
	echo := newEchoTool(t)
	tools := []tool.BaseTool{echo}
	if applyApproval(tools, nil) == nil {
		t.Fatal("nil approver 应原样返回")
	}
}
