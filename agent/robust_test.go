package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// slowTool 响应 ctx 取消的慢工具：睡 sleep 后返回 "slow"；ctx 到期时提前返回错误。
func newSlowTool(t *testing.T, sleep time.Duration) tool.BaseTool {
	t.Helper()
	st, err := toolutils.InferTool("slow", "慢工具", func(ctx context.Context, _ echoInput) (string, error) {
		timer := time.NewTimer(sleep)
		defer timer.Stop()
		select {
		case <-timer.C:
			return "slow", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("构建 slow 工具失败: %v", err)
	}
	return st
}

// panicTool 无条件 panic。
func newPanicTool(t *testing.T, name string) tool.BaseTool {
	t.Helper()
	pt, err := toolutils.InferTool(name, "panic 工具", func(_ context.Context, _ echoInput) (string, error) {
		panic("boom: 工具炸了")
	})
	if err != nil {
		t.Fatalf("构建 panic 工具失败: %v", err)
	}
	return pt
}

// runToolFlow 用 scriptedModel 驱动一次工具调用，返回 EventToolEnd。
func runToolFlow(t *testing.T, a *Agent) Event {
	t.Helper()
	stream, err := a.ChatStreamEvents(context.Background(), "use tool")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var toolEnd Event
	for {
		ev, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("Recv 失败: %v", rerr)
		}
		if ev.Type == EventToolEnd {
			toolEnd = ev
		}
	}
	return toolEnd
}

func toolCallStream(name string) []*schema.Message {
	return []*schema.Message{{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: schema.FunctionCall{Name: name, Arguments: `{"text":"hi"}`},
		}},
	}}
}

