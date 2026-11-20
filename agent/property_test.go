package agent

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// 本文件为两个压缩器（TruncateCompactor / TokenCompactor）的属性测试：
// 使用手写随机生成器构造合法的 ReAct 会话序列，对每个随机用例校验压缩输出
// 必须满足的不变量（工具调用对不切开、条数/token 预算、头部保留、恒等输出）。
// 不使用第三方属性测试库；随机种子固定，失败时打印种子与用例序号以便复现。

const (
	// propTruncateBase / propTokenBase 为两组属性用例的随机种子基数（固定以便复现）。
	propTruncateBase = int64(20261121)
	propTokenBase    = int64(20261122)
	// propCases 为每个压缩器的随机用例数。
	propCases = 250
)

// propZhChars / propEnChars 用于生成中英文混合的随机内容。
var (
	propZhChars = []rune("数据压缩测试代理会话消息工具调用历史上下文截断摘要系统用户问答处理结果参数说明")
	propEnChars = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// randContent 生成长度 0~maxLen 的中英文混合随机字符串。
func randContent(rng *rand.Rand, maxLen int) string {
	n := rng.Intn(maxLen + 1)
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		if rng.Intn(2) == 0 {
			b.WriteRune(propZhChars[rng.Intn(len(propZhChars))])
		} else {
			b.WriteByte(propEnChars[rng.Intn(len(propEnChars))])
		}
	}
	return b.String()
}

// genHistory 生成合法的 ReAct 会话序列：
// 以 user 消息开头，主体按 [user, assistant] 对为主，
// 以一定概率生成 [assistant(带 ToolCalls), tool...] 对；
// tool 消息的 ToolCallID 与对应 assistant 的 ToolCall.ID 严格配对。
func genHistory(t *testing.T, rng *rand.Rand) []*schema.Message {
	t.Helper()
	// 首条消息为系统级前缀，保持较短（设计上"至少保留第一条消息"，
	// 首条本身超预算时输出必然超出 MaxTokens）。
	msgs := []*schema.Message{schema.UserMessage(randContent(rng, 40))}
	rounds := 1 + rng.Intn(15)
	callSeq := 0
	for i := 0; i < rounds; i++ {
		if rng.Intn(10) < 3 {
			// 工具调用轮：生成 [assistant(带 ToolCalls), tool] 对，
			// ToolCallID 与 tool 响应严格一对一配对。
			id := fmt.Sprintf("call-%d", callSeq)
			callSeq++
			msgs = append(msgs,
				schema.AssistantMessage(randContent(rng, 100), []schema.ToolCall{{
					ID:   id,
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "lookup",
						Arguments: fmt.Sprintf(`{"q":%q}`, randContent(rng, 40)),
					},
				}}),
				schema.ToolMessage(randContent(rng, 60), id))
			continue
		}
		msgs = append(msgs,
			schema.UserMessage(randContent(rng, 200)),
			schema.AssistantMessage(randContent(rng, 200), nil))
	}
	// 生成器自身的合法性自检：输入必须满足工具调用对约束。
	checkToolPairs(t, "generator", 0, -1, msgs)
	return msgs
}

// checkToolPairs 校验消息序列中工具调用对的完整性：
//   - 任何 tool 消息的前一条必须是 assistant 工具调用消息，
//     且其 ToolCallID 能对应该 assistant 的某个 ToolCall.ID；
//   - 任何带 ToolCalls 的 assistant，其下一条必须是匹配的 tool 响应。
func checkToolPairs(t *testing.T, tag string, seed int64, caseIdx int, msgs []*schema.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role == schema.Tool {
			if i == 0 {
				t.Errorf("[%s seed=%d case=%d] 第 0 条是 tool 消息，缺少前置 assistant 工具调用", tag, seed, caseIdx)
				return
			}
			p := msgs[i-1]
			matched := p.Role == schema.Assistant && len(p.ToolCalls) > 0
			if matched {
				matched = false
				for _, tc := range p.ToolCalls {
					if tc.ID == m.ToolCallID {
						matched = true
						break
					}
				}
			}
			if !matched {
				t.Errorf("[%s seed=%d case=%d] 第 %d 条 tool 消息（ToolCallID=%q）前一条不是同 ID 的 assistant 工具调用", tag, seed, caseIdx, i, m.ToolCallID)
				return
			}
		}
		if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
			if i+1 >= len(msgs) || msgs[i+1].Role != schema.Tool {
				t.Errorf("[%s seed=%d case=%d] 第 %d 条 assistant 工具调用的 tool 响应被切开", tag, seed, caseIdx, i)
				return
			}
			matched := false
			for _, tc := range m.ToolCalls {
				if tc.ID == msgs[i+1].ToolCallID {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("[%s seed=%d case=%d] 第 %d 条 assistant 的首个 tool 响应 ID 不匹配", tag, seed, caseIdx, i)
				return
			}
		}
	}
}

