package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type output struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type recordedMsg struct {
	Role    schema.RoleType
	Content string
}

// genRecorderModel 按脚本依次返回 Generate 输出，并记录每轮收到的消息。
type genRecorderModel struct {
	replies []string
	calls   [][]recordedMsg
}

func (m *genRecorderModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	record := make([]recordedMsg, 0, len(msgs))
	for _, msg := range msgs {
		record = append(record, recordedMsg{Role: msg.Role, Content: msg.Content})
	}
	m.calls = append(m.calls, record)

	content := "not json"
	if len(m.replies) > len(m.calls)-1 {
		content = m.replies[len(m.calls)-1]
	}
	return &schema.Message{
		Role:    schema.Assistant,
		Content: content,
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
		},
	}, nil
}

func (m *genRecorderModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reply, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{reply}), nil
}

func (m *genRecorderModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestGenerateIntoFencedJSONFirstRound(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"```json\n{\"name\":\"nexus\",\"count\":3}\n```"}}
	a := newTestAgent(t, cm)

	var out output
	if err := a.GenerateInto(context.Background(), "给我一个对象", &out); err != nil {
		t.Fatalf("GenerateInto 失败: %v", err)
	}
	if out.Name != "nexus" || out.Count != 3 {
		t.Fatalf("out = %+v", out)
	}
	if len(cm.calls) != 1 {
		t.Fatalf("模型调用次数 = %d, want 1", len(cm.calls))
	}
}

func TestGenerateIntoRetryAfterInvalidJSON(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"这是解释 {oops", `{"name":"ok","count":1}`}}
	a := newTestAgent(t, cm)

	var out output
	if err := a.GenerateInto(context.Background(), "json please", &out); err != nil {
		t.Fatalf("GenerateInto 失败: %v", err)
	}
	if out.Name != "ok" || out.Count != 1 {
		t.Fatalf("out = %+v", out)
	}
	if len(cm.calls) != 2 {
		t.Fatalf("模型调用次数 = %d, want 2", len(cm.calls))
	}

	// 第二次输入应包含修正提示，且包含上一轮的失败输出作为 assistant 消息。
	second := cm.calls[1]
	found := false
	for _, m := range second {
		if m.Role == schema.User && strings.Contains(m.Content, "修正") && strings.Contains(m.Content, "JSON") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二次输入缺少修正提示: %+v", second)
	}
	hasAssistant := false
	for _, m := range second {
		if m.Role == schema.Assistant && strings.Contains(m.Content, "oops") {
			hasAssistant = true
		}
	}
	if !hasAssistant {
		t.Fatalf("第二次输入缺少上一轮失败输出: %+v", second)
	}
}

func TestGenerateIntoExhaustedAttempts(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"bad1", "bad2", "bad3"}}
	a := newTestAgent(t, cm)

	var out output
	err := a.GenerateInto(context.Background(), "json", &out)
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if len(cm.calls) != 3 {
		t.Fatalf("模型调用次数 = %d, want 3", len(cm.calls))
	}
	// 最后一次错误应是最后一轮的解析错误链。
	if !errors.Is(err, ErrNoJSON) {
		t.Fatalf("错误应包装最后一次解析错误 ErrNoJSON: %v", err)
	}
}

func TestGenerateIntoHistoryOnlyFinalRound(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"{bad", `{"name":"final","count":9}`}}
	a := newTestAgent(t, cm)

	var out output
	if err := a.GenerateInto(context.Background(), "user input", &out); err != nil {
		t.Fatalf("GenerateInto 失败: %v", err)
	}

	history, err := a.History(context.Background())
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("历史条数 = %d, want 2: %+v", len(history), history)
	}
	if history[0].Role != schema.User || history[0].Content != "user input" {
		t.Fatalf("历史 user = %+v", history[0])
	}
	if history[1].Role != schema.Assistant || history[1].Content != `{"name":"final","count":9}` {
		t.Fatalf("历史 assistant = %+v", history[1])
	}
}

func TestGenerateIntoUsageHookOnSuccess(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"```json\n{\"name\":\"u\",\"count\":2}\n```"}}
	var got *Usage
	a, err := New(context.Background(), Options{
		ChatModel: cm,
		UsageHook: func(_ context.Context, u *Usage) { got = u },
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}

	var out output
	if err := a.GenerateInto(context.Background(), "hi", &out); err != nil {
		t.Fatalf("GenerateInto 失败: %v", err)
	}
	if got == nil || got.TotalTokens != 5 {
		t.Fatalf("UsageHook 收到 = %+v, want total 5", got)
	}
}

func TestGenerateIntoExhaustedNoUsageHook(t *testing.T) {
	cm := &genRecorderModel{replies: []string{"bad", "bad", "bad"}}
	var called bool
	a, err := New(context.Background(), Options{
		ChatModel: cm,
		UsageHook: func(context.Context, *Usage) { called = true },
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}

	var out output
	if err := a.GenerateInto(context.Background(), "hi", &out); err == nil {
		t.Fatal("应当返回错误")
	}
	if called {
		t.Fatal("失败轮不应触发 UsageHook")
	}
}
