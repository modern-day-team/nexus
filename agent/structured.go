package agent

import (
	"context"
	"encoding/json"
	"fmt"

	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/schema"
)

// 本文件实现结构化输出：GenerateInto 要求模型返回 JSON 并解析到调用方结构，
// 解析失败时携带错误信息在同一轮次内重试。

// defaultGenerateAttempts 是 GenerateInto 的默认重试轮数（含首次）。
const defaultGenerateAttempts = 3

// GenerateInto 发送一条用户消息，要求模型返回 JSON 并解析到 out（指针）。
//
// 解析失败时，把错误信息作为新的 user 消息反馈给模型重试，最多 3 轮（含首次）。
// 所有轮次失败返回最后一次解析错误。
//
// 与 Chat 的差异：
//   - 请求消息在本方法内组装：系统提示词 + 会话历史 + user(input + JSON 指令)，
//     不改写 requestMessages 与会话历史的只追加约定；
//   - 重试轮次发生在同一 turn 内（beginTurn/endTurn 串行化），中间轮的
//     反馈消息与失败输出不写入会话历史，只有最终成功的一轮 user+assistant 落库；
//   - UsageHook/TraceHook 在成功轮（或轮次级错误）触发，语义与 Chat 对齐。
//
// 调用走 a.inner.Generate 的完整路径（复用工具调用、历史与压缩）。
func (a *Agent) GenerateInto(ctx context.Context, input string, out any, opts ...einoagent.AgentOption) error {
	if out == nil {
		return fmt.Errorf("agent: out 不能为空")
	}

	if err := a.turn.begin(ctx); err != nil {
		return err
	}
	defer a.turn.end()

	// 工具事件经 ctx 传递 hook；轮次事件经 Agent 字段直接回调。
	ctx = withTrace(ctx, a.traceHook)
	a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnStart, Input: input})

	history, err := a.session.Messages(ctx)
	if err != nil {
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err})
		return err
	}
	if a.compactor != nil {
		if history, err = a.compactor.Compact(ctx, history); err != nil {
			a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err})
			return err
		}
	}

	// 组装消息：系统提示词 → 会话历史 → user(输入 + JSON 指令)。
	// 反馈消息只追加到本地切片，不落库。
	msgs := make([]*schema.Message, 0, len(history)+2)
	if a.systemPrompt != "" {
		msgs = append(msgs, schema.SystemMessage(a.systemPrompt))
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, schema.UserMessage(input+"\n\n只以 JSON 形式回答，不要包含任何其他文本或 markdown 代码块。"))

	var lastErr error
	for range defaultGenerateAttempts {
		reply, genErr := a.inner.Generate(ctx, msgs, opts...)
		if genErr != nil {
			a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: genErr})
			return genErr
		}

		raw, extErr := ExtractJSON(reply.Content)
		if extErr == nil {
			if unmarshalErr := json.Unmarshal([]byte(raw), out); unmarshalErr == nil {
				// 成功：仅落最终一轮 user+assistant，与 Chat 一致。
				if appendErr := a.session.Append(ctx, schema.UserMessage(input), finalAssistantMessage(reply)); appendErr != nil {
					return fmt.Errorf("agent: 写入会话历史失败: %w", appendErr)
				}
				usage := usageFromMeta(reply.ResponseMeta)
				a.notifyUsage(ctx, usage)
				a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Usage: usage})
				return nil
			} else {
				lastErr = unmarshalErr
			}
		} else {
			lastErr = extErr
		}

		// 反馈消息追加到请求尾部，不写入会话历史。
		msgs = append(msgs,
			schema.AssistantMessage(reply.Content, nil),
			schema.UserMessage(fmt.Sprintf("上一次输出不是合法 JSON/未通过校验：%v。请只输出修正后的 JSON。", lastErr)),
		)
	}

	err = fmt.Errorf("agent: 结构化输出在 %d 轮内未得到合法 JSON: %w", defaultGenerateAttempts, lastErr)
	a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err})
	return err
}
