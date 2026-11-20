package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// 本文件实现子 Agent 委派（delegation）：
// 把一个子 Agent 包装成一个工具，父 Agent 通过 tool calling 调用它，
// 类似 Pi agent 风格的 delegation。

// DelegationOptions 描述委派工具的构建参数。
type DelegationOptions struct {
	// Name 工具名（必填，作为父 Agent 可见的工具名）。
	Name string
	// Description 工具描述（必填，父 Agent 据此决定何时委派）。
	Description string
	// SubAgent 是被委派的子 Agent（自带独立 Session 与工具集）。必填。
	SubAgent *Agent
}

// delegationInput 是委派工具的入参 schema。
type delegationInput struct {
	Input string `json:"input" jsonschema:"required" jsonschema_description:"委派给子 Agent 的任务描述"`
}

// NewDelegationTool 把子 Agent 包装为可被父 Agent 调用的 InvokableTool。
// 工具入参 schema 为 {"input": string}（required），即要传给子 Agent 的任务描述。
// 子 Agent 的会话历史跨多次委派保留（子 Agent 有记忆）。
func NewDelegationTool(opts DelegationOptions) (tool.InvokableTool, error) {
	if opts.Name == "" {
		return nil, errors.New("agent: 委派工具 Name 不能为空")
	}
	if opts.Description == "" {
		return nil, errors.New("agent: 委派工具 Description 不能为空")
	}
	if opts.SubAgent == nil {
		return nil, errors.New("agent: 委派工具 SubAgent 不能为空")
	}
	return toolutils.InferTool(opts.Name, opts.Description, runDelegation(opts.SubAgent))
}

// runDelegation 返回委派工具的执行函数。
func runDelegation(sub *Agent) func(context.Context, delegationInput) (string, error) {
	return func(ctx context.Context, in delegationInput) (string, error) {
		if in.Input == "" {
			return "", errors.New("agent: 委派工具入参 input 不能为空")
		}
		// 子 Agent 自行串行化轮次（turn gate），历史跨多次委派保留。
		reply, err := sub.Chat(ctx, in.Input)
		if err != nil {
			return "", fmt.Errorf("agent: 子 Agent 委派失败: %w", err)
		}
		return reply.Content, nil
	}
}