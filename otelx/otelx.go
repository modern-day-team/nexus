// Package otelx 把 agent 内核的 TraceEvent 映射为遵循
// OpenTelemetry GenAI 语义约定的 span。
//
// 用法（推荐，run span 与工具 span 形成父子嵌套）：
//
//	tracer := otel.Tracer("nexus")
//	hooks := otelx.New(ctx, tracer)
//	a, _ := agent.New(ctx, agent.Options{
//	    ChatModel:         cm,
//	    TraceHook:         hooks.TraceHook,
//	    ContextCustomizer: hooks.ContextCustomizer,
//	})
//
// 兼容旧用法（无嵌套，工具 span 独立）：
//
//	a, _ := agent.New(ctx, agent.Options{
//	    ChatModel: cm,
//	    TraceHook: otelx.NewTraceHook(tracer),
//	})
package otelx

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/modern-day-team/nexus/agent"
)

// 默认属性值与上限。
const (
	defaultSystemName = "nexus"
	defaultAgentName  = "agent"
	// maxContentAttrLen 限制内容类属性长度，避免大结果撑爆 span。
	maxContentAttrLen = 4096
)

// Options 配置 New/NewTraceHook 的映射行为。
type Options struct {
	// SystemName 是 gen_ai.system 属性值，默认 "nexus"。
	SystemName string
	// AgentName 出现在 agent.run 的 span 名中（"gen_ai.<AgentName>.agent.run"），
	// 默认 "agent"。
	AgentName string
}

// Option 修改 Options。
type Option func(*Options)

// WithSystemName 覆盖 gen_ai.system 属性值。
func WithSystemName(name string) Option {
	return func(o *Options) { o.SystemName = name }
}

// WithAgentName 设置 span 名中的 Agent 名。
func WithAgentName(name string) Option {
	return func(o *Options) { o.AgentName = name }
}

// runSpanState 是一轮对话的进行中 span，挂在 hook 闭包内的注册表上，
// 以"原始请求 ctx"为键串联 TraceTurnStart 与 TraceTurnEnd 两次回调。
type runSpanState struct {
	span trace.Span
}

// originalCtxKey 用于让经 customizer 注入 span 后的派生 ctx 仍能找回
// 原始请求 ctx（注册表的键），保证 TurnEnd 配对可靠。
type originalCtxKey struct{}

// Hooks 是 otelx 提供给 agent 内核的双注册钩子集合：TraceHook 回调
// trace 事件，ContextCustomizer 把 TurnStart 启动的 run span 注入请求
// ctx，使整轮（含工具执行）都落在 run span 之下。两者必须配套使用
// （分别传给 agent.Options.TraceHook 与 agent.Options.ContextCustomizer）。
type Hooks struct {
	// TraceHook 传给 agent.Options.TraceHook。
	TraceHook agent.TraceHook
	// ContextCustomizer 传给 agent.Options.ContextCustomizer。可为 nil
	// （见 NewTraceHook），此时维持旧行为：工具 span 与 run span 无父子关系。
	ContextCustomizer agent.ContextCustomizer
}

// New 返回带 span 父子嵌套能力的双注册钩子。
//
// ctx 流转：内核在每轮先以原始请求 ctx C0 发出 TraceTurnStart（hook 据此
// 启动 run span 并以 C0 为键登记），随后以同一 C0 调用 ContextCustomizer；
// customizer 按 C0 取出该轮 span，返回注入 span 的派生 ctx C1（并携带 C0
// 本身），内核以 C1 驱动本轮 inner.Generate/Stream 与工具执行——工具 span
// 因此成为 run span 的子 span。TurnEnd 时内核以 C1 回调 hook，hook 经
// originalCtxKey 找回 C0 完成配对并结束 run span。ChatStream 的后台
// goroutine 经 context.WithoutCancel 包装 C1，值链保留，配对同样成立。
func New(ctx context.Context, tracer trace.Tracer, opts ...Option) *Hooks {
	// ctx 预留给未来的 provider/作用域派生，当前不使用。
	_ = ctx
	h := newHooks(tracer, opts...)
	return &Hooks{
		TraceHook:         h.hook,
		ContextCustomizer: h.customizer,
	}
}

// NewTraceHook 返回把 TraceEvent 映射为 OTel GenAI span 的 agent.TraceHook。
//
// 兼容入口：等价于 New 的 TraceHook 但不提供 ContextCustomizer，
// 工具 span 与 run span 无父子关系（仅补属性）。新代码建议迁移到 New。
//
// span 命名遵循 GenAI 语义约定：
//   - TraceTurnStart/TurnEnd → 单个 span "gen_ai.<agent 名>.agent.run"，
//     属性 gen_ai.system、gen_ai.request.model（配置了 ModelName 时）、
//     gen_ai.usage.input_tokens/output_tokens/total_tokens（有用量时）、
//     gen_ai.usage.cached_tokens（缓存命中 >0 时）；
//   - TraceToolCall → 独立短 span "execute_tool <name>"，
//     属性 gen_ai.tool.name、gen_ai.tool.call.arguments/result（摘要）。
//
// TurnStart 与 TurnEnd 是两次独立回调，TurnStart 启动的 span 存在 hook
// 闭包内的注册表中（以原始请求 ctx 为键），TurnEnd 时取出并结束。
// 注册表的键始终是"原始请求 ctx"：经 ContextCustomizer 注入 span 后的
// 派生 ctx 会在 hook 内通过 originalCtxKey 找回原始 ctx 再作键，
// 故无论内核以哪个 ctx 回调 TurnEnd，配对均可靠。
func NewTraceHook(tracer trace.Tracer, opts ...Option) agent.TraceHook {
	return newHooks(tracer, opts...).hook
}

