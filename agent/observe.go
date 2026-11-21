package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// 本文件是工具包装器（中间件）的唯一聚集地。
// 包装层级（从外到内，即执行顺序）：
//
//	observedInvokableTool   最外层：记录工具开始/结束事件。它看到的
//	                        Result/Err 是内层转换后的最终输出，因此
//	                        超时/panic 错误也会进入 EventToolEnd.Err；
//	                        同时它负责把工具错误转成文本结果（见下），
//	                        不中断 ReAct 循环。
//	approvedTool            中间层：审批问询，拒绝则短路返回（不执行
//	                        工具），拒绝文本经外层观测进入事件结果。
//	robustTool              最内层：真正执行工具，附加超时与 panic 恢复。
//
// 注意：观测必须放在审批外层——审批拒绝的短路文本才能进入
// EventToolEnd.Result（interactive_test 依赖该契约）。
// 对 Agent 而言只有 setupTools 一个入口。

// ToolApprover 在每次工具调用前被询问。返回 nil 表示放行；
// 返回错误表示拒绝，该错误会以文本形式作为工具结果反馈给模型
// （而不是中断本轮），模型可据此调整后续动作——
// 类似 Claude Code 的权限模式：用户拒绝后助手仍可继续对话。
type ToolApprover func(ctx context.Context, call schema.ToolCall) error

// deniedToolPrefix 是拒绝时写入工具结果的前缀，便于模型识别。
const deniedToolPrefix = "工具调用被用户拒绝"

// setupTools 组装工具调用链：事件观测（最外）→ 审批 → 超时/panic 防护（最内，
// 真正执行工具）。包装是从内到外逐层套上的：先 wrapTools 加 robust，再加
// approved，最后加 observe。审批拒绝在内层短路，拒绝文本经外层观测进入
// 事件结果（见文件头注释）。timeout <=0 表示不限时。
func setupTools(tools []tool.BaseTool, approver ToolApprover, timeout time.Duration) []tool.BaseTool {
	tools = robustTools(tools, timeout)
	if approver != nil {
		tools = wrapTools(tools, func(t tool.InvokableTool) tool.InvokableTool {
			return &approvedTool{InvokableTool: t, approver: approver}
		})
	}
	tools = observeTools(tools)
	return tools
}

// wrapTools 对实现了 InvokableTool 的工具逐个应用包装，其余保持不变。
func wrapTools(tools []tool.BaseTool, wrap func(tool.InvokableTool) tool.InvokableTool) []tool.BaseTool {
	out := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		if invokable, ok := t.(tool.InvokableTool); ok {
			out[i] = wrap(invokable)
			continue
		}
		out[i] = t
	}
	return out
}

// toolName 安全获取工具名，失败时返回空串。
func toolName(ctx context.Context, t tool.InvokableTool) string {
	if info, err := t.Info(ctx); err == nil && info != nil {
		return info.Name
	}
	return ""
}

// approvedTool 在工具执行前插入审批钩子。
type approvedTool struct {
	tool.InvokableTool
	approver ToolApprover
}

func (t *approvedTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	call := schema.ToolCall{Function: schema.FunctionCall{
		Name:      toolName(ctx, t.InvokableTool),
		Arguments: argumentsInJSON,
	}}
	if err := t.approver(ctx, call); err != nil {
		// 拒绝不作为错误返回，避免中断 ReAct 循环；模型会看到拒绝原因。
		return deniedToolPrefix + ": " + err.Error(), nil
	}
	return t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
}

// observedInvokableTool 在工具执行前后发出事件。未提供 emitter 时行为与原始工具一致。
type observedInvokableTool struct {
	tool.InvokableTool
}

func (t *observedInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	name := toolName(ctx, t.InvokableTool)

	emitEvent(ctx, Event{Type: EventToolStart, ToolName: name, Arguments: argumentsInJSON})

	out, err := t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)

	// 工具执行失败不中断 ReAct 循环：把错误文本并入工具结果反馈给模型
	// （与审批拒绝的处理一致，见 approvedTool），模型可据此调整后续动作。
	// 错误本身仍保留在 EventToolEnd.Err 与 trace 事件中供调用方观测。
	result := out
	if err != nil {
		result = "工具执行失败: " + err.Error()
		if out != "" {
			result = out + "\n" + result
		}
	}

	// 工具级 trace 事件：hook 由 ctx 传入（见 trace.go withTrace/emitTrace）。
	emitTrace(ctx, TraceEvent{
		Kind:      TraceToolCall,
		ToolName:  name,
		Arguments: argumentsInJSON,
		Result:    result,
		Err:       err,
	})

	emitEvent(ctx, Event{
		Type:      EventToolEnd,
		ToolName:  name,
		Arguments: argumentsInJSON,
		Result:    result,
		Err:       err,
	})
	return result, nil
}

