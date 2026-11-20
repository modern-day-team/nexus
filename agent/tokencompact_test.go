package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestEstimateTokens(t *testing.T) {
	// 纯 ASCII：4 字符约 1 token。
	ascii := schema.UserMessage("abcdefgh") // 8 ASCII -> 2 tokens
	if got := EstimateTokens(ascii); got != 2 {
		t.Fatalf("ASCII 估算 = %d, want 2", got)
	}
	// 纯中文：1 字符 1 token。
	cn := schema.UserMessage("你好世界") // 4 非ASCII -> 4 tokens
	if got := EstimateTokens(cn); got != 4 {
		t.Fatalf("中文估算 = %d, want 4", got)
	}
	// 混合：ASCII/4 + 非ASCII。
	mixed := schema.UserMessage("abcd你好") // 1 + 2 = 3
	if got := EstimateTokens(mixed); got != 3 {
		t.Fatalf("混合估算 = %d, want 3", got)
	}
	// ToolCalls 的 JSON 参数参与估算。
	call := schema.AssistantMessage("", []schema.ToolCall{
		{Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"abcdefgh"}`}},
	})
	// 4 + 18 = 22 ASCII -> 5 tokens + 工具名 4 -> 6? name 4 chars + args 18 chars = 22 -> 5.
	if got := EstimateTokens(call); got != 5 {
		t.Fatalf("工具调用估算 = %d, want 5", got)
	}
	// 空内容返回不低于 1。
	if got := EstimateTokens(schema.UserMessage("")); got != 1 {
		t.Fatalf("空消息估算 = %d, want 1", got)
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("abcd"), // 1
		schema.UserMessage("你好"),   // 2
	}
	if got := EstimateMessagesTokens(msgs); got != 3 {
		t.Fatalf("总估算 = %d, want 3", got)
	}
}

func TestTokenCompactorUnderBudget(t *testing.T) {
	c := NewTokenCompactor(1000, 10)
	if c == nil {
		t.Fatal("expected non-nil compactor")
	}
	msgs := []*schema.Message{
		schema.UserMessage("短问题一"),
		schema.AssistantMessage("短回答一", nil),
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("未超预算被压缩: %d -> %d", len(msgs), len(out))
	}
}

func TestTokenCompactorTruncates(t *testing.T) {
	c := NewTokenCompactor(60, 0)
	ctx := context.Background()

	// 构造超限历史：每轮约 6 token（"问题"=2 + "回答"=2 + 杂项），
	// 粗略以条数为准：11 条 x ~6 >= 60 必超限。
	msgs := []*schema.Message{
		schema.UserMessage("任务说明（需保留的头部内容）"),
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			schema.UserMessage("这轮的问题内容比较长一些"),
			schema.AssistantMessage("这轮的回答内容也比较长一些哦", nil),
		)
	}
	total := EstimateMessagesTokens(msgs)

	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	// 截断说明会额外占用一点 token，允许少量超出。
	if got := EstimateMessagesTokens(out); got > total {
		t.Fatalf("截断后总 token 反而增加: %d > %d", got, total)
	}
	if got := EstimateMessagesTokens(out); got > c.MaxTokens+EstimateTokens(out[1])+10 {
		t.Fatalf("截断后总预算失控: %d", got)
	}
	if out[0].Content != "任务说明（需保留的头部内容）" {
		t.Fatalf("KeepHeadTokens=0 仍应保留第一条消息: %q", out[0].Content)
	}
	// 截断说明存在且为 user 角色，位于头尾之间。
	foundNotice := false
	for i, m := range out {
		if strings.Contains(m.Content, "已因超出上下文限制被截断") {
			foundNotice = true
			if m.Role != schema.User {
				t.Fatalf("说明消息应为 user 角色，第 %d 条不是", i)
			}
			if i == 0 || i == len(out)-1 {
				t.Fatal("说明消息应位于头尾之间")
			}
			break
		}
	}
	if !foundNotice {
		t.Fatal("缺少截断说明消息")
	}
	// 尾段必须从 user 边界开始。
	tailStart := 2
	if out[tailStart].Role != schema.User {
		t.Fatalf("尾段应从 user 边界开始, got %s", out[tailStart].Role)
	}
}

func TestTokenCompactorKeepsHead(t *testing.T) {
	// 头部预算保留开头多条消息。
	c := NewTokenCompactor(80, 30)
	head1 := schema.UserMessage("头部说明甲乙丙丁") // 8 tokens
	head2 := schema.UserMessage("头部说明戊己庚辛") // 8 tokens
	msgs := []*schema.Message{head1, head2}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			schema.UserMessage("中间的问题内容比较长一些"),
			schema.AssistantMessage("中间的回答内容也比较长一些哦", nil),
		)
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 3 || out[0] != head1 || out[1] != head2 {
		t.Fatalf("头部两条消息应被保留: %d 条", len(out))
	}
}

func TestTokenCompactorKeepsToolPair(t *testing.T) {
	// 尾段窗口切在 assistant 工具调用与 tool 响应之间时，应回退到 user 边界。
	c := NewTokenCompactor(40, 0)
	msgs := []*schema.Message{
		schema.UserMessage("触发工具调用的开头"),
	}
	// 每条约 20+ token，两轮必然超限。
	for i := 0; i < 3; i++ {
		args := `{"text":"` + strings.Repeat("a", 60) + `"}`
		msgs = append(msgs,
			schema.UserMessage("这轮的问题"+strings.Repeat("b", 40)),
			schema.AssistantMessage("", []schema.ToolCall{
				{Function: schema.FunctionCall{Name: "echo", Arguments: args}},
			}),
			schema.ToolMessage("tool result "+strings.Repeat("c", 40), "id"),
		)
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	// 校验尾部 assistant 工具调用与其 tool 响应必须成对保留。
	for i, m := range out {
		if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
			if i+1 >= len(out) || out[i+1].Role != schema.Tool {
				t.Fatalf("第 %d 条 assistant 工具调用的 tool 响应被切开", i)
			}
		}
		if m.Role == schema.Tool && (i == 0 || out[i-1].Role != schema.Assistant || len(out[i-1].ToolCalls) == 0) {
			t.Fatalf("第 %d 条 tool 消息缺少前面的 assistant 工具调用", i)
		}
	}
	// 所有 tool 响应必须在截断说明之后（即说明之前不出现 tool 消息）。
	for i, m := range out {
		if strings.Contains(m.Content, "已因超出上下文限制被截断") {
			for _, before := range out[:i] {
				if before.Role == schema.Tool {
					t.Fatal("tool 响应不应出现在截断说明之前")
				}
			}
			break
		}
	}
}

func TestTokenCompactorNoUserBoundary(t *testing.T) {
	// 头部保留区之后全是 assistant/tool 消息，找不到 user 边界应返回 error。
	c := NewTokenCompactor(30, 10)
	msgs := []*schema.Message{
		schema.UserMessage("开头一条较长的消息内容"),
		schema.AssistantMessage("", []schema.ToolCall{
			{Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"` + strings.Repeat("a", 60) + `"}`}},
		}),
		schema.ToolMessage("tool result "+strings.Repeat("b", 60), "id"),
	}
	if _, err := c.Compact(context.Background(), msgs); err == nil {
		t.Fatal("找不到 user 边界应返回 error")
	}
}

func TestNewTokenCompactorInvalid(t *testing.T) {
	if NewTokenCompactor(0, 0) != nil {
		t.Fatal("MaxTokens<=0 应返回 nil")
	}
	if NewTokenCompactor(-5, 3) != nil {
		t.Fatal("MaxTokens<0 应返回 nil")
	}
	if c := NewTokenCompactor(100, -1); c.KeepHeadTokens != 0 {
		t.Fatalf("KeepHeadTokens<0 应归 0, got %d", c.KeepHeadTokens)
	}
	// KeepHeadTokens 超过 MaxTokens 一半应压到一半。
	if c := NewTokenCompactor(100, 90); c.KeepHeadTokens != 50 {
		t.Fatalf("KeepHeadTokens = %d, want 50（不超过一半）", c.KeepHeadTokens)
	}
}