// hookSet 是 TraceHook 与 ContextCustomizer 共享状态的闭包集合。
type hookSet struct {
	tracer trace.Tracer
	cfg    Options
	// runs 以原始请求 ctx 为键登记进行中的 run span；ctx 是可比较的
	// 指针类型，可直接作 map 键。TurnEnd 时删除，避免泄漏。
	runs sync.Map // map[context.Context]*runSpanState
}

func newHooks(tracer trace.Tracer, opts ...Option) *hookSet {
	cfg := Options{SystemName: defaultSystemName, AgentName: defaultAgentName}
	for _, o := range opts {
		o(&cfg)
	}
	return &hookSet{tracer: tracer, cfg: cfg}
}

// spanKey 从回调 ctx 还原注册表键：customizer 注入过 span 的派生 ctx
// 会携带原始请求 ctx；未注入时回调 ctx 本身就是原始请求 ctx。
func (h *hookSet) spanKey(ctx context.Context) context.Context {
	if orig, ok := ctx.Value(originalCtxKey{}).(context.Context); ok && orig != nil {
		return orig
	}
	return ctx
}

func (h *hookSet) hook(ctx context.Context, ev agent.TraceEvent) {
	switch ev.Kind {
	case agent.TraceTurnStart:
		key := h.spanKey(ctx)
		_, span := h.tracer.Start(ctx, h.cfg.runSpanName())
		span.SetAttributes(h.cfg.baseAttrs()...)
		if ev.Model != "" {
			span.SetAttributes(attribute.String("gen_ai.request.model", ev.Model))
		}
		if ev.Input != "" {
			span.SetAttributes(attribute.String("gen_ai.prompt", ev.Input))
		}
		// 同一键上的旧 run 未正常收尾时兜底关闭，避免悬挂 span。
		if old, ok := h.runs.Load(key); ok {
			old.(*runSpanState).span.End()
		}
		h.runs.Store(key, &runSpanState{span: span})

	case agent.TraceTurnEnd:
		key := h.spanKey(ctx)
		state, ok := h.runs.LoadAndDelete(key)
		if !ok {
			// TurnStart 缺失（如调用方只接 TurnEnd 场景）时直接构造单 span，
			// 起始时间只能以当前时间近似。
			_, span := h.tracer.Start(ctx, h.cfg.runSpanName(), trace.WithTimestamp(time.Now()))
			span.SetAttributes(h.cfg.baseAttrs()...)
			setUsageAndErr(span, ev)
			span.End()
			return
		}
		span := state.(*runSpanState).span
		setUsageAndErr(span, ev)
		span.End()

	case agent.TraceToolCall:
		// ctx 已由 customizer 注入 run span 时，此处 tracer.Start 会使
		// 工具 span 自动成为 run span 的子 span；未注入时保持独立。
		_, span := h.tracer.Start(ctx, "execute_tool "+ev.ToolName)
		span.SetAttributes(h.cfg.baseAttrs()...)
		span.SetAttributes(
			attribute.String("gen_ai.tool.name", ev.ToolName),
		)
		if ev.Arguments != "" {
			span.SetAttributes(attribute.String("gen_ai.tool.call.arguments", truncate(ev.Arguments)))
		}
		if ev.Result != "" {
			span.SetAttributes(attribute.String("gen_ai.tool.call.result", truncate(ev.Result)))
		}
		if ev.Err != nil {
			span.RecordError(ev.Err)
			span.SetStatus(codes.Error, ev.Err.Error())
		}
		span.End()
	}
}

// customizer 把 TurnStart 已启动的 run span 注入请求 ctx。
// 内核保证 TurnStart hook 先于 customizer 以同一原始请求 ctx 被调用，
// 故此处按 spanKey(ctx) 必能取到该轮 span。返回的 ctx 携带原始请求 ctx
// （originalCtxKey），保证后续 TurnEnd 回调仍能找回注册表键。
func (h *hookSet) customizer(ctx context.Context, ev agent.TraceEvent) context.Context {
	key := h.spanKey(ctx)
	state, ok := h.runs.Load(key)
	if !ok {
		return ctx
	}
	spanCtx := trace.ContextWithSpan(ctx, state.(*runSpanState).span)
	return context.WithValue(spanCtx, originalCtxKey{}, key)
}

// runSpanName 返回一轮对话的 span 名。
func (o Options) runSpanName() string {
	return "gen_ai." + o.AgentName + ".agent.run"
}

// baseAttrs 返回所有 span 共用的基础属性。
func (o Options) baseAttrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.system", o.SystemName),
	}
}

// setUsageAndErr 把轮次用量与错误写入 run span。
func setUsageAndErr(span trace.Span, ev agent.TraceEvent) {
	if u := ev.Usage; u != nil {
		span.SetAttributes(
			attribute.Int64("gen_ai.usage.input_tokens", int64(u.PromptTokens)),
			attribute.Int64("gen_ai.usage.output_tokens", int64(u.CompletionTokens)),
			attribute.Int64("gen_ai.usage.total_tokens", int64(u.TotalTokens)),
		)
		if u.CachedTokens > 0 {
			span.SetAttributes(attribute.Int64("gen_ai.usage.cached_tokens", int64(u.CachedTokens)))
		}
	}
	if ev.Err != nil {
		span.RecordError(ev.Err)
		span.SetStatus(codes.Error, ev.Err.Error())
	}
}

// truncate 截断过长的内容属性。
func truncate(s string) string {
	if len(s) <= maxContentAttrLen {
		return s
	}
	return s[:maxContentAttrLen]
}
