// Package otelx 把 agent 内核的 TraceEvent 映射为遵循
// OpenTelemetry GenAI 语义约定的 span。
//
// 用法：
//
//	tracer := otel.Tracer("nexus")
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

// Options 配置 NewTraceHook 的映射行为。
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
// 以请求 ctx 为键串联 TraceTurnStart 与 TraceTurnEnd 两次回调。
type runSpanState struct {
	span trace.Span
}

// NewTraceHook 返回把 TraceEvent 映射为 OTel GenAI span 的 agent.TraceHook。
//
// span 命名遵循 GenAI 语义约定：
//   - TraceTurnStart/TurnEnd → 单个 span "gen_ai.<agent 名>.agent.run"，
//     属性 gen_ai.system、gen_ai.usage.input_tokens/output_tokens（有用量时）；
//   - TraceToolCall → 独立短 span "execute_tool <name>"，
//     属性 gen_ai.tool.name、gen_ai.tool.call.arguments/result（摘要）。
//
// TurnStart 与 TurnEnd 是两次独立回调且内核不会把派生 ctx 传回，
// 因此 TurnStart 启动的 span 存在 hook 闭包内的注册表中（以请求 ctx 为键），
// TurnEnd 时取出并结束。TurnStart 与 TurnEnd 使用同一请求 ctx（ChatStream 的
// 后台 goroutine 经 context.WithoutCancel 包装，值与指针均保留），
// 故配对可靠；未配对的 TurnStart span 会被下一次同 ctx 的 TurnStart 兜底关闭。
func NewTraceHook(tracer trace.Tracer, opts ...Option) agent.TraceHook {
	cfg := Options{SystemName: defaultSystemName, AgentName: defaultAgentName}
	for _, o := range opts {
		o(&cfg)
	}

	// runs 以请求 ctx 为键登记进行中的 run span；ctx 是可比较的指针类型，
	// 可直接作 map 键。TurnEnd 时删除，避免泄漏。
	var runs sync.Map // map[context.Context]*runSpanState

	return func(ctx context.Context, ev agent.TraceEvent) {
		switch ev.Kind {
		case agent.TraceTurnStart:
			_, span := tracer.Start(ctx, cfg.runSpanName())
			span.SetAttributes(cfg.baseAttrs()...)
			if ev.Input != "" {
				span.SetAttributes(attribute.String("gen_ai.prompt", ev.Input))
			}
			// 同一 ctx 上的旧 run 未正常收尾时兜底关闭，避免悬挂 span。
			if old, ok := runs.Load(ctx); ok {
				old.(*runSpanState).span.End()
			}
			runs.Store(ctx, &runSpanState{span: span})

		case agent.TraceTurnEnd:
			state, ok := runs.LoadAndDelete(ctx)
			if !ok {
				// TurnStart 缺失（如调用方只接 TurnEnd 场景）时直接构造单 span，
				// 起始时间只能以当前时间近似。
				_, span := tracer.Start(ctx, cfg.runSpanName(), trace.WithTimestamp(time.Now()))
				span.SetAttributes(cfg.baseAttrs()...)
				setUsageAndErr(span, ev)
				span.End()
				return
			}
			span := state.(*runSpanState).span
			setUsageAndErr(span, ev)
			span.End()

		case agent.TraceToolCall:
			_, span := tracer.Start(ctx, "execute_tool "+ev.ToolName)
			span.SetAttributes(cfg.baseAttrs()...)
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
