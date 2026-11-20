package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// errorModel 在 Generate 时返回错误，用于模拟子 Agent 模型调用失败。
type errorModel struct {
	err error
}

func (m *errorModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, m.err
}

func (m *errorModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, m.err
}

func (m *errorModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func newDelegationTool(t *testing.T, opts DelegationOptions) tool.InvokableTool {
	t.Helper()
	dt, err := NewDelegationTool(opts)
	if err != nil {
		t.Fatalf("构建委派工具失败: %v", err)
	}
	return dt
}

func TestNewDelegationToolValidatesOptions(t *testing.T) {
	ctx := context.Background()
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}})

	if _, err := NewDelegationTool(DelegationOptions{Description: "d", SubAgent: sub}); err == nil {
		t.Fatal("Name 为空时应报错")
	}
	if _, err := NewDelegationTool(DelegationOptions{Name: "n", SubAgent: sub}); err == nil {
		t.Fatal("Description 为空时应报错")
	}
	if _, err := NewDelegationTool(DelegationOptions{Name: "n", Description: "d"}); err == nil {
		t.Fatal("SubAgent 为空时应报错")
	}

	info, err := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub}).Info(ctx)
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "delegate" || info.Desc != "委派任务" {
		t.Fatalf("工具信息 = %+v", info)
	}
}

func TestDelegationEmptyInput(t *testing.T) {
	ctx := context.Background()
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "hi"}})
	dt := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	_, err := dt.InvokableRun(ctx, `{"input":""}`)
	if err == nil {
		t.Fatal("input 为空时应报错")
	}
	if !strings.Contains(err.Error(), "input 不能为空") {
		t.Fatalf("错误信息 = %v", err)
	}

	// input 字段缺失同样视为空。
	if _, err := dt.InvokableRun(ctx, `{}`); err == nil {
		t.Fatal("缺少 input 字段时应报错")
	}
}

func TestDelegationReturnsSubReply(t *testing.T) {
	ctx := context.Background()
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "子任务完成"}})
	dt := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	out, err := dt.InvokableRun(ctx, `{"input":"帮我写个摘要"}`)
	if err != nil {
		t.Fatalf("委派失败: %v", err)
	}
	if out != "子任务完成" {
		t.Fatalf("工具结果 = %q, want 子任务完成", out)
	}

	// 子 Agent 会话写入了一轮。
	history, err := sub.History(ctx)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(history) != 2 || history[0].Content != "帮我写个摘要" || history[1].Content != "子任务完成" {
		t.Fatalf("子 Agent 历史 = %+v", history)
	}
}

func TestDelegationSubAgentError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("模型宕机")
	sub := newTestAgent(t, &errorModel{err: wantErr})
	dt := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	_, err := dt.InvokableRun(ctx, `{"input":"任务"}`)
	if err == nil {
		t.Fatal("子 Agent 失败时应返回错误")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("错误应包装原始错误: %v", err)
	}
}

func TestDelegationSubAgentHistoryPersists(t *testing.T) {
	ctx := context.Background()
	sub := newTestAgent(t, &staticModel{reply: &schema.Message{Role: schema.Assistant, Content: "收到"}})
	dt := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	if _, err := dt.InvokableRun(ctx, `{"input":"第一件事"}`); err != nil {
		t.Fatalf("第一次委派失败: %v", err)
	}
	first, err := sub.History(ctx)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("第一次委派后历史条数 = %d, want 2", len(first))
	}

	if _, err := dt.InvokableRun(ctx, `{"input":"第二件事"}`); err != nil {
		t.Fatalf("第二次委派失败: %v", err)
	}
	second, err := sub.History(ctx)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(second) != 4 {
		t.Fatalf("第二次委派后历史条数 = %d, want 4", len(second))
	}
	if second[0].Content != "第一件事" || second[2].Content != "第二件事" {
		t.Fatalf("子 Agent 历史未延续: %+v", second)
	}
}