// observeTools 为工具加上事件观测包装。
func observeTools(tools []tool.BaseTool) []tool.BaseTool {
	if len(tools) == 0 {
		return tools
	}
	return wrapTools(tools, func(t tool.InvokableTool) tool.InvokableTool {
		return &observedInvokableTool{InvokableTool: t}
	})
}

// applyApproval 为工具加上审批钩子。保留为导出测试与外部复用入口。
func applyApproval(tools []tool.BaseTool, approver ToolApprover) []tool.BaseTool {
	if approver == nil {
		return tools
	}
	return wrapTools(tools, func(t tool.InvokableTool) tool.InvokableTool {
		return &approvedTool{InvokableTool: t, approver: approver}
	})
}

// toolTimeoutCtxKey 标记 robustTool 已经给本次调用设置过超时，避免嵌套包装
// （例如委派链）重复叠加 deadline。
type toolTimeoutCtxKey struct{}

// robustTools 为工具加上超时与 panic 恢复包装。timeout <=0 时只做 panic 恢复。
func robustTools(tools []tool.BaseTool, timeout time.Duration) []tool.BaseTool {
	if len(tools) == 0 {
		return tools
	}
	return wrapTools(tools, func(t tool.InvokableTool) tool.InvokableTool {
		return &robustTool{InvokableTool: t, timeout: timeout}
	})
}

// robustTool 是最内层的健壮性包装：真正执行工具，并保证
//  1. 超时：timeout > 0 时用 context.WithTimeout 派生带超时的 ctx。
//     Go 的 WithTimeout 以调用方 ctx 为父，最终 deadline 是两者中较早到期的
//     一个——若调用方 ctx 已有更短 deadline，则不会延长它，天然取 min。
//  2. panic 恢复：工具 panic 不再击穿 ReAct 循环，而是转换为错误结果。
//     panic 的堆栈摘要（debug.Stack 前 2KB）放进返回的 error 里而非文本
//     结果里：文本结果会作为正常工具输出被模型当作数据消费，而堆栈是
//     调试信息，放 error 更符合"工具执行失败"的语义，也更便于上层日志
//     与 EventToolEnd.Err 捕获。
type robustTool struct {
	tool.InvokableTool
	timeout time.Duration
}

func (t *robustTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (out string, err error) {
	name := toolName(ctx, t.InvokableTool)

	if t.timeout > 0 {
		if v, ok := ctx.Value(toolTimeoutCtxKey{}).(bool); !ok || !v {
			// 注：WithTimeout 的最终 deadline 是父 deadline 与 timeout 的较早者，
			// 调用方 ctx 已有更短 deadline 时不会被延长。
			c, cancel := context.WithTimeout(ctx, t.timeout)
			ctx = context.WithValue(c, toolTimeoutCtxKey{}, true)
			defer cancel()
		}
	}

	defer func() {
		if r := recover(); r != nil {
			// 堆栈摘要放入 error（截断到 2KB），模型只会看到一行摘要文本。
			stack := debug.Stack()
			if len(stack) > 2*1024 {
				stack = stack[:2*1024]
			}
			err = fmt.Errorf("tool panic: %v\n堆栈摘要:\n%s", r, stack)
			out = ""
		}
	}()

	out, err = t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
	// 超时错误统一改写为带工具名与超时时长的友好信息，模型可据此调整。
	// 工具实现不一定返回 context.DeadlineExceeded 本身（可能返回包装错误或
	// 自行检查 ctx 后返回 canceled），因此以 ctx 最终状态为准：deadline 已
	// 到期且有错误返回时视为超时。
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("tool %q 超时（%s）", name, t.timeout)
	}
	return out, err
}
