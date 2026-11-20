package agent

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// 本文件是工具包装器（中间件）的唯一聚集地：
// 审批（approve）在内层先问询，事件观测（observe）在外层放行后才记录。
// 对 Agent 而言只有 setupTools 一个入口。

// ToolApprover 在每次工具调用前被询问。返回 nil 表示放行；
// 返回错误表示拒绝，该错误会以文本形式作为工具结果反馈给模型
// （而不是中断本轮），模型可据此调整后续动作——
// 类似 Claude Code 的权限模式：用户拒绝后助手仍可继续对话。
type ToolApprover func(ctx context.Context, call schema.ToolCall) error

// deniedToolPrefix 是拒绝时写入工具结果的前缀，便于模型识别。
const deniedToolPrefix = "工具调用被用户拒绝"

// setupTools 组装工具调用链：审批 → 事件观测。无任何包装时原样返回。
func setupTools(tools []tool.BaseTool, approver ToolApprover) []tool.BaseTool {
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

	// 工具级 trace 事件：hook 由 ctx 传入（见 trace.go withTrace/emitTrace）。
	emitTrace(ctx, TraceEvent{
		Kind:      TraceToolCall,
		ToolName:  name,
		Arguments: argumentsInJSON,
		Result:    out,
		Err:       err,
	})

	emitEvent(ctx, Event{
		Type:      EventToolEnd,
		ToolName:  name,
		Arguments: argumentsInJSON,
		Result:    out,
		Err:       err,
	})
	return out, err
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