// propNoticeIndex 返回截断说明消息在输出中的下标；不存在时返回 -1。
func propNoticeIndex(out []*schema.Message) int {
	for i, m := range out {
		if strings.Contains(m.Content, "已因超出上下文限制被截断") {
			return i
		}
	}
	return -1
}

// propCheckHead 校验头部保留区（截断说明之前的前缀）与输入完全一致（指针级）。
func propCheckHead(t *testing.T, tag string, seed int64, caseIdx int, in, out []*schema.Message) {
	t.Helper()
	n := propNoticeIndex(out)
	if n < 0 {
		return // 未截断，由恒等性检查覆盖
	}
	for i := 0; i < n; i++ {
		if i >= len(in) || out[i] != in[i] {
			t.Errorf("[%s seed=%d case=%d] 头部保留区第 %d 条与输入不一致", tag, seed, caseIdx, i)
			return
		}
	}
}

// propCheckIdentity 校验未超预算时输出与输入完全一致（同一底层数组、同序同指针）。
func propCheckIdentity(t *testing.T, tag string, seed int64, caseIdx int, in, out []*schema.Message) {
	t.Helper()
	if len(out) != len(in) {
		t.Errorf("[%s seed=%d case=%d] 未超预算被压缩: %d -> %d 条", tag, seed, caseIdx, len(in), len(out))
		return
	}
	if len(in) > 0 && &out[0] != &in[0] {
		t.Errorf("[%s seed=%d case=%d] 未超预算时输出应与输入为同一消息切片", tag, seed, caseIdx)
	}
}

func TestPropertyTruncateCompactor(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(propTruncateBase))
	for i := 0; i < propCases; i++ {
		msgs := genHistory(t, rng)
		maxMessages := 4 + rng.Intn(30)
		keepHead := rng.Intn(maxMessages/2 + 1)
		c := NewTruncateCompactor(maxMessages, keepHead)
		if c == nil {
			t.Fatalf("seed=%d case=%d: MaxMessages=%d 不应返回 nil", propTruncateBase, i, maxMessages)
		}

		out, err := c.Compact(ctx, msgs)
		if err != nil {
			// 仅当头部保留区之后完全不存在 user 边界时允许报错。
			if !strings.Contains(err.Error(), "找不到 user 边界") {
				t.Errorf("seed=%d case=%d: 意外错误 %v", propTruncateBase, i, err)
			}
			continue
		}

		// 不变量 1：工具调用对不被切开。
		checkToolPairs(t, "truncate", propTruncateBase, i, out)
		// 不变量 2：输出条数不超过 MaxMessages+1（+1 为截断说明）。
		if len(out) > maxMessages+1 {
			t.Errorf("seed=%d case=%d: 输出条数 %d 超过 MaxMessages(%d)+1", propTruncateBase, i, len(out), maxMessages)
		}
		// 不变量 3：头部保留区与输入一致。
		propCheckHead(t, "truncate", propTruncateBase, i, msgs, out)

		// 不变量 4：未超预算时输出与输入完全一致。
		identity, err := NewTruncateCompactor(len(msgs)+3, 0).Compact(ctx, msgs)
		if err != nil {
			t.Errorf("seed=%d case=%d: 宽预算下不应报错: %v", propTruncateBase, i, err)
		} else {
			propCheckIdentity(t, "truncate-identity", propTruncateBase, i, msgs, identity)
		}
	}
}

func TestPropertyTokenCompactor(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(propTokenBase))
	for i := 0; i < propCases; i++ {
		msgs := genHistory(t, rng)
		maxTokens := 60 + rng.Intn(340)
		keepHeadTokens := rng.Intn(maxTokens/2 + 1)
		c := NewTokenCompactor(maxTokens, keepHeadTokens)
		if c == nil {
			t.Fatalf("seed=%d case=%d: MaxTokens=%d 不应返回 nil", propTokenBase, i, maxTokens)
		}

		out, err := c.Compact(ctx, msgs)
		if err != nil {
			// 仅当头部保留区之后完全不存在 user 边界时允许报错。
			if !strings.Contains(err.Error(), "找不到 user 边界") {
				t.Errorf("seed=%d case=%d: 意外错误 %v", propTokenBase, i, err)
			}
			continue
		}

		// 不变量 1：工具调用对不被切开。
		checkToolPairs(t, "token", propTokenBase, i, out)
		// 不变量 2：输出估算 token 不超预算。容差来源：截断说明消息本身
		// 不计入尾段预算（与 tokencompact_test.go 既有口径一致），故总预算
		// 放宽 MaxTokens + 说明自身 token 数，另留 10 容差应对说明文案计数。
		got := EstimateMessagesTokens(out)
		noticeTokens := 0
		if n := propNoticeIndex(out); n >= 0 {
			noticeTokens = EstimateTokens(out[n])
			if out[n].Role != schema.User {
				t.Errorf("seed=%d case=%d: 截断说明应为 user 角色", propTokenBase, i)
			}
		}
		if EstimateTokens(msgs[0]) > maxTokens {
			// 首条消息本身超出 MaxTokens：按"至少保留第一条消息"的约定，
			// 输出必然超出预算，该预算不变量不适用（注释说明容差来源）。
			continue
		}
		if got > maxTokens+noticeTokens+10 {
			t.Errorf("seed=%d case=%d: 输出 token %d 超过预算 MaxTokens(%d)+说明(%d)+10", propTokenBase, i, got, maxTokens, noticeTokens)
		}
		// 不变量 3：头部保留区与输入一致。
		propCheckHead(t, "token", propTokenBase, i, msgs, out)

		// 不变量 4：未超预算时输出与输入完全一致。
		total := EstimateMessagesTokens(msgs)
		identity, err := NewTokenCompactor(total, total).Compact(ctx, msgs)
		if err != nil {
			t.Errorf("seed=%d case=%d: 宽预算下不应报错: %v", propTokenBase, i, err)
		} else {
			propCheckIdentity(t, "token-identity", propTokenBase, i, msgs, identity)
		}
	}
}

