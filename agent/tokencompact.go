package agent

import (
	"context"
	"fmt"
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
	ascii, nonASCII := 0, 0
	for _, r := range msg.Content {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	// ToolCalls 的 JSON 参数与工具名同样参与估算。
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
	total := EstimateMessagesTokens(msgs)
	if total <= c.MaxTokens {
		return msgs, nil
	}

	// 头部：按整条消息累计，保留不超过 KeepHeadTokens 预算的最长前缀；
	// 至少保留第一条消息（保证系统级上下文前缀存在）。
	headEnd := 1
	headTokens := EstimateTokens(msgs[0])
	for headEnd < len(msgs) {
		t := EstimateTokens(msgs[headEnd])
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
	headTokens = EstimateMessagesTokens(msgs[:headEnd])

	// 尾部：用剩余预算从末尾向前取整条消息。
	tailBudget := c.MaxTokens - headTokens
	start := len(msgs)
	for tail := 0; start-1 >= headEnd; start-- {
		t := EstimateTokens(msgs[start-1])
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
