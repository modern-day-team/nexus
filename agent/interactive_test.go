package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// countingTool 记录工具被实际执行的次数。
type countingTool struct {
	mu    sync.Mutex
	calls int
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newCountingTool(t *testing.T, name string, ct *countingTool) tool.BaseTool {
	t.Helper()
	return newEchoToolNamed(t, name, func() { ct.mu.Lock(); ct.calls++; ct.mu.Unlock() })
}

// newEchoToolNamed 构建执行时回调 onRun 的 echo 工具。
func newEchoToolNamed(t *testing.T, name string, onRun func()) tool.BaseTool {
	t.Helper()
	impl, err := toolutils.InferTool(name, "回显输入文本", func(_ context.Context, in echoInput) (string, error) {
		onRun()
		return in.Text, nil
	})
	if err != nil {
		t.Fatalf("构建 %s 工具失败: %v", name, err)
	}
	return impl
}

// toolCallMsg 构造一次工具调用的 assistant 消息。
func toolCallMsg(name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_" + name,
			Type:     "function",
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// newInteractiveAgent 构建带交互式审批的测试 Agent。
func newInteractiveAgent(t *testing.T, ia *InteractiveApprover, cm model.ToolCallingChatModel, tools ...tool.BaseTool) *Agent {
	t.Helper()
	a, err := New(context.Background(), Options{
		ChatModel:    cm,
		Tools:        tools,
		ToolApprover: ia.Approver(),
	})
	if err != nil {
		t.Fatalf("构建 Agent 失败: %v", err)
	}
	return a
}

// runStreamToolFlow 消费事件流，对每个审批事件调用 onApproval，
// 返回最后的工具结果与 Done 事件。
func runStreamToolFlow(
	t *testing.T, stream *schema.StreamReader[Event],
	onApproval func(req *ApprovalRequest, respond func(ApprovalDecision)),
) (toolEnd string, done *Event) {
	t.Helper()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv 失败: %v", err)
		}
		switch ev.Type {
		case EventApprovalRequest:
			if ev.Request == nil || ev.Request.CallID == "" || ev.Respond == nil {
				t.Fatalf("审批事件不完整: %+v", ev)
			}
			if onApproval != nil {
				onApproval(ev.Request, ev.Respond)
			}
		case EventToolEnd:
			toolEnd = ev.Result
		case EventDone:
			e := ev
			done = &e
		}
	}
	return toolEnd, done
}

func TestInteractiveApproverAllow(t *testing.T) {
	ct := &countingTool{}
	a := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"hi"}`)},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		newCountingTool(t, "echo", ct),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	toolEnd, done := runStreamToolFlow(t, stream, func(req *ApprovalRequest, respond func(ApprovalDecision)) {
		if req.ToolName != "echo" || req.Arguments != `{"text":"hi"}` {
			t.Errorf("审批请求 = %+v", req)
		}
		respond(ApprovalDecision{Allow: true})
	})
	if done == nil || done.Err != nil {
		t.Fatalf("Done = %+v", done)
	}
	if toolEnd != "hi" {
		t.Fatalf("工具结果 = %q, want hi", toolEnd)
	}
	if ct.count() != 1 {
		t.Fatalf("工具执行次数 = %d, want 1", ct.count())
	}
}

func TestInteractiveApproverDeny(t *testing.T) {
	ct := &countingTool{}
	a := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"hi"}`)},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		newCountingTool(t, "echo", ct),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	toolEnd, done := runStreamToolFlow(t, stream, func(_ *ApprovalRequest, respond func(ApprovalDecision)) {
		respond(ApprovalDecision{Allow: false, Reason: "不允许"})
	})
	if done == nil || done.Err != nil {
		t.Fatalf("Done = %+v", done)
	}
	if !strings.Contains(toolEnd, deniedToolPrefix) || !strings.Contains(toolEnd, "不允许") {
		t.Fatalf("工具结果 = %q, want 含 %q 与原因", toolEnd, deniedToolPrefix)
	}
	if ct.count() != 0 {
		t.Fatalf("工具不应被执行, 执行次数 = %d", ct.count())
	}
}

