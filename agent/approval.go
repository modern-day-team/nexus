package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ToolApprover 在每次工具调用前被询问。返回 nil 表示放行；
// 返回错误表示拒绝，该错误会以文本形式作为工具结果反馈给模型
// （而不是中断本轮），模型可据此调整后续动作——
// 类似 Claude Code 的权限模式：用户拒绝后助手仍可继续对话。
type ToolApprover func(ctx context.Context, call schema.ToolCall) error

// deniedToolPrefix 是拒绝时写入工具结果的前缀，便于模型识别。
const deniedToolPrefix = "工具调用被用户拒绝"

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
		return fmt.Sprintf("%s: %v", deniedToolPrefix, err), nil
	}
	return t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
}

func toolName(ctx context.Context, t tool.InvokableTool) string {
	if info, err := t.Info(ctx); err == nil && info != nil {
		return info.Name
	}
	return ""
}

// applyApproval 为工具加上审批钩子。approver 为 nil 时原样返回。
func applyApproval(tools []tool.BaseTool, approver ToolApprover) []tool.BaseTool {
	if approver == nil {
		return tools
	}
	out := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		if invokable, ok := t.(tool.InvokableTool); ok {
			out[i] = &approvedTool{InvokableTool: invokable, approver: approver}
			continue
		}
		out[i] = t
	}
	return out
}