// 包装层级单元验证：observe 最外层 → approval 中间层 → robust 最内层。
func TestSetupToolsLayering(t *testing.T) {
	inner := newEchoTool(t)
	approverCalled := false
	tools := setupTools([]tool.BaseTool{inner}, func(_ context.Context, _ schema.ToolCall) error {
		approverCalled = true
		return nil
	}, 100*time.Millisecond)

	if len(tools) != 1 {
		t.Fatalf("工具数 = %d, want 1", len(tools))
	}
	observed, ok := tools[0].(*observedInvokableTool)
	if !ok {
		t.Fatalf("最外层应为 observedInvokableTool, got %T", tools[0])
	}
	approved, ok := observed.InvokableTool.(*approvedTool)
	if !ok {
		t.Fatalf("中间层应为 approvedTool, got %T", observed.InvokableTool)
	}
	robust, ok := approved.InvokableTool.(*robustTool)
	if !ok {
		t.Fatalf("最内层应为 robustTool, got %T", approved.InvokableTool)
	}
	if robust.timeout != 100*time.Millisecond {
		t.Fatalf("robustTool.timeout = %v, want 100ms", robust.timeout)
	}

	// 执行一次验证审批先于工具发生。
	out, err := approved.InvokableRun(context.Background(), `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun 失败: %v", err)
	}
	if out != "hi" || !approverCalled {
		t.Fatalf("out=%q approverCalled=%v", out, approverCalled)
	}
}

func TestToolTimeout(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "final"}
	a, err := New(context.Background(), Options{
		ChatModel:   &scriptedModel{streams: [][]*schema.Message{toolCallStream("slow"), {final}}},
		Tools:       []tool.BaseTool{newSlowTool(t, 100*time.Millisecond)},
		ToolTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}

	start := time.Now()
	toolEnd := runToolFlow(t, a)
	if elapsed := time.Since(start); elapsed > 90*time.Millisecond {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
	if !strings.Contains(toolEnd.Result, "超时") {
		t.Fatalf("工具结果应含超时错误文本, got %q", toolEnd.Result)
	}
	if toolEnd.Err == nil || !strings.Contains(toolEnd.Err.Error(), "超时") {
		t.Fatalf("EventToolEnd.Err 应非空且含超时, got %v", toolEnd.Err)
	}
}

func TestToolTimeoutDisabled(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "final"}
	a, err := New(context.Background(), Options{
		ChatModel: &scriptedModel{streams: [][]*schema.Message{toolCallStream("slow"), {final}}},
		Tools:     []tool.BaseTool{newSlowTool(t, 20*time.Millisecond)},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	toolEnd := runToolFlow(t, a)
	if toolEnd.Err != nil || toolEnd.Result != "slow" {
		t.Fatalf("不限时时工具应正常返回, result=%q err=%v", toolEnd.Result, toolEnd.Err)
	}
}

func TestToolPanicRecovered(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "final"}
	a, err := New(context.Background(), Options{
		ChatModel: &scriptedModel{streams: [][]*schema.Message{toolCallStream("boom"), {final}}},
		Tools:     []tool.BaseTool{newPanicTool(t, "boom")},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	toolEnd := runToolFlow(t, a)
	if !strings.Contains(toolEnd.Result, "tool panic") {
		t.Fatalf("工具结果应含 tool panic, got %q", toolEnd.Result)
	}
	if toolEnd.Err == nil || !strings.Contains(toolEnd.Err.Error(), "tool panic: boom") {
		t.Fatalf("Err 应含 panic 信息与堆栈, got %v", toolEnd.Err)
	}
	if len(toolEnd.Err.Error()) < 64 {
		t.Fatal("Err 中应包含堆栈摘要")
	}
}

func TestToolPanicWithApproval(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "final"}
	a, err := New(context.Background(), Options{
		ChatModel: &scriptedModel{streams: [][]*schema.Message{toolCallStream("boom"), {final}}},
		Tools:     []tool.BaseTool{newPanicTool(t, "boom")},
		ToolApprover: func(_ context.Context, _ schema.ToolCall) error {
			return nil // 全部放行
		},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	toolEnd := runToolFlow(t, a)
	if !strings.Contains(toolEnd.Result, "tool panic") {
		t.Fatalf("放行后 panic 应被恢复为工具结果, got %q", toolEnd.Result)
	}
}

func TestToolApprovalDenialUnaffected(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "final"}
	a, err := New(context.Background(), Options{
		ChatModel: &scriptedModel{streams: [][]*schema.Message{toolCallStream("echo"), {final}}},
		Tools:     []tool.BaseTool{newEchoTool(t)},
		ToolApprover: func(_ context.Context, _ schema.ToolCall) error {
			return errors.New("不行")
		},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	toolEnd := runToolFlow(t, a)
	if !strings.Contains(toolEnd.Result, deniedToolPrefix) {
		t.Fatalf("拒绝结果 = %q", toolEnd.Result)
	}
	if toolEnd.Err != nil {
		t.Fatalf("拒绝不应作为错误返回: %v", toolEnd.Err)
	}
}

func TestDroppedEventsNormalTurnIsZero(t *testing.T) {
	final := &schema.Message{Role: schema.Assistant, Content: "done"}
	a := newTestAgent(t, &staticModel{reply: final})

	stream, err := a.ChatStreamEvents(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	for {
		_, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("Recv 失败: %v", rerr)
		}
	}
	stream.Close()

	if got := a.DroppedEvents(); got != 0 {
		t.Fatalf("正常消费一轮后 DroppedEvents = %d, want 0", got)
	}
}

// sink 丢弃场景单测：本轮结束后（done 关闭）到达的事件计入 sink 局部
// 计数与 Agent 级汇总。
func TestEventSinkDropCounting(t *testing.T) {
	var agentDropped atomic.Int64
	done := make(chan struct{})
	_, writer := schema.Pipe[Event](2)
	sink := &eventSink{writer: writer, done: done, dropped: &agentDropped}

	close(done)
	sink.emit(Event{Type: EventToken, Content: "late"})

	if got := sink.drops.Load(); got != 1 {
		t.Fatalf("sink.drops = %d, want 1", got)
	}
	if got := agentDropped.Load(); got != 1 {
		t.Fatalf("Agent 级丢弃计数 = %d, want 1", got)
	}

	// dropped 为 nil（独立构造的 sink）不 panic。
	standalone := &eventSink{writer: writer, done: done}
	standalone.emit(Event{Type: EventToken, Content: "late"})
	if got := standalone.drops.Load(); got != 1 {
		t.Fatalf("standalone.drops = %d, want 1", got)
	}
}

// 消费方提前 Close 流不崩溃；轮次正常收尾。
func TestCloseStreamEarlyDoesNotCrash(t *testing.T) {
	turnEnded := make(chan struct{}, 1)
	final := &schema.Message{Role: schema.Assistant, Content: "done"}
	a, err := New(context.Background(), Options{
		ChatModel: &staticModel{reply: final},
		TraceHook: func(_ context.Context, ev TraceEvent) {
			if ev.Kind == TraceTurnEnd {
				turnEnded <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}

	stream, err := a.ChatStreamEvents(context.Background(), "hi")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	stream.Close() // 立即关闭，不消费

	select {
	case <-turnEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("轮次未收尾")
	}
}
