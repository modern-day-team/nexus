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

	keepTail := c.MaxMessages - c.KeepHead
	// 找到尾部窗口的起点：最早且距末尾不超过 keepTail 的 user 消息。
	start := len(msgs) - keepTail
	for i := start; i < len(msgs); i++ {
		if msgs[i].Role == schema.User {
			// 继续向前看是否还有更早的 user 消息在窗口内，
			// 取最早的一个作为安全截断点。
			start = i
			break
		}
	}
	// 窗口内没有 user 消息边界可对齐（如连续工具调用），再多丢一些，
	// 丢到紧邻位置的前一个 user 消息处；找不到就整段丢弃头部保留区之外的内容。
	if msgs[start].Role != schema.User {
		for i := start; i >= 0; i-- {
			if msgs[i].Role == schema.User {
				start = i
				break
			}
		}
	}
	if msgs[start].Role != schema.User {
		return nil, fmt.Errorf("agent: 历史压缩失败：%d 条消息中找不到 user 边界", len(msgs))
	}

	out := make([]*schema.Message, 0, c.KeepHead+len(msgs)-start)
	out = append(out, msgs[:c.KeepHead]...)
	// 插入一条说明性 user 消息，告知模型早期上下文已被截断，
	// 同时充当尾段消息 tool/user 约束的缓冲边界。
	notice := schema.UserMessage(fmt.Sprintf(
		"[系统提示] 早期 %d 条历史消息已因超出上下文限制被截断。", start-c.KeepHead))
	out = append(out, notice)
	out = append(out, msgs[start:]...)
	return out, nil
}