func TestInteractiveApproverRespondTwiceIsNoOp(t *testing.T) {
	ct := &countingTool{}
	a := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"hi"}`)},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		newCountingTool(t, "echo", ct),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	toolEnd, done := runStreamToolFlow(t, stream, func(_ *ApprovalRequest, respond func(ApprovalDecision)) {
		respond(ApprovalDecision{Allow: true})
		respond(ApprovalDecision{Allow: false, Reason: "不应生效"}) // 二次调用 no-op
	})
	if done == nil || done.Err != nil {
		t.Fatalf("Done = %+v", done)
	}
	if toolEnd != "hi" {
		t.Fatalf("工具结果 = %q, want hi（二次 Respond 不应覆盖首次决定）", toolEnd)
	}
}

func TestInteractiveApproverTimeout(t *testing.T) {
	ct := &countingTool{}
	a := newInteractiveAgent(t, NewInteractiveApprover(50*time.Millisecond),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"hi"}`)},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		newCountingTool(t, "echo", ct),
	)

	stream, err := a.ChatStreamEvents(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var sawRequest bool
	toolEnd, done := runStreamToolFlow(t, stream, func(req *ApprovalRequest, _ func(ApprovalDecision)) {
		sawRequest = true
		if req.CallID == "" {
			t.Error("CallID 为空")
		}
		// 不 Respond，等待超时。
	})
	if !sawRequest {
		t.Fatal("未收到审批请求")
	}
	if done == nil || done.Err != nil {
		t.Fatalf("Done = %+v", done)
	}
	if !strings.Contains(toolEnd, deniedToolPrefix) || !strings.Contains(toolEnd, errApprovalTimeout) {
		t.Fatalf("工具结果 = %q, want 超时拒绝", toolEnd)
	}
	if ct.count() != 0 {
		t.Fatalf("超时后工具不应被执行, 执行次数 = %d", ct.count())
	}
}

// genScriptedModel 为 Generate 路径（Chat）准备的脚本模型：按次序返回消息。
type genScriptedModel struct {
	gens [][]*schema.Message
	idx  int
}

func (m *genScriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	msg := &schema.Message{Role: schema.Assistant, Content: "ok"}
	if m.idx < len(m.gens) {
		msgs := m.gens[m.idx]
		m.idx++
		return msgs[0], nil
	}
	return msg, nil
}

func (m *genScriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "ok"}}), nil
}

