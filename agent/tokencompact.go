package agent

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

// EstimateTokens 估算单条消息的 token 数。
// 近似规则：ASCII 字符约 4 字符 1 token；非 ASCII（中文等）约 1 字符 1 token；
// ToolCalls 的 JSON 参数同样参与估算；返回值不低于 1。
func EstimateTokens(msg *schema.Message) int {
	if msg == nil {
		return 1
	}
	ascii, nonASCII := countTokens(msg.Content)
	// ToolCalls 的 JSON 参数与工具名同样参与估算。
	for _, call := range msg.ToolCalls {
		a, n := countTokens(call.Function.Name)
		ascii, nonASCII = ascii+a, nonASCII+n
		a, n = countTokens(call.Function.Arguments)
		ascii, nonASCII = ascii+a, nonASCII+n
	}
	tokens := ascii/4 + nonASCII
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// countTokens 单遍扫描字符串，统计 ASCII 字符数与非 ASCII 字符数。
// Content 与 ToolCalls 的 Name/Arguments 复用同一实现，消除重复扫描代码。
//
// 实测（BenchmarkEstimateTokens）：逐字节 + DecodeRuneInString 的写法
// 比 range 扫描慢 15%~25%（range 的 rune 解码为编译器内联优化路径），
// 因此保持 range 实现。
func countTokens(s string) (ascii, nonASCII int) {
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	return ascii, nonASCII
}

// EstimateMessagesTokens 估算一组消息的总 token 数。
func EstimateMessagesTokens(msgs []*schema.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m)
	}
	return total
}

// TokenCompactor 按 token 预算截断历史，实现 Compactor 接口（见 compact.go）。
// 策略与 TruncateCompactor 一致但以 token 为单位：
//   - 预算 = MaxTokens；从头累计 KeepHeadTokens 预算保留开头消息；
//   - 剩余预算（MaxTokens - 头部实际占用）从消息末尾向前取；
//   - 截断点对齐 user 消息边界（不可切开 assistant 工具调用与 tool 响应对）；
//   - 超限时在头尾之间插入一条 user 角色的截断说明消息
//     （文案与 TruncateCompactor 相同风格："…已因超出上下文限制被截断"，
//     说明中给出被截断的消息条数）；
//   - 未超预算时原样返回。
//
// 边界情况约定：
//   - 头部按整条消息累计，至少保留第一条消息（保证系统级上下文前缀存在）；
//   - 若 KeepHeadTokens 超过 MaxTokens 的一半，则压到一半，
//     避免头部预算吃光整体预算导致尾段无法保留（与 TruncateCompactor 一致）；
//   - 尾段预算为 0 或所有剩余消息都放不进尾段时，仅保留头部与截断说明；
//   - 头部保留区之后找不到 user 消息边界可对齐时返回 error。
type TokenCompactor struct {
	// MaxTokens 为压缩后允许的最大估算 token 数。
	MaxTokens int
	// KeepHeadTokens 为开头固定保留的 token 预算（维持系统级上下文的稳定前缀，
	// 利于缓存命中）。按整条消息累计，不超过预算的最长消息前缀会被保留。
	KeepHeadTokens int

	// tokenCache 缓存每条消息的估算结果：key 为 *schema.Message 指针，
	// value 为 token 数。EstimateTokens 对同一消息指针是确定性纯函数，
	// 而调用方传入的 msgs 来自 session.Messages 的副本，其中元素指针
	// 逐轮身份稳定（session 缓存同一组指针；调用方拿到并修改的是副本，
	// 不会替换 session 内部指针），因此缓存命中安全。
	//
	// 假设与限制（文档说明）：
	//   - 缓存无上限增长：被 GC 回收的消息指针对应的条目会残留到
	//     compactor 生命周期结束（指针本身可被 GC，仅少量 int 残留），
	//     属可接受的权衡；长生命周期 compactor 可调用 ResetCache 主动清理。
	//   - 跨实例/跨会话共享 compactor 时指针身份仍唯一，缓存安全。
	//   - 若调用方绕过 session、每轮重新构造消息指针，则缓存退化为
	//     miss-only，行为仍正确（只是无加速）。
	tokenCache sync.Map

	// nocache 为内部测试/基准开关：置位时绕过缓存走原始估算路径。
	nocache bool
}

// ResetCache 清空 token 估算缓存。
// 仅需在长生命周期 compactor 中定期调用以释放残留条目；
// 常规用法（compactor 与 Agent 同生命周期）无需调用。
func (c *TokenCompactor) ResetCache() {
	c.tokenCache.Range(func(key, _ any) bool {
		c.tokenCache.Delete(key)
		return true
	})
}

// estimateTokens 返回单条消息的 token 数，优先命中缓存。
func (c *TokenCompactor) estimateTokens(msg *schema.Message) int {
	if c.nocache {
		return EstimateTokens(msg)
	}
	if v, ok := c.tokenCache.Load(msg); ok {
		return v.(int)
	}
	n := EstimateTokens(msg)
	c.tokenCache.Store(msg, n)
	return n
}

