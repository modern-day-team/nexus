package otelx

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/modern-day-team/nexus/agent"
)

// staticModel 固定回复。
type staticModel struct {
	reply *schema.Message
}

func (m *staticModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.reply, nil
}

func (m *staticModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.reply}), nil
}

func (m *staticModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// scriptedModel 按脚本先调用工具再给最终回复。
type scriptedModel struct {
	streams [][]*schema.Message
	idx     int
}

func (m *scriptedModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if m.idx < len(m.streams) {
		msgs := m.streams[m.idx]
		m.idx++
		return msgs[0], nil
	}
	return m.streams[len(m.streams)-1][0], nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := m.streams[len(m.streams)-1]
	if m.idx < len(m.streams) {
		chunks = m.streams[m.idx]
		m.idx++
	}
	return schema.StreamReaderFromArray(chunks), nil
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type echoInput struct {
	Text string `json:"text" jsonschema:"required"`
}

func newEchoTool(t *testing.T) tool.BaseTool {
	t.Helper()
	echo, err := toolutils.InferTool("echo", "回显输入文本", func(_ context.Context, in echoInput) (string, error) {
		return "echo:" + in.Text, nil
	})
	if err != nil {
		t.Fatalf("构建 echo 工具失败: %v", err)
	}
	return echo
}

// newRecorderAgent 构建带 OTel trace hook 的 Agent 与 SpanRecorder。
func newRecorderAgent(t *testing.T, cm model.ToolCallingChatModel, tools ...tool.BaseTool) (*agent.Agent, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	hook := NewTraceHook(tp.Tracer("nexus-test"))

	a, err := agent.New(context.Background(), agent.Options{
		ChatModel: cm,
		Tools:     tools,
		TraceHook: hook,
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	return a, rec
}

func spanByName(rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestChatProducesAgentRunSpan(t *testing.T) {
	a, rec := newRecorderAgent(t, &staticModel{reply: &schema.Message{
		Role:    schema.Assistant,
		Content: "hello",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8,
		}},
	}})

	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("span 数 = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "gen_ai.agent.agent.run" {
		t.Fatalf("span 名 = %q, want gen_ai.agent.agent.run", span.Name())
	}

	attrs := map[string]interface{}{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if attrs["gen_ai.system"] != "nexus" {
		t.Fatalf("gen_ai.system = %v", attrs["gen_ai.system"])
	}
	if attrs["gen_ai.usage.input_tokens"] != int64(3) {
		t.Fatalf("gen_ai.usage.input_tokens = %v, want 3", attrs["gen_ai.usage.input_tokens"])
	}
	if attrs["gen_ai.usage.output_tokens"] != int64(5) {
		t.Fatalf("gen_ai.usage.output_tokens = %v, want 5", attrs["gen_ai.usage.output_tokens"])
	}
	if span.Status().Code != codes.Unset {
		t.Fatalf("正常轮次 span status = %v, want Unset", span.Status())
	}
}

func TestToolCallProducesExecuteToolSpan(t *testing.T) {
	final := &schema.Message{
		Role:    schema.Assistant,
		Content: "final",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
		}},
	}
	toolCall := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"hi"}`},
		}},
	}

	a, rec := newRecorderAgent(t,
		&scriptedModel{streams: [][]*schema.Message{{toolCall}, {final}}},
		newEchoTool(t),
	)

	if _, err := a.Chat(context.Background(), "use echo"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	runSpan := spanByName(rec, "gen_ai.agent.agent.run")
	if runSpan == nil {
		t.Fatalf("缺少 agent.run span, got %d spans", len(rec.Ended()))
	}
	toolSpan := spanByName(rec, "execute_tool echo")
	if toolSpan == nil {
		t.Fatalf("缺少 execute_tool span, got %d spans", len(rec.Ended()))
	}

	toolAttrs := map[string]interface{}{}
	for _, kv := range toolSpan.Attributes() {
		toolAttrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if toolAttrs["gen_ai.tool.name"] != "echo" {
		t.Fatalf("gen_ai.tool.name = %v", toolAttrs["gen_ai.tool.name"])
	}
	if toolAttrs["gen_ai.tool.call.arguments"] != `{"text":"hi"}` {
		t.Fatalf("arguments = %v", toolAttrs["gen_ai.tool.call.arguments"])
	}
	if toolAttrs["gen_ai.tool.call.result"] != "echo:hi" {
		t.Fatalf("result = %v", toolAttrs["gen_ai.tool.call.result"])
	}

	runAttrs := map[string]interface{}{}
	for _, kv := range runSpan.Attributes() {
		runAttrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if runAttrs["gen_ai.usage.input_tokens"] != int64(10) {
		t.Fatalf("run span input_tokens = %v, want 10", runAttrs["gen_ai.usage.input_tokens"])
	}
}

func TestNoUsageAndErrorAttributesAreSafe(t *testing.T) {
	// 无用量回复。
	a, rec := newRecorderAgent(t, &staticModel{reply: &schema.Message{
		Role: schema.Assistant, Content: "plain",
	}})
	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	span := spanByName(rec, "gen_ai.agent.agent.run")
	if span == nil {
		t.Fatalf("缺少 agent.run span")
	}
	for _, kv := range span.Attributes() {
		if string(kv.Key) == "gen_ai.usage.input_tokens" {
			t.Fatalf("无用量时不应有 input_tokens 属性")
		}
	}

	// 出错轮次：模型返回错误。
	errModel := &errModel{}
	a2, rec2 := newRecorderAgent(t, errModel)
	if _, err := a2.Chat(context.Background(), "hi"); err == nil {
		t.Fatalf("期望 Chat 返回错误")
	}
	span2 := spanByName(rec2, "gen_ai.agent.agent.run")
	if span2 == nil {
		t.Fatalf("缺少 agent.run span")
	}
	if span2.Status().Code != codes.Error {
		t.Fatalf("错误轮次 span status = %v, want Error", span2.Status())
	}
}

type errModel struct{}

func (m *errModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("boom")
}

func (m *errModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("boom")
}

func (m *errModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// newRecorderAgentWithCustomizer 构建带 New 双注册钩子（span 父子嵌套）的 Agent。
func newRecorderAgentWithCustomizer(t *testing.T, cm model.ToolCallingChatModel, tools ...tool.BaseTool) (*agent.Agent, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	hooks := New(context.Background(), tp.Tracer("nexus-test"))

	a, err := agent.New(context.Background(), agent.Options{
		ChatModel:         cm,
		Tools:             tools,
		TraceHook:         hooks.TraceHook,
		ContextCustomizer: hooks.ContextCustomizer,
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	return a, rec
}

func attrsOf(s sdktrace.ReadOnlySpan) map[string]interface{} {
	attrs := map[string]interface{}{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	return attrs
}

func scriptedEchoFixture(t *testing.T) (*schema.Message, *schema.Message, *schema.Message) {
	t.Helper()
	final := &schema.Message{
		Role:    schema.Assistant,
		Content: "final",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
			PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 4},
		}},
	}
	toolCall := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "echo", Arguments: `{"text":"hi"}`},
		}},
	}
	return toolCall, final, final
}

func TestNestingToolSpanIsChildOfRunSpan(t *testing.T) {
	toolCall, final, _ := scriptedEchoFixture(t)
	a, rec := newRecorderAgentWithCustomizer(t,
		&scriptedModel{streams: [][]*schema.Message{{toolCall}, {final}}},
		newEchoTool(t),
	)

	if _, err := a.Chat(context.Background(), "use echo"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	runSpan := spanByName(rec, "gen_ai.agent.agent.run")
	if runSpan == nil {
		t.Fatalf("缺少 agent.run span, got %d spans", len(rec.Ended()))
	}
	toolSpan := spanByName(rec, "execute_tool echo")
	if toolSpan == nil {
		t.Fatalf("缺少 execute_tool span, got %d spans", len(rec.Ended()))
	}
	if toolSpan.Parent().SpanID() != runSpan.SpanContext().SpanID() {
		t.Fatalf("tool span parent = %v, want run span %v",
			toolSpan.Parent().SpanID(), runSpan.SpanContext().SpanID())
	}

	// gen_ai.request.model 与 cached_tokens 属性（本次通过 ModelName 配置）。
	ra := attrsOf(runSpan)
	if ra["gen_ai.usage.cached_tokens"] != int64(4) {
		t.Fatalf("cached_tokens = %v, want 4", ra["gen_ai.usage.cached_tokens"])
	}
}

func TestModelAttributeOnRunSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	hooks := New(context.Background(), tp.Tracer("nexus-test"))

	a, err := agent.New(context.Background(), agent.Options{
		ChatModel:         &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}},
		TraceHook:         hooks.TraceHook,
		ContextCustomizer: hooks.ContextCustomizer,
		ModelName:         "glm-5",
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	if _, err := a.Chat(context.Background(), "hi"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	attrs := attrsOf(spanByName(rec, "gen_ai.agent.agent.run"))
	if attrs["gen_ai.request.model"] != "glm-5" {
		t.Fatalf("gen_ai.request.model = %v, want glm-5", attrs["gen_ai.request.model"])
	}
}

func TestLegacyHookKeepsIndependentToolSpan(t *testing.T) {
	toolCall, final, _ := scriptedEchoFixture(t)
	// 旧 NewTraceHook：无 customizer，工具 span 与 run span 独立。
	a, rec := newRecorderAgent(t,
		&scriptedModel{streams: [][]*schema.Message{{toolCall}, {final}}},
		newEchoTool(t),
	)
	if _, err := a.Chat(context.Background(), "use echo"); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	runSpan := spanByName(rec, "gen_ai.agent.agent.run")
	toolSpan := spanByName(rec, "execute_tool echo")
	if runSpan == nil || toolSpan == nil {
		t.Fatalf("缺少 span, got %d spans", len(rec.Ended()))
	}
	if toolSpan.Parent().SpanID().IsValid() {
		t.Fatalf("旧 hook 下工具 span 不应有父 span, got %v", toolSpan.Parent().SpanID())
	}
}

func TestNestingAcrossChatStreamEvents(t *testing.T) {
	toolCall, final, _ := scriptedEchoFixture(t)
	a, rec := newRecorderAgentWithCustomizer(t,
		&scriptedModel{streams: [][]*schema.Message{{toolCall}, {final}}},
		newEchoTool(t),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			break
		}
		if ev.Type == agent.EventDone {
			break
		}
	}

	runSpan := spanByName(rec, "gen_ai.agent.agent.run")
	toolSpan := spanByName(rec, "execute_tool echo")
	if runSpan == nil || toolSpan == nil {
		t.Fatalf("缺少 span, got %d spans", len(rec.Ended()))
	}
	if toolSpan.Parent().SpanID() != runSpan.SpanContext().SpanID() {
		t.Fatalf("ChatStreamEvents 下 tool span parent = %v, want %v",
			toolSpan.Parent().SpanID(), runSpan.SpanContext().SpanID())
	}
}
