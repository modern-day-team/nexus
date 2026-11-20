package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// Compactor 在每次请求前压缩会话历史（请求时压缩，不回写会话）。
// 典型场景：长会话超出模型上下文窗口时，自动丢弃或摘要早期消息。
type Compactor interface {
	// Compact 返回压缩后的历史。实现应保证返回的消息仍满足
	// "tool 消息必须紧跟对应的 assistant 工具调用消息" 的约束。
	Compact(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, error)
}

// TruncateCompactor 是默认的历史截断压缩器。
// 保留开头 KeepHead 条消息（维持系统级上下文的稳定前缀，利于缓存命中）
// 与最近 MaxMessages-KeepHead 条消息；截断点对齐到 user 消息边界，
// 避免把 assistant 工具调用与其 tool 响应切开。
type TruncateCompactor struct {
	// MaxMessages 为压缩后允许的最大历史条数。
	MaxMessages int
	// KeepHead 为开头固定保留的条数（如会话开头的任务说明）。
	// 若超过 MaxMessages 的一半则取一半。
	KeepHead int
}

// NewTruncateCompactor 创建截断压缩器。MaxMessages<=0 时返回 nil（不压缩）。
func NewTruncateCompactor(maxMessages, keepHead int) *TruncateCompactor {
	if maxMessages <= 0 {
		return nil
	}
	if keepHead < 0 {
		keepHead = 0
	}
	if keepHead > maxMessages/2 {
		keepHead = maxMessages / 2
	}
	return &TruncateCompactor{MaxMessages: maxMessages, KeepHead: keepHead}
}

// Compact 超限时截断历史，其余情况原样返回。
func (c *TruncateCompactor) Compact(_ context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
	if len(msgs) <= c.MaxMessages {
		return msgs, nil
	}

	// 头部边界不能切开 assistant 工具调用与 tool 响应对：
	// 若边界落在工具响应中间，或紧邻的前一条是带工具调用的 assistant
	// （其 tool 响应将被丢进截断区），则向前收缩头部边界。
	headKeep := c.KeepHead
	for headKeep > 0 && headKeep < len(msgs) &&
		(msgs[headKeep].Role == schema.Tool || len(msgs[headKeep-1].ToolCalls) > 0) {
		headKeep--
	}

	keepTail := c.MaxMessages - c.KeepHead
	// 找到尾部窗口的起点：最早且距末尾不超过 keepTail 的 user 消息。
	start := len(msgs) - keepTail
	if start < headKeep {
		start = headKeep
	}
	for i := start; i < len(msgs); i++ {
		if msgs[i].Role == schema.User {
			// 继续向前看是否还有更早的 user 消息在窗口内，
			// 取最早的一个作为安全截断点。
			start = i
			break
		}
	}
	// 窗口内没有 user 消息边界可对齐（如结尾是一段连续工具调用）：
	// 向前回退会把头部保留区之外的消息重复保留、撑爆条数预算，
	// 因此直接丢弃整个尾段窗口；整段历史完全找不到 user 边界则返回 error。
	found := start < len(msgs) && msgs[start].Role == schema.User
	if !found {
		hasUser := false
		for i := 0; i < len(msgs); i++ {
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

	if start == len(msgs) {
		// 无尾段可保留：仅返回完整头部前缀与截断说明。
		notice := schema.UserMessage(fmt.Sprintf(
			"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", len(msgs)-headKeep))
		return append(msgs[:headKeep:headKeep], notice), nil
	}

	out := make([]*schema.Message, 0, headKeep+len(msgs)-start+1)
	out = append(out, msgs[:headKeep]...)
	// 插入一条说明性 user 消息，告知模型早期上下文已被截断，
	// 同时充当尾段消息 tool/user 约束的缓冲边界。
	notice := schema.UserMessage(fmt.Sprintf(
		"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", start-headKeep))
	out = append(out, notice)
	out = append(out, msgs[start:]...)
	return out, nil
}
