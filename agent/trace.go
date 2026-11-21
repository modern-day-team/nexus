package agent

import (
	"context"
)

// 本文件实现可观测 trace 钩子：在每轮开始/结束与每次工具调用结束时
// 同步回调 TraceHook，让调用方把内核事件映射为外部追踪系统（如 OTel）的 span。
// 事件类型与回调时机见 TraceKind。

// TraceKind 标识 trace 事件类型。
type TraceKind string

const (
	// TraceTurnStart 是一轮对话开始（含 Input）。
	TraceTurnStart TraceKind = "turn_start"
	// TraceTurnEnd 是一轮对话结束（含 Usage、Err）。
	TraceTurnEnd TraceKind = "turn_end"
	// TraceToolCall 是一次工具调用结束（含 ToolName/Arguments/Result/Err）。
	TraceToolCall TraceKind = "tool_call"
)

// TraceEvent 是一次 trace 事件。
type TraceEvent struct {
	Kind TraceKind
	// Input 为本轮用户输入，仅 TraceTurnStart 有效。
	Input string
	// ToolName/Arguments/Result 仅 TraceToolCall 有效。
	ToolName  string
	Arguments string
	Result    string
	// Usage 仅 TraceTurnEnd 有效（模型未返回用量时为 nil）。
	Usage *Usage
	// Err 为本轮或工具的错误。
	Err error
	// Model 为本轮使用的模型名，取自 Options.ModelName，仅轮次事件
	// （TraceTurnStart/TraceTurnEnd）携带，未配置时为空串。
	Model string
}

// ContextCustomizer 在每轮 TraceTurnStart 事件发出后被调用，返回的 ctx
// 作为本轮后续所有操作的执行 ctx（包括 inner.Generate/Stream 与工具执行）。
//
// 约定：
//   - 实现不得阻塞（同步调用，在轮次关键路径上）；
//   - 返回的 ctx 会贯穿整轮，包括工具执行，因此可用来注入
//     OTel span 等 per-turn 资源（见 otelx.New 的 Hooks.ContextCustomizer）；
//   - 收到的 ctx 是请求 ctx（尚未被本轮 customizer 修改过），
//     ev 是刚发出的 TraceTurnStart 事件；
//   - 为 nil 时零开销跳过。
type ContextCustomizer func(ctx context.Context, ev TraceEvent) context.Context

// TraceHook 在每轮开始/结束与每次工具调用结束时同步调用，实现应轻量。
// hook 为 nil 时零开销跳过。
type TraceHook func(ctx context.Context, ev TraceEvent)

// traceCtxKey 是挂载 TraceHook 的 context key。
// 轮次事件经 Agent 字段直接回调；工具事件发生在内核深处，
// 只能通过 ctx 把 hook 传入工具包装器（与 emitterCtxKey 同一模式）。
type traceCtxKey struct{}

// withTrace 把 hook 挂到 ctx 上，供工具包装器经 emitTrace 读取。
func withTrace(ctx context.Context, hook TraceHook) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey{}, hook)
}

// emitTrace 从 ctx 读取 hook 并回调工具调用事件；无 hook 时零开销跳过。
func emitTrace(ctx context.Context, ev TraceEvent) {
	if hook, ok := ctx.Value(traceCtxKey{}).(TraceHook); ok && hook != nil {
		hook(ctx, ev)
	}
}

// notifyTrace 在有钩子时同步回调轮次级 trace 事件。
func (a *Agent) notifyTrace(ctx context.Context, ev TraceEvent) {
	if a.traceHook == nil {
		return
	}
	a.traceHook(ctx, ev)
}

// startTurnTrace 统一三条路径（Chat/ChatStream/ChatStreamEvents）的轮次起点：
// 先用当前请求 ctx 发出 TraceTurnStart（事件本身不依赖 ctx 注入，先发可以
// 让 otelx 等 hook 先启动 run span 并登记，随后的 customizer 才能取到它），
// 再调用 ContextCustomizer（TurnStart 先发、customizer 后调用），返回的 ctx
// 作为本轮后续所有操作（inner.Generate/Stream、工具执行）的执行 ctx。
func (a *Agent) startTurnTrace(ctx context.Context, input string) context.Context {
	ev := TraceEvent{Kind: TraceTurnStart, Input: input, Model: a.modelName}
	a.notifyTrace(ctx, ev)
	if a.ctxCustomizer != nil {
		ctx = a.ctxCustomizer(ctx, ev)
	}
	return ctx
}
