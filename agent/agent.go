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
	// SystemPrompt 为系统提示词，可为空。
	SystemPrompt string
	// MaxStep 为 ReAct 最大步数，<=0 时使用 DefaultMaxStep。
	MaxStep int
}

// Agent 是一个带多轮会话历史的 ReAct Agent 内核。
// 同一个 Agent 实例内的会话历史按调用顺序累积，非并发安全的多会话复用。
type Agent struct {
	inner        *react.Agent
	systemPrompt string

	mu      sync.Mutex
	history []*schema.Message
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

	a := &Agent{inner: inner, systemPrompt: opts.SystemPrompt}
	if opts.SystemPrompt != "" {
		a.history = append(a.history, schema.SystemMessage(opts.SystemPrompt))
	}
	return a, nil
}

// Chat 发送一条用户消息并等待完整回复，同时把本轮对话写入历史。
func (a *Agent) Chat(ctx context.Context, input string, opts ...einoagent.AgentOption) (*schema.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	msgs := append(cloneMessages(a.history), schema.UserMessage(input))
	reply, err := a.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}

	a.history = append(a.history, schema.UserMessage(input), finalAssistantMessage(reply))
	return reply, nil
}

// ChatStream 发送一条用户消息并以流式方式返回回复分片。
// 调用方必须在使用完后 Close 返回的流。会话历史会在流被完整消费后异步写入。
func (a *Agent) ChatStream(ctx context.Context, input string, opts ...einoagent.AgentOption) (*schema.StreamReader[*schema.Message], error) {
	a.mu.Lock()
	msgs := append(cloneMessages(a.history), schema.UserMessage(input))
	a.mu.Unlock()

	stream, err := a.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}

	copies := stream.Copy(2)
	out, internal := copies[0], copies[1]

	a.mu.Lock()
	a.history = append(a.history, schema.UserMessage(input))
	a.mu.Unlock()

	go func() {
		defer internal.Close()
		msg, err := schema.ConcatMessageStream(internal)
		if err != nil {
			return
		}
		a.mu.Lock()
		a.history = append(a.history, finalAssistantMessage(msg))
		a.mu.Unlock()
	}()

	return out, nil
}

// History 返回当前会话历史的副本。
func (a *Agent) History() []*schema.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneMessages(a.history)
}

// Reset 清空会话历史，保留系统提示词。
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	system := make([]*schema.Message, 0, 1)
	if a.systemPrompt != "" {
		system = append(system, schema.SystemMessage(a.systemPrompt))
	}
	a.history = system
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

func cloneMessages(msgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	return out
}