// estimateMessagesTokens 返回一组消息的总 token 数（带缓存）。
func (c *TokenCompactor) estimateMessagesTokens(msgs []*schema.Message) int {
	total := 0
	for _, m := range msgs {
		total += c.estimateTokens(m)
	}
	return total
}

// NewTokenCompactor 创建按 token 预算截断的压缩器。
// maxTokens<=0 时返回 nil（不压缩）；keepHeadTokens<0 归 0。
func NewTokenCompactor(maxTokens, keepHeadTokens int) *TokenCompactor {
	if maxTokens <= 0 {
		return nil
	}
	if keepHeadTokens < 0 {
		keepHeadTokens = 0
	}
	if keepHeadTokens > maxTokens/2 {
		keepHeadTokens = maxTokens / 2
	}
	return &TokenCompactor{MaxTokens: maxTokens, KeepHeadTokens: keepHeadTokens}
}

// Compact 超过 token 预算时截断历史，其余情况原样返回。
func (c *TokenCompactor) Compact(_ context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
	total := c.estimateMessagesTokens(msgs)
	if total <= c.MaxTokens {
		return msgs, nil
	}

	// 头部：按整条消息累计，保留不超过 KeepHeadTokens 预算的最长前缀；
	// 至少保留第一条消息（保证系统级上下文前缀存在）。
	headEnd := 1
	headTokens := c.estimateTokens(msgs[0])
	for headEnd < len(msgs) {
		t := c.estimateTokens(msgs[headEnd])
		if headTokens+t > c.KeepHeadTokens {
			break
		}
		headTokens += t
		headEnd++
	}

	// 头部边界不能切开 assistant 工具调用与 tool 响应对：
	// 若边界落在工具响应中间，或紧邻的前一条是带工具调用的 assistant
	// （其 tool 响应将被丢进截断区），则向前收缩头部边界。
	for headEnd > 1 && headEnd < len(msgs) &&
		(msgs[headEnd].Role == schema.Tool || len(msgs[headEnd-1].ToolCalls) > 0) {
		headEnd--
	}
	headTokens = c.estimateMessagesTokens(msgs[:headEnd])

	// 尾部：用剩余预算从末尾向前取整条消息。
	tailBudget := c.MaxTokens - headTokens
	start := len(msgs)
	for tail := 0; start-1 >= headEnd; start-- {
		t := c.estimateTokens(msgs[start-1])
		if tail+t > tailBudget {
			break
		}
		tail += t
	}
	if start < headEnd {
		start = headEnd
	}
	// 尾段预算为 0 或剩余消息都放不进尾段（start == len(msgs)）：
	// 只保留头部与截断说明，被截断的是头部保留区之后的全部消息；
	// 无尾段内容，不存在切开工具调用对的风险，无需对齐 user 边界。
	if start == len(msgs) {
		out := make([]*schema.Message, 0, headEnd+1)
		out = append(out, msgs[:headEnd]...)
		out = append(out, schema.UserMessage(fmt.Sprintf(
			"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", len(msgs)-headEnd)))
		return out, nil
	}
	// 对齐 user 边界：截断点必须落在 user 消息上，
	// 避免把 assistant 工具调用与其 tool 响应切开。
	if start < len(msgs) && msgs[start].Role != schema.User {
		// 先在窗口内向前找（多丢一些，取最早的 user 作为安全截断点）。
		aligned := false
		for i := start; i < len(msgs); i++ {
			if msgs[i].Role == schema.User {
				start = i
				aligned = true
				break
			}
		}
		if !aligned {
			// 窗口内没有 user 边界（如结尾是一段连续工具调用）。
			// 向前回退会保留超出尾段预算的消息、撑爆 token 预算，
			// 因此直接丢弃整个尾段窗口；头部保留区之后完全没有
			// user 边界则按约定返回 error。
			hasUser := false
			for i := headEnd; i < len(msgs); i++ {
				if msgs[i].Role == schema.User {
					hasUser = true
					break
				}
			}
			if !hasUser {
				return nil, fmt.Errorf("agent: 历史压缩失败：%d 条消息中找不到 user 边界", len(msgs))
			}
			start = len(msgs)
		}
	}
	if start == len(msgs) {
		out := make([]*schema.Message, 0, headEnd+1)
		out = append(out, msgs[:headEnd]...)
		out = append(out, schema.UserMessage(fmt.Sprintf(
			"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", len(msgs)-headEnd)))
		return out, nil
	}

	out := make([]*schema.Message, 0, headEnd+len(msgs)-start+1)
	out = append(out, msgs[:headEnd]...)
	// 插入一条说明性 user 消息，告知模型早期上下文已被截断，
	// 同时充当尾段消息 tool/user 约束的缓冲边界。
	out = append(out, schema.UserMessage(fmt.Sprintf(
		"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", start-headEnd)))
	out = append(out, msgs[start:]...)
	return out, nil
}
