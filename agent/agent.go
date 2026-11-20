// Package agent 实现基于 Eino ReAct 的 Agent 内核。
//
// 用法：
//
//	a, err := agent.New(ctx, agent.Options{ChatModel: cm, Tools: tools})
//	reply, err := a.Chat(ctx, "你好")
//	stream, err := a.ChatStream(ctx, "帮我查一下现在的北京时间")
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// DefaultMaxStep 是 ReAct 循环的默认最大步数。
const DefaultMaxStep = 12

// Options 用于构建 Agent。
type Options struct {
	// ChatModel 必须实现 tool calling 能力。
	ChatModel model.ToolCallingChatModel
	// Tools 是可被模型调用的工具集合，可为空。
	Tools []tool.BaseTool
	// SystemPrompt 为系统提示词，可为空。系统提示词不写入会话历史。
	SystemPrompt string
	// MaxStep 为 ReAct 最大步数，<=0 时使用 DefaultMaxStep。
	MaxStep int
	// Session 为会话存储，可为空，默认使用 MemorySession。
	Session Session
}

// Agent 是一个基于 ReAct 的 Agent 内核。
// 同一实例内的会话消息由 Session 保存，Chat 会串行执行以保证多轮顺序。
type Agent struct {
	inner        *react.Agent
	systemPrompt string
	session      Session

	mu sync.Mutex
}

// New 构建 Agent。
func New(ctx context.Context, opts Options) (*Agent, error) {
	if opts.ChatModel == nil {
		return nil, errors.New("agent: ChatModel 不能为空")
	}
	maxStep := opts.MaxStep
	if maxStep <= 0 {
		maxStep = DefaultMaxStep
	}

	inner, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: opts.ChatModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: observeTools(opts.Tools),
		},
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, err
	}

	session := opts.Session
	if session == nil {
		session = NewMemorySession()
	}

	return &Agent{
		inner:        inner,
		systemPrompt: opts.SystemPrompt,
		session:      session,
	}, nil
}

// Chat 发送一条用户消息并等待完整回复，同时把本轮对话写入会话。
func (a *Agent) Chat(ctx context.Context, input string, opts ...einoagent.AgentOption) (*schema.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		return nil, err
	}

	reply, err := a.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}

	if err := a.session.Append(ctx, schema.UserMessage(input), finalAssistantMessage(reply)); err != nil {
		return reply, fmt.Errorf("agent: 写入会话历史失败: %w", err)
	}
	return reply, nil
}

// ChatStream 发送一条用户消息并以流式方式返回回复分片。
// 调用方必须在使用完后 Close 返回的流。会话历史会在流被完整消费后异步写入。
func (a *Agent) ChatStream(ctx context.Context, input string, opts ...einoagent.AgentOption) (*schema.StreamReader[*schema.Message], error) {
	a.mu.Lock()
	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	stream, err := a.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if err := a.session.Append(ctx, schema.UserMessage(input)); err != nil {
		a.mu.Unlock()
		stream.Close()
		return nil, err
	}
	a.mu.Unlock()

	copies := stream.Copy(2)
	out, internal := copies[0], copies[1]

	go func() {
		defer internal.Close()
		msg, err := schema.ConcatMessageStream(internal)
		if err != nil {
			return
		}
		_ = a.session.Append(ctx, finalAssistantMessage(msg))
	}()

	return out, nil
}

// History 返回当前会话的历史消息副本（不含系统提示词）。
func (a *Agent) History(ctx context.Context) ([]*schema.Message, error) {
	return a.session.Messages(ctx)
}

// Reset 清空当前会话。
func (a *Agent) Reset(ctx context.Context) error {
	return a.session.Clear(ctx)
}

// requestMessages 组合系统提示词、会话历史与本次用户输入。
func (a *Agent) requestMessages(ctx context.Context, input string) ([]*schema.Message, error) {
	history, err := a.session.Messages(ctx)
	if err != nil {
		return nil, err
	}

	msgs := make([]*schema.Message, 0, len(history)+2)
	if a.systemPrompt != "" {
		msgs = append(msgs, schema.SystemMessage(a.systemPrompt))
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, schema.UserMessage(input))
	return msgs, nil
}

// finalAssistantMessage 只保留最终文本与推理内容，剥离工具调用，
// 避免历史中出现缺少 tool 响应的 assistant 消息。
func finalAssistantMessage(msg *schema.Message) *schema.Message {
	if msg == nil {
		return schema.AssistantMessage("", nil)
	}
	return &schema.Message{
		Role:             schema.Assistant,
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
	}
}
