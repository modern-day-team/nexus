// Package agent 实现基于 Eino ReAct 的 Agent 内核。
//
// 文件职责划分（低耦合高内聚）：
//
//	agent.go       内核：构建、轮次编排、请求消息组装
//	turn.go        轮次串行化信号量
//	session.go     Session 接口与内存实现
//	filesession.go 文件持久化 Session 实现
//	observe.go     工具包装器：审批与事件观测
//	events.go      流式事件类型与 ChatStreamEvents
//	retry.go       模型调用重试
//	compact.go     请求前历史压缩
//	usagehook.go   每轮用量回调钩子
//	trace.go       可观测 trace 钩子
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
	"sync/atomic"
	"time"

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
	// Compactor 在每次请求前压缩会话历史，可为空（不压缩）。
	// 默认实现见 TruncateCompactor。
	Compactor Compactor
	// Retry 为模型调用重试策略，可为空（不重试）。
	Retry *RetryOptions
	// ToolApprover 在每次工具调用前被询问，可为空（全部放行）。
	ToolApprover ToolApprover
	// UsageHook 在每轮对话结束后被回调，携带本轮 token 用量，可为空（不回调）。
	// 见 UsageHook。
	UsageHook UsageHook
	// TraceHook 在每轮开始/结束与每次工具调用结束时被回调，可为空（不回调）。
	// 见 TraceHook。
	TraceHook TraceHook
	// ModelName 是本轮使用的模型名，会随 TurnStart/TurnEnd trace 事件
	// （TraceEvent.Model）透出，供下游映射为 gen_ai.request.model 等属性。
	// 可为空。
	ModelName string
	// ContextCustomizer 在每轮 TraceTurnStart 发出后被调用（可为空，零开销跳过），
	// 返回的 ctx 作为本轮后续所有操作的执行 ctx，会贯穿整轮，包括工具执行。
	// 实现不得阻塞；典型用途是把 TurnStart 创建的 trace span 注入请求 ctx，
	// 使工具 span 挂到 run span 之下（见 otelx.New）。
	// 见 ContextCustomizer。
	ContextCustomizer ContextCustomizer
	// ToolTimeout 为单次工具调用的超时时长，<=0 表示不限时。
	// 超时时工具结果为一条错误文本反馈给模型，不会中断本轮；
	// panic 同样被恢复为错误结果（见 observe.go robustTool）。
	ToolTimeout time.Duration
}

// Agent 是一个基于 ReAct 的 Agent 内核。
// 同一实例上的轮次（Chat/流式对话/Reset）串行执行；流式调用必须消费完或关闭返回的流，
// 否则该轮次不会结束，后续轮次会被阻塞。
type Agent struct {
	inner        *react.Agent
	systemPrompt string
	// systemMessage 是构建时一次性创建的系统提示词消息（systemPrompt 非空时）。
	// 消息内容不可变，requestMessages 每轮复用同一指针，
	// 避免重复分配；调用方不应修改该消息。
	systemMessage *schema.Message
	session       Session
	compactor     Compactor
	usageHook     UsageHook
	traceHook     TraceHook
	modelName     string
	ctxCustomizer ContextCustomizer

	// turn 串行化轮次。
	turn turnGate

	// droppedEvents 累计所有轮次因消费方关闭/本轮结束而丢弃的事件总数。
	// 每轮新建的 eventSink 通过指针累加到该字段（见 events.go）。
	droppedEvents atomic.Int64
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
		ToolCallingModel: newRetryingModel(opts.ChatModel, opts.Retry),
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: setupTools(opts.Tools, opts.ToolApprover, opts.ToolTimeout),
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

	a := &Agent{
		inner:         inner,
		systemPrompt:  opts.SystemPrompt,
		session:       session,
		compactor:     opts.Compactor,
		usageHook:     opts.UsageHook,
		traceHook:     opts.TraceHook,
		modelName:     opts.ModelName,
		ctxCustomizer: opts.ContextCustomizer,
		turn:          newTurnGate(),
	}
	if opts.SystemPrompt != "" {
		// 系统提示词在构建期固定，消息不可变，跨轮次复用同一指针安全。
		a.systemMessage = schema.SystemMessage(opts.SystemPrompt)
	}
	return a, nil
}

