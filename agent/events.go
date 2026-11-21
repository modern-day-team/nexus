package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/schema"
)

// 本文件聚集流式事件：事件类型、emitter 管道与 ChatStreamEvents 主循环。
// 工具的事件包装见 observe.go。

// EventType 标识流式事件类型。
type EventType string

const (
	// EventToken 是模型输出的正文分片。
	EventToken EventType = "token"
	// EventReasoning 是模型输出的思考过程分片（部分模型支持）。
	EventReasoning EventType = "reasoning"
	// EventToolStart 表示一次工具调用开始。
	EventToolStart EventType = "tool_start"
	// EventToolEnd 表示一次工具调用结束。
	EventToolEnd EventType = "tool_end"
	// EventApprovalRequest 表示一次待审批的工具调用，需要外部通过
	// Respond 提交决定后本轮才会继续。仅在使用交互式审批（见
	// interactive.go）时产生。
	EventApprovalRequest EventType = "approval_request"
	// EventError 表示流式过程中出现错误。EventDone 会随后到达并携带同一错误。
	EventError EventType = "error"
	// EventDone 表示本轮结束，且会话历史已写入。它总是最后一个事件。
	// 若本轮出错，Err 非空；Usage 在模型返回用量时非空。
	EventDone EventType = "done"
)

// Usage 是本轮的 token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 是本轮 prompt 中命中上下文缓存的 token 数。
	// DeepSeek 等提供前缀缓存的模型会返回该值，可用于监控缓存命中率：
	// CachedTokens / PromptTokens 越接近 1 越省时延与成本。
	CachedTokens int
}

// Event 是 ChatStreamEvents 输出的流式事件。
type Event struct {
	Type EventType
	// Content 为 token 或 reasoning 分片的内容。
	Content string
	// ToolName 为工具名，仅工具事件有效。
	ToolName string
	// Arguments 为工具调用的 JSON 参数，仅工具事件有效。
	Arguments string
	// Result 为工具返回结果，仅 EventToolEnd 有效。
	Result string
	// Usage 为本轮 token 用量，仅 EventDone 可能非空。
	Usage *Usage
	// Err 为工具执行或流式过程中的错误。
	Err error
	// Agent 是产生该事件的 Agent 名。空串表示本体 Agent；
	// 子 Agent 经委派透传的事件会带上委派工具名。
	Agent string
	// Request 为审批请求，仅 EventApprovalRequest 有效。
	Request *ApprovalRequest
	// Respond 提交审批决定，仅 EventApprovalRequest 有效；只能调用一次，
	// 二次调用为 no-op。必须在审批超时前调用。
	Respond func(ApprovalDecision)
}

type eventEmitter func(Event)

type emitterCtxKey struct{}

func withEmitter(ctx context.Context, emit eventEmitter) context.Context {
	return context.WithValue(ctx, emitterCtxKey{}, emit)
}

func emitEvent(ctx context.Context, ev Event) {
	if emit, ok := ctx.Value(emitterCtxKey{}).(eventEmitter); ok && emit != nil {
		emit(ev)
	}
}

// eventSink 在 runDone 关闭后丢弃事件，保证工具 goroutine 不会因通道关闭而 panic。
type eventSink struct {
	ch   chan<- Event
	done <-chan struct{}
}

func (s eventSink) emit(ev Event) {
	select {
	case s.ch <- ev:
	case <-s.done:
	}
}

