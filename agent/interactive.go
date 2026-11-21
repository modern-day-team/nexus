package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
)

// 本文件实现 HITL 交互式审批桥 InteractiveApprover：
// 工具调用时经 ctx 上的 emitter 发出 EventApprovalRequest，
// 外部通过事件上的 Respond 提交决定后继续或拒绝。

// 交互式审批的错误文案，作为拒绝原因反馈给模型。
const (
	errNoEmitter        = "当前模式不支持交互式审批（需 ChatStreamEvents）"
	errApprovalCanceled = "审批已取消"
	errApprovalTimeout  = "审批超时"
)

// InteractiveApprover 是 HITL 审批桥，实现 ToolApprover。
// 每次工具调用生成 ApprovalRequest 并经 ctx 上的 emitter 发出
// EventApprovalRequest（Respond 提交决定到内部 channel），然后阻塞等待决定：
//   - Allow=true 放行；Allow=false 按 deniedToolPrefix+Reason 拒绝；
//   - timeout<=0 或等待超过 timeout：视为拒绝（Reason="审批超时"）；
//   - ctx 取消：立即拒绝（Reason="审批已取消"）。
//
// 无 emitter 的调用路径（如 Chat）：直接拒绝（见 errNoEmitter）。
type InteractiveApprover struct {
	timeout time.Duration
}

// NewInteractiveApprover 构造交互式审批桥。timeout 为等待外部决定的时限，
// timeout<=0 表示无限等待（仍受 ctx 取消约束）。
func NewInteractiveApprover(timeout time.Duration) *InteractiveApprover {
	return &InteractiveApprover{timeout: timeout}
}

// Approve 实现 ToolApprover：发出审批事件并阻塞等待外部决定。
// 返回 nil 表示放行；返回错误表示拒绝（错误文本作为工具结果反馈给模型）。
func (a *InteractiveApprover) Approve(ctx context.Context, call schema.ToolCall) error {
	ch := make(chan ApprovalDecision, 1)
	var once sync.Once
	respond := func(d ApprovalDecision) {
		once.Do(func() { ch <- d })
	}

	req := newApprovalRequest(call.Function.Name, call.Function.Arguments)
	emitEvent(ctx, Event{Type: EventApprovalRequest, Request: req, Respond: respond})

	// 无 emitter 时 emitEvent 为 no-op，外部无法 Respond，直接拒绝。
	if _, ok := ctx.Value(emitterCtxKey{}).(eventEmitter); !ok {
		return errors.New(errNoEmitter)
	}

	var timer <-chan time.Time
	if a.timeout > 0 {
		timer = time.After(a.timeout)
	}

	select {
	case d := <-ch:
		if d.Allow {
			return nil
		}
		return errors.New(d.Reason)
	case <-timer:
		return errors.New(errApprovalTimeout)
	case <-ctx.Done():
		return errors.New(errApprovalCanceled)
	}
}

// Approver 返回适配 ToolApprover 类型的函数值，便于直接传入 Options.ToolApprover。
func (a *InteractiveApprover) Approver() ToolApprover {
	return a.Approve
}

var t0 = time.Now()

func startTime() time.Time { return t0 }