// 以下为针对已修复缺陷的回归测试（固定构造用例，不依赖随机）。

func TestRegressionTruncateHeadSplitsToolPair(t *testing.T) {
	// KeepHead 边界落在 assistant 工具调用与 tool 响应之间时，
	// 应收缩头部边界而不是切开工具对（历史上头部会保留悬空的 assistant）。
	ctx := context.Background()
	c := NewTruncateCompactor(4, 1)
	msgs := []*schema.Message{
		schema.UserMessage("任务说明"),
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "call-0", Type: "function", Function: schema.FunctionCall{Name: "lookup", Arguments: `{}`}},
		}),
		schema.ToolMessage("结果", "call-0"),
		schema.UserMessage("继续"),
		schema.AssistantMessage("好的", nil),
		schema.UserMessage("下一个问题"),
	}
	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	checkToolPairs(t, "regression-head-pair", 0, 0, out)
}

func TestRegressionTruncateNoUserInWindow(t *testing.T) {
	// 尾部窗口内没有 user 边界且结尾是连续工具调用时，
	// 不应向前回退导致头部保留区与尾段重叠重复保留（历史缺陷：start 回退到 0 造成消息重复）。
	ctx := context.Background()
	c := NewTruncateCompactor(4, 1)
	msgs := []*schema.Message{
		schema.UserMessage("任务说明"),
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "call-0", Type: "function", Function: schema.FunctionCall{Name: "lookup", Arguments: `{}`}},
		}),
		schema.ToolMessage("结果一", "call-0"),
		schema.ToolMessage("结果二", "call-0"),
		schema.AssistantMessage("汇总", nil),
	}
	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	checkToolPairs(t, "regression-no-user", 0, 0, out)
	if len(out) > 4+1 {
		t.Fatalf("输出条数 = %d, 超过 MaxMessages+1", len(out))
	}
	// 不应出现重复消息：任何一条消息（指针级）在输出中至多出现一次。
	seen := make(map[*schema.Message]bool)
	for _, m := range out {
		if seen[m] {
			t.Fatal("头部保留区与回退尾段重叠，消息重复保留")
		}
		seen[m] = true
	}
}

func TestRegressionTokenTailOverflow(t *testing.T) {
	// 尾段窗口切在工具对中间且窗口内无 user 边界时，
	// 不应向前回退保留超出尾段预算的消息（历史缺陷：输出 token 失控）。
	ctx := context.Background()
	c := NewTokenCompactor(40, 0)
	msgs := []*schema.Message{
		schema.UserMessage("触发工具调用的开头"),
	}
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
	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	checkToolPairs(t, "regression-token-overflow", 0, 0, out)
	got := EstimateMessagesTokens(out)
	noticeTokens := 0
	if n := propNoticeIndex(out); n >= 0 {
		noticeTokens = EstimateTokens(out[n])
	}
	if got > 40+noticeTokens+10 {
		t.Fatalf("输出 token = %d, 超过预算 40+说明(%d)+10", got, noticeTokens)
	}
}

func TestRegressionTokenHeadSplitsToolPair(t *testing.T) {
	// KeepHeadTokens 边界落在工具对中间时，应收缩头部边界而不是保留悬空的
	// assistant 工具调用（其 tool 响应被丢进截断区）。
	ctx := context.Background()
	c := NewTokenCompactor(100, 15)
	msgs := []*schema.Message{
		schema.UserMessage("头部说明甲乙丙丁"),                       // 8 tokens
		schema.AssistantMessage("", []schema.ToolCall{        // 约 3 tokens
			{Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"abcd"}`}},
		}),
		schema.ToolMessage("结果戊己庚", "id-0"), // 5 tokens
		schema.UserMessage(strings.Repeat("长", 60)),
		schema.AssistantMessage(strings.Repeat("应", 60), nil),
	}
	out, err := c.Compact(ctx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	checkToolPairs(t, "regression-token-head-pair", 0, 0, out)
}