// ChatStreamEvents 发送一条用户消息并以事件流方式返回模型输出与工具调用过程。
//
// 事件契约：EventDone 是最后一个事件，到达时本轮会话历史已写入；
// 若本轮出错，EventError 先于 EventDone 到达且两者 Err 相同。
// 调用方必须消费完或 Close 返回的流，否则该轮次不会结束。
//
// 函数立即返回事件流；模型调用与工具执行在后台 goroutine 中驱动。
// 这保证工具执行期间产生的事件（如审批请求，见 interactive.go）能被
// 调用方实时消费并响应，而不是等到本轮结束才批量送达。
// inner.Stream 阶段的错误也经 EventError/EventDone 送达，而非同步返回。
func (a *Agent) ChatStreamEvents(
	ctx context.Context, input string, opts ...einoagent.AgentOption,
) (*schema.StreamReader[Event], error) {
	if err := a.turn.begin(ctx); err != nil {
		return nil, err
	}

	// 工具事件经 ctx 传递 hook；轮次事件经 Agent 字段直接回调。
	ctx = withTrace(ctx, a.traceHook)
	a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnStart, Input: input})

	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		a.turn.end()
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err})
		return nil, err
	}

	events := make(chan Event, 64)
	runDone := make(chan struct{})
	sink := eventSink{ch: events, done: runDone}
	runCtx := withEmitter(withTrace(ctx, a.traceHook), sink.emit)

	out, writer := schema.Pipe[Event](64)

	// 单一写入者：把内部事件通道转发到 Eino 流，保证 Send 串行。
	go drainEvents(writer, events, runDone)

	// 失败收尾：发 EventError 与 EventDone，保持"Done 总是最后一个事件"的契约。
	fail := func(err error) {
		sink.emit(Event{Type: EventError, Err: err})
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Err: err})
		sink.emit(Event{Type: EventDone, Err: err})
	}

	go func() {
		defer a.turn.end()
		defer close(runDone)

		// inner.Stream 在后台调用：eino ReAct 的 Stream 会同步跑完
		// 首轮模型推理与工具调用才返回首个分片，若在此同步等待，
		// 工具执行期间的事件（如审批请求）无法被调用方实时消费。
		modelStream, err := a.inner.Stream(runCtx, msgs, opts...)
		if err != nil {
			fail(err)
			return
		}
		defer modelStream.Close()

		if err := a.session.Append(runCtx, schema.UserMessage(input)); err != nil {
			fail(err)
			return
		}

		var content, reasoning strings.Builder
		var usage *Usage
		var streamErr error

		for {
			chunk, recvErr := modelStream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				streamErr = recvErr
				sink.emit(Event{Type: EventError, Err: recvErr})
				break
			}
			if chunk == nil {
				continue
			}

			if meta := chunk.ResponseMeta; meta != nil && meta.Usage != nil {
				usage = usageFromMeta(meta)
			}
			if chunk.ReasoningContent != "" {
				reasoning.WriteString(chunk.ReasoningContent)
				sink.emit(Event{Type: EventReasoning, Content: chunk.ReasoningContent})
			}
			if chunk.Content != "" {
				content.WriteString(chunk.Content)
				sink.emit(Event{Type: EventToken, Content: chunk.Content})
			}
		}

		// 先写入历史再发 Done，保证调用方收到 Done 后可安全发起下一轮。
		appendErr := a.session.Append(context.WithoutCancel(ctx), &schema.Message{
			Role:             schema.Assistant,
			Content:          content.String(),
			ReasoningContent: reasoning.String(),
		})
		if appendErr != nil && streamErr == nil {
			streamErr = appendErr
		}

		// 在发 Done 之前回调用量钩子，保证钩子先于调用方收到本轮结束信号。
		a.notifyUsage(ctx, usage)
		a.notifyTrace(ctx, TraceEvent{Kind: TraceTurnEnd, Usage: usage, Err: streamErr})

		sink.emit(Event{Type: EventDone, Usage: usage, Err: streamErr})
	}()

	return out, nil
}

// drainEvents 转发事件直到本轮结束，并丢弃消费方关闭后的事件。
func drainEvents(writer *schema.StreamWriter[Event], events <-chan Event, runDone <-chan struct{}) {
	defer writer.Close()

	var closed bool
	forward := func(ev Event) {
		if closed {
			return
		}
		closed = writer.Send(ev, nil)
	}

	for {
		select {
		case ev := <-events:
			forward(ev)
		case <-runDone:
			for {
				select {
				case ev := <-events:
					forward(ev)
				default:
					return
				}
			}
		}
	}
}