// NewWithStore 在 Options 基础上从 store 取 conversationID 对应的 Session 注入。
// opts.Session 会被覆盖。返回的 *Agent 使用完毕后由调用方管理 store 生命周期。
func NewWithStore(ctx context.Context, store SessionStore, conversationID string, opts Options) (*Agent, error) {
	if store == nil {
		return nil, errors.New("agent: SessionStore 不能为空")
	}
	session, err := store.Session(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	opts.Session = session
	return New(ctx, opts)
}

// Chat 发送一条用户消息并等待完整回复，同时把本轮对话写入会话。
func (a *Agent) Chat(ctx context.Context, input string, opts ...einoagent.AgentOption) (*schema.Message, error) {
	if err := a.turn.begin(ctx); err != nil {
		return nil, err
	}
	defer a.turn.end()

	// 工具事件经 ctx 传递 hook；轮次事件经 Agent 字段直接回调。
	// TurnStart 先发、customizer 后调用，customizer 返回的 ctx 贯穿整轮（含工具执行）。
	ctx = withTrace(ctx, a.traceHook)
	ctx = a.startTurnTrace(ctx, input)
	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
		return nil, err
	}

	reply, err := a.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
		return nil, err
	}

	if err := a.session.Append(ctx, schema.UserMessage(input), finalAssistantMessage(reply)); err != nil {
		return reply, fmt.Errorf("agent: 写入会话历史失败: %w", err)
	}
	usage := usageFromMeta(reply.ResponseMeta)
	a.notifyUsage(ctx, usage)
	a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Usage: usage, Model: a.modelName})
	return reply, nil
}

// ChatStream 发送一条用户消息并以流式方式返回回复分片。
// 调用方必须在使用完后 Close 返回的流；本轮历史在流结束后写入。
func (a *Agent) ChatStream(
	ctx context.Context, input string, opts ...einoagent.AgentOption,
) (*schema.StreamReader[*schema.Message], error) {
	if err := a.turn.begin(ctx); err != nil {
		return nil, err
	}

	// 工具事件经 ctx 传递 hook；轮次事件经 Agent 字段直接回调。
	// TurnStart 先发、customizer 后调用，customizer 返回的 ctx 贯穿整轮（含工具执行）。
	ctx = withTrace(ctx, a.traceHook)
	ctx = a.startTurnTrace(ctx, input)

	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		a.turn.end()
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
		return nil, err
	}

	stream, err := a.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		a.turn.end()
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
		return nil, err
	}
	if err := a.session.Append(ctx, schema.UserMessage(input)); err != nil {
		stream.Close()
		a.turn.end()
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
		return nil, err
	}

	copies := stream.Copy(2)
	out, internal := copies[0], copies[1]

	go func() {
		defer a.turn.end()
		defer internal.Close()

		msg, err := schema.ConcatMessageStream(internal)
		if err != nil {
			a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err, Model: a.modelName})
			return
		}
		// 历史写入不随请求 ctx 取消，避免调用方取消后历史永久缺失。
		_ = a.session.Append(context.WithoutCancel(ctx), finalAssistantMessage(msg))
		usage := usageFromMeta(msg.ResponseMeta)
		a.notifyUsage(ctx, usage)
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Usage: usage, Model: a.modelName})
	}()

	return out, nil
}

// History 返回当前会话的历史消息副本（不含系统提示词）。
func (a *Agent) History(ctx context.Context) ([]*schema.Message, error) {
	return a.session.Messages(ctx)
}

// DroppedEvents 返回自 Agent 创建以来因消费方关闭/本轮结束而丢弃的
// 流式事件总数。调用方提前 Close ChatStreamEvents 返回的流后，本轮
// 后续事件会被丢弃并计入该值（见 events.go eventSink）。
func (a *Agent) DroppedEvents() int64 {
	return a.droppedEvents.Load()
}

// Reset 清空当前会话。若存在进行中的轮次，Reset 会等待其结束后再清空。
func (a *Agent) Reset(ctx context.Context) error {
	if err := a.turn.begin(ctx); err != nil {
		return err
	}
	defer a.turn.end()

	return a.session.Clear(ctx)
}

// requestMessages 组合系统提示词、会话历史与本次用户输入。
//
// 消息顺序为 系统提示词 → 会话历史 → 本次输入，且历史只追加、不改写
// （见 finalAssistantMessage），保证多轮间请求前缀字节级稳定，
// 以命中模型服务商的 prompt cache，降低首 token 延迟与计费 token 数。
// 调用方不应在 Agent 之外改写会话历史中的消息内容。
func (a *Agent) requestMessages(ctx context.Context, input string) ([]*schema.Message, error) {
	history, err := a.session.Messages(ctx)
	if err != nil {
		return nil, err
	}
	// 请求时压缩，不回写会话：历史本身保持完整，压缩只影响本次请求。
	if a.compactor != nil {
		if history, err = a.compactor.Compact(ctx, history); err != nil {
			return nil, err
		}
	}

	msgs := make([]*schema.Message, 0, len(history)+2)
	if a.systemMessage != nil {
		msgs = append(msgs, a.systemMessage)
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
