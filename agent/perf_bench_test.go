package agent

import (
	"context"
	"unicode/utf8"

	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 性能基准集合。为便于优化前后对比（benchstat 风格），
// 部分基准内置 "legacy" 子基准：内联复制旧实现路径，
// 与优化后的实现同轮运行，消除机器/环境差异。

// ---------------------------------------------------------------------------
// 公共构造
// ---------------------------------------------------------------------------

// benchModel 是仅用于构建 Agent 的空模型，不会被实际调用。
type benchModel struct{}

func (benchModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("", nil), nil
}
func (m benchModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}
func (m benchModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) { return m, nil }

// buildBenchHistory 生成 n 条混合 user/assistant/tool 历史消息。
// 所有轮次共享同一组指针（模拟 session 中指针身份逐轮稳定）。
func buildBenchHistory(n int) []*schema.Message {
	msgs := make([]*schema.Message, 0, n)
	for i := 0; i < n; i++ {
		switch i % 3 {
		case 0:
			msgs = append(msgs, schema.UserMessage("用户消息 "+string(rune('a'+i%26))+"：请帮我查询一下当前的系统状态并汇总结果，内容足够长以模拟真实对话。"))
		case 1:
			msgs = append(msgs, schema.UserMessage("中文消息，包含较多非 ASCII 字符以覆盖非 ASCII 快路径。今天天气很好，我们来讨论一下项目的进展情况吧。"))
		default:
			msgs = append(msgs, schema.AssistantMessage("assistant reply with some english content and a bit of 中文 mixed in for realism", nil))
		}
	}
	return msgs
}

// ---------------------------------------------------------------------------
// 1. TokenCompactor 缓存基准
// ---------------------------------------------------------------------------

// legacyEstimateTokens 内联复制旧版（单遍 range 扫描）实现，作为对比基线。
func legacyEstimateTokens(msg *schema.Message) int {
	if msg == nil {
		return 1
	}
	ascii, nonASCII := 0, 0
	for _, r := range msg.Content {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	for _, call := range msg.ToolCalls {
		for _, r := range call.Function.Name {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				nonASCII++
			}
		}
		for _, r := range call.Function.Arguments {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				nonASCII++
			}
		}
	}
	tokens := ascii/4 + nonASCII
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func legacyEstimateMessagesTokens(msgs []*schema.Message) int {
	total := 0
	for _, m := range msgs {
		total += legacyEstimateTokens(m)
	}
	return total
}

// legacyCompact 内联复制旧版 TokenCompactor.Compact（无缓存）路径。
func legacyCompact(c *TokenCompactor, _ context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
	total := legacyEstimateMessagesTokens(msgs)
	if total <= c.MaxTokens {
		return msgs, nil
	}
	headEnd := 1
	headTokens := legacyEstimateTokens(msgs[0])
	for headEnd < len(msgs) {
		t := legacyEstimateTokens(msgs[headEnd])
		if headTokens+t > c.KeepHeadTokens {
			break
		}
		headTokens += t
		headEnd++
	}
	for headEnd > 1 && headEnd < len(msgs) &&
		(msgs[headEnd].Role == schema.Tool || len(msgs[headEnd-1].ToolCalls) > 0) {
		headEnd--
	}
	headTokens = legacyEstimateMessagesTokens(msgs[:headEnd])
	tailBudget := c.MaxTokens - headTokens
	start := len(msgs)
	for tail := 0; start-1 >= headEnd; start-- {
		t := legacyEstimateTokens(msgs[start-1])
		if tail+t > tailBudget {
			break
		}
		tail += t
	}
	if start < headEnd {
		start = headEnd
	}
	if start == len(msgs) {
		out := make([]*schema.Message, 0, headEnd+1)
		out = append(out, msgs[:headEnd]...)
		out = append(out, schema.UserMessage("x"))
		return out, nil
	}
	if start < len(msgs) && msgs[start].Role != schema.User {
		aligned := false
		for i := start; i < len(msgs); i++ {
			if msgs[i].Role == schema.User {
				start = i
				aligned = true
				break
			}
		}
		if !aligned {
			hasUser := false
			for i := headEnd; i < len(msgs); i++ {
				if msgs[i].Role == schema.User {
					hasUser = true
					break
				}
			}
			if !hasUser {
				return nil, context.Canceled
			}
			start = len(msgs)
		}
	}
	if start == len(msgs) {
		out := make([]*schema.Message, 0, headEnd+1)
		out = append(out, msgs[:headEnd]...)
		out = append(out, schema.UserMessage("x"))
		return out, nil
	}
	out := make([]*schema.Message, 0, headEnd+len(msgs)-start+1)
	out = append(out, msgs[:headEnd]...)
	out = append(out, schema.UserMessage("x"))
	out = append(out, msgs[start:]...)
	return out, nil
}

// BenchmarkTokenCompactorCache 测量连续多轮 Compact 的开销：
// history=500，每 op 执行 10 轮 Compact（模拟长会话连续 10 次请求）。
// compact 历史超预算，每轮都会触发全量估算；缓存命中应消除重复估算。
func BenchmarkTokenCompactorCache(b *testing.B) {
	history := buildBenchHistory(500)
	c := NewTokenCompactor(2000, 500)
	if c == nil {
		b.Fatal("compactor 为空")
	}
	ctx := context.Background()

	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for round := 0; round < 10; round++ {
				if _, err := c.Compact(ctx, history); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("legacy_nocache", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for round := 0; round < 10; round++ {
				if _, err := legacyCompact(c, ctx, history); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

// BenchmarkTokenCompactorCacheUnderBudget 测量未超预算时的开销
// （每次请求都要前置计算 total，这是长会话最常见的路径）。
func BenchmarkTokenCompactorCacheUnderBudget(b *testing.B) {
	history := buildBenchHistory(500)
	c := NewTokenCompactor(1<<62, 500)
	ctx := context.Background()

	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for round := 0; round < 10; round++ {
				if _, err := c.Compact(ctx, history); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("legacy_nocache", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for round := 0; round < 10; round++ {
				if _, err := legacyCompact(c, ctx, history); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 2. FileSession 大历史 Messages 深拷贝基准
// ---------------------------------------------------------------------------

// BenchmarkFileSessionMessagesLarge 量化深拷贝成本随历史长度的变化。
// 结论（见任务报告）：深拷贝是语义正确性的必要代价，不做缓存优化。
func BenchmarkFileSessionMessagesLarge(b *testing.B) {
	for _, n := range []int{10, 100, 500, 1000} {
		b.Run("history_"+itoaB(n), func(b *testing.B) {
			path := b.TempDir() + "/bench.jsonl"
			s, err := NewFileSession(path)
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			for i := 0; i < n; i++ {
				msg := schema.UserMessage("历史消息 #：请帮我查询一下当前的系统状态并汇总结果，内容足够长以模拟真实对话长度。")
				if i%3 == 1 {
					msg = schema.AssistantMessage("assistant reply with english content and 中文 mixed", nil)
				}
				if err := s.Append(ctx, msg); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Messages(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCloneMessage 直接测量单条消息深拷贝。
func BenchmarkCloneMessage(b *testing.B) {
	withTool := schema.AssistantMessage("思考内容", nil)
	withTool.ToolCalls = []schema.ToolCall{
		{Function: schema.FunctionCall{Name: "search", Arguments: `{"query":"北京今天天气怎么样，适合出行吗","limit":10}`}},
	}
	user := schema.UserMessage("用户消息：请帮我查询一下当前的系统状态并汇总结果，内容足够长以模拟真实对话。")

	b.Run("user", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = cloneMessage(user)
		}
	})
	b.Run("assistant_toolcall", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = cloneMessage(withTool)
		}
	})
}

// ---------------------------------------------------------------------------
// 3. requestMessages 基准
// ---------------------------------------------------------------------------

// BenchmarkRequestMessages 测量每轮请求前的消息组装开销。
func BenchmarkRequestMessages(b *testing.B) {
	for _, sys := range []string{"", "你是一个专业的助手，系统提示词内容较长以模拟真实场景。请仔细分析用户的问题并给出准确的回答。"} {
		name := "nosystem"
		if sys != "" {
			name = "system"
		}
		b.Run(name, func(b *testing.B) {
			a, err := New(context.Background(), Options{
				ChatModel:    benchModel{},
				SystemPrompt: sys,
				Session:      NewMemorySession(),
			})
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			history := buildBenchHistory(500)
			if err := a.session.Append(ctx, history...); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := a.requestMessages(ctx, "本次用户输入"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. EstimateTokens 基准
// ---------------------------------------------------------------------------

// BenchmarkEstimateTokens 对比新旧估算实现。
func BenchmarkEstimateTokens(b *testing.B) {
	mixed := schema.UserMessage("用户消息：请帮我查询一下当前的系统状态并汇总结果，English content mixed in. ASCII 占比通常较高，like this sentence here.")
	cn := schema.UserMessage("纯中文消息：今天天气很好，我们来讨论一下项目的进展情况，看看接下来应该怎么做。")
	tool := schema.AssistantMessage("", nil)
	tool.ToolCalls = []schema.ToolCall{
		{Function: schema.FunctionCall{Name: "search", Arguments: `{"query":"北京今天天气怎么样，适合出行吗","limit":10,"extra":"some more json args to scan"}`}},
	}

	benches := []struct {
		name string
		msg  *schema.Message
	}{
		{"mixed", mixed},
		{"chinese", cn},
		{"toolcall", tool},
	}
	for _, tc := range benches {
		b.Run(tc.name+"/current", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = EstimateTokens(tc.msg)
			}
		})
		b.Run(tc.name+"/legacy", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = legacyEstimateTokens(tc.msg)
			}
		})
	}
}

// itoaB 避免基准文件依赖 fmt。
func itoaB(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