func (m *genScriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestInteractiveApproverNoEmitterChat(t *testing.T) {
	ct := &countingTool{}
	a := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&genScriptedModel{gens: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"hi"}`)},
			{{Role: schema.Assistant, Content: "final"}},
		}},
		newCountingTool(t, "echo", ct),
	)

	reply, err := a.Chat(context.Background(), "use echo")
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if reply.Content != "final" {
		t.Fatalf("回复 = %q, want final", reply.Content)
	}
	if ct.count() != 0 {
		t.Fatalf("无 emitter 时工具不应被执行, 执行次数 = %d", ct.count())
	}
}

func TestInteractiveApproverCtxCancel(t *testing.T) {
	ia := NewInteractiveApprover(0) // 不限时
	ctx, cancel := context.WithCancel(withEmitter(context.Background(), func(Event) {}))
	done := make(chan error, 1)
	go func() {
		done <- ia.Approve(ctx, schema.ToolCall{Function: schema.FunctionCall{Name: "echo", Arguments: "{}"}})
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != errApprovalCanceled {
			t.Fatalf("取消后 Approve = %v, want %s", err, errApprovalCanceled)
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Approve 未返回")
	}
}

func TestInteractiveDelegationApproval(t *testing.T) {
	ct := &countingTool{}
	// 子 Agent：带审批 + echo 工具。
	sub := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("echo", `{"text":"sub-ok"}`)},
			{{Role: schema.Assistant, Content: "sub-final"}},
		}},
		newCountingTool(t, "echo", ct),
	)
	dt, err := NewDelegationStreamTool(DelegationOptions{Name: "delegate", Description: "委派", SubAgent: sub})
	if err != nil {
		t.Fatalf("构建委派工具失败: %v", err)
	}
	// 父 Agent：同样使用审批（委派工具本身也会走父 approver，但 scriptedModel
	// 只调用 delegate 一次；父审批与子审批都在父事件流上可见，逐个放行）。
	parent := newInteractiveAgent(t, NewInteractiveApprover(time.Second),
		&scriptedModel{streams: [][]*schema.Message{
			{toolCallMsg("delegate", `{"input":"做点事"}`)},
			{{Role: schema.Assistant, Content: "parent-final"}},
		}},
		dt,
	)

	stream, err := parent.ChatStreamEvents(context.Background(), "委派任务")
	if err != nil {
		t.Fatalf("ChatStreamEvents 失败: %v", err)
	}
	defer stream.Close()

	var (
		sawSubRequest bool
		toolEnd       string
	)
	toolEnd, done := runStreamToolFlow(t, stream, func(req *ApprovalRequest, respond func(ApprovalDecision)) {
		// 子 Agent 的审批请求经父事件流透传，Agent 字段 = 委派工具名的事件
		// 已在 delegate_stream.go 覆盖；此处所有审批请求均放行。
		if req.Arguments == `{"text":"sub-ok"}` {
			sawSubRequest = true
		}
		respond(ApprovalDecision{Allow: true})
	})
	if done == nil || done.Err != nil {
		t.Fatalf("Done = %+v", done)
	}
	if !sawSubRequest {
		t.Fatal("父事件流未收到子 Agent 的审批请求")
	}
	if !strings.Contains(toolEnd, "sub-final") {
		t.Fatalf("委派工具结果 = %q, want 含 sub-final", toolEnd)
	}
	if ct.count() != 1 {
		t.Fatalf("子工具执行次数 = %d, want 1", ct.count())
	}
}

func TestInteractiveApproverConcurrentCalls(t *testing.T) {
	// 两个不同工具在两条 goroutine 中同时请求审批，CallID 独立、决定不串扰。
	ia := NewInteractiveApprover(time.Second)
	ctx := withEmitter(context.Background(), func(Event) {})

	type result struct {
		id     string
		decide func(ApprovalDecision)
		got    error
	}

	run := func(name string, decide func(*ApprovalRequest) ApprovalDecision, wg *sync.WaitGroup, out *result) {
		defer wg.Done()
		ch := make(chan error, 1)
		release := make(chan struct{})
		emitOnce := sync.Once{}
		go func() {
			err := ia.Approve(withEmitter(ctx, func(ev Event) {
				if ev.Type == EventApprovalRequest && ev.Request.ToolName == name {
					out.id = ev.Request.CallID
					out.decide = ev.Respond
					emitOnce.Do(func() { close(release) })
				}
			}), schema.ToolCall{Function: schema.FunctionCall{Name: name, Arguments: "{}"}})
			ch <- err
		}()
		select {
		case <-release:
		case <-time.After(time.Second):
			t.Errorf("%s: 未收到审批请求", name)
			return
		}
		dec := decide(&ApprovalRequest{CallID: out.id})
		out.decide(dec)
		out.got = <-ch
	}

	var wg sync.WaitGroup
	r1, r2 := result{}, result{}
	wg.Add(2)
	go func() {
		run("toolA", func(*ApprovalRequest) ApprovalDecision { return ApprovalDecision{Allow: true} }, &wg, &r1)
	}()
	go func() {
		run("toolB", func(*ApprovalRequest) ApprovalDecision { return ApprovalDecision{Allow: false, Reason: "拒绝B"} }, &wg, &r2)
	}()
	wg.Wait()

	if r1.id == "" || r2.id == "" || r1.id == r2.id {
		t.Fatalf("CallID 应独立且非空: %q vs %q", r1.id, r2.id)
	}
	if r1.got != nil {
		t.Fatalf("toolA 应放行, got %v", r1.got)
	}
	if r2.got == nil || r2.got.Error() != "拒绝B" {
		t.Fatalf("toolB 应拒绝, got %v", r2.got)
	}
}
