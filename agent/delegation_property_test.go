package agent

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 本文件为委派工具（delegation）的属性测试：
// 随机进行 3~10 轮委派（输入交替生成），断言
//   1. 每次委派的返回值等于子 Agent 当轮回复的 Content；
//   2. 子 Agent 的 History 随委派轮次单调递增（每轮写入 user+assistant 两条）。
// 使用固定 scriptedModel 序列与固定随机种子，失败可复现。

// propScriptedModel 按脚本顺序依次返回回复；脚本耗尽后返回空回复。
type propScriptedModel struct {
	replies []string
	idx     int
}

func (m *propScriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	reply := ""
	if m.idx < len(m.replies) {
		reply = m.replies[m.idx]
		m.idx++
	}
	return &schema.Message{Role: schema.Assistant, Content: reply}, nil
}

func (m *propScriptedModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *propScriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

const propDelegationSeed = int64(20261123)

func TestPropertyDelegationMultiRound(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(propDelegationSeed))
	rounds := 3 + rng.Intn(8) // 3~10 轮

	// 固定脚本序列：第 i 轮委派的子 Agent 回复内容。
	replies := make([]string, rounds)
	for i := range replies {
		replies[i] = fmt.Sprintf("子任务回复-%d", i)
	}
	sub := newTestAgent(t, &propScriptedModel{replies: replies})
	dt := newDelegationTool(t, DelegationOptions{Name: "delegate", Description: "委派任务", SubAgent: sub})

	prevLen := 0
	for i := 0; i < rounds; i++ {
		input := fmt.Sprintf("委派输入-%d", i)
		out, err := dt.InvokableRun(ctx, fmt.Sprintf(`{"input":%q}`, input))
		if err != nil {
			t.Fatalf("seed=%d 第 %d 轮委派失败: %v", propDelegationSeed, i, err)
		}
		// 断言 1：返回值等于子 Agent 当轮回复的 Content。
		if out != replies[i] {
			t.Errorf("seed=%d 第 %d 轮: 工具结果 = %q, want %q", propDelegationSeed, i, out, replies[i])
		}

		history, err := sub.History(ctx)
		if err != nil {
			t.Fatalf("seed=%d 第 %d 轮: History 失败: %v", propDelegationSeed, i, err)
		}
		// 断言 2：History 单调递增，且每轮恰好追加 user+assistant 两条。
		if len(history) <= prevLen {
			t.Errorf("seed=%d 第 %d 轮: 子 Agent 历史 %d 条未比上一轮 %d 条多", propDelegationSeed, i, len(history), prevLen)
		}
		if len(history) != 2*(i+1) {
			t.Errorf("seed=%d 第 %d 轮: 子 Agent 历史 = %d 条, want %d 条", propDelegationSeed, i, len(history), 2*(i+1))
		}
		prevLen = len(history)

		// 每轮写入的内容：user 为委派输入，assistant 为脚本回复。
		if got := history[2*i]; got == nil || got.Role != schema.User || got.Content != input {
			t.Errorf("seed=%d 第 %d 轮: 历史[%d] = (%s, %q), want (user, %q)", propDelegationSeed, i, 2*i, got.Role, got.Content, input)
		}
		if got := history[2*i+1]; got == nil || got.Role != schema.Assistant || got.Content != replies[i] {
			t.Errorf("seed=%d 第 %d 轮: 历史[%d] = (%s, %q), want (assistant, %q)", propDelegationSeed, i, 2*i+1, got.Role, got.Content, replies[i])
		}
	}
}
