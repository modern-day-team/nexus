package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/schema"
)

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
	// EventError 表示流式过程中出现错误，流随后结束。
	EventError EventType = "error"
)

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
	// Err 为工具执行或流式过程中的错误。
	Err error
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

// observedInvokableTool 在工具执行前后发出事件。未提供 emitter 时行为与原始工具一致。
type observedInvokableTool struct {
	tool.InvokableTool
}

func (t *observedInvokableTool) InvokableRun(
	ctx context.Context, argumentsInJSON string, opts ...tool.Option,
) (string, error) {
	name := ""
	if info, err := t.InvokableTool.Info(ctx); err == nil && info != nil {
		name = info.Name
	}

	emitEvent(ctx, Event{Type: EventToolStart, ToolName: name, Arguments: argumentsInJSON})

	out, err := t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)

	emitEvent(ctx, Event{
		Type:      EventToolEnd,
		ToolName:  name,
		Arguments: argumentsInJSON,
		Result:    out,
		Err:       err,
	})
	return out, err
}

// observeTools 为实现了 InvokableTool 的工具加上事件包装，其余工具保持不变。
func observeTools(tools []tool.BaseTool) []tool.BaseTool {
	if len(tools) == 0 {
		return tools
	}
	out := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		if invokable, ok := t.(tool.InvokableTool); ok {
			out[i] = &observedInvokableTool{InvokableTool: invokable}
			continue
		}
		out[i] = t
	}
	return out
}

// ChatStreamEvents 发送一条用户消息并以事件流方式返回模型输出与工具调用过程。
// 调用方必须在使用完后 Close 返回的流；历史会在流结束后写入。
func (a *Agent) ChatStreamEvents(
	ctx context.Context, input string, opts ...einoagent.AgentOption,
) (*schema.StreamReader[Event], error) {
	a.mu.Lock()
	msgs, err := a.requestMessages(ctx, input)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}

	events := make(chan Event, 64)
	ctx = withEmitter(ctx, func(ev Event) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	})

	modelStream, err := a.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if err := a.session.Append(ctx, schema.UserMessage(input)); err != nil {
		a.mu.Unlock()
		modelStream.Close()
		return nil, err
	}
	a.mu.Unlock()

	out, writer := schema.Pipe[Event](64)

	// 单一写入者：把内部事件通道转发到 Eino 流，保证 Send 串行。
	go func() {
		var closed bool
		for ev := range events {
			if closed {
				continue
			}
			closed = writer.Send(ev, nil)
		}
		writer.Close()
	}()

	go func() {
		defer modelStream.Close()

		var content, reasoning strings.Builder
		for {
			chunk, recvErr := modelStream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				events <- Event{Type: EventError, Err: recvErr}
				break
			}
			if chunk == nil {
				continue
			}
			if chunk.ReasoningContent != "" {
				reasoning.WriteString(chunk.ReasoningContent)
				events <- Event{Type: EventReasoning, Content: chunk.ReasoningContent}
			}
			if chunk.Content != "" {
				content.WriteString(chunk.Content)
				events <- Event{Type: EventToken, Content: chunk.Content}
			}
		}
		close(events)

		_ = a.session.Append(ctx, &schema.Message{
			Role:             schema.Assistant,
			Content:          content.String(),
			ReasoningContent: reasoning.String(),
		})
	}()

	return out, nil
}
