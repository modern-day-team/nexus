package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// 本文件实现委派工具的流式事件透传：
// 与 NewDelegationTool 相同的工具形态，但执行时驱动子 Agent 的
// ChatStreamEvents，并把子事件经父 ctx 上的 emitter 透传到父事件流。

// NewDelegationStreamTool 的行为与 NewDelegationTool 相同（入参 {"input": string}），
// 但执行时通过子 Agent 的 ChatStreamEvents 驱动，并把子 Agent 的全部事件
// 经 emitEvent 透传到父 Agent 的事件流（Event.Agent = 工具名）。
// 最终工具结果 = 子 Agent 最终正文（EventToken 拼接）。
// 若父 ctx 上没有 emitter（如父用 Chat 而非 ChatStreamEvents），事件透传为 no-op，
// 行为退化为普通委派。
//
// 嵌套委派说明：子事件经父 emitter 透传时 Agent 字段直接覆盖为本次工具名，
// 即事件归属到最内层委派工具；不做多层前缀拼接的复杂处理。
func NewDelegationStreamTool(opts DelegationOptions) (tool.InvokableTool, error) {
	if err := validateDelegationOptions(opts); err != nil {
		return nil, err
	}
	return toolutils.InferTool(opts.Name, opts.Description, runDelegationStream(opts.SubAgent, opts.Name))
}

// runDelegationStream 返回流式透传委派工具的执行函数。
func runDelegationStream(sub *Agent, name string) func(context.Context, delegationInput) (string, error) {
	return func(ctx context.Context, in delegationInput) (string, error) {
		if in.Input == "" {
			return "", errors.New("agent: 委派工具入参 input 不能为空")
		}

		// 子 Agent 自行串行化轮次（turn gate），历史跨多次委派保留。
		stream, err := sub.ChatStreamEvents(ctx, in.Input)
		if err != nil {
			return "", fmt.Errorf("agent: 子 Agent 委派失败: %w", err)
		}
		defer stream.Close()

		var content string
		for {
			ev, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("agent: 子 Agent 委派失败: %w", err)
			}

			// 每个事件先透传到父事件流（含 EventError 与 EventDone，
			// 保持事件契约一致），Agent 字段覆盖为本次委派工具名；
			// 父 ctx 上没有 emitter 时为 no-op。
			emitEvent(ctx, Event{
				Type:      ev.Type,
				Content:   ev.Content,
				ToolName:  ev.ToolName,
				Arguments: ev.Arguments,
				Result:    ev.Result,
				Usage:     ev.Usage,
				Err:       ev.Err,
				Agent:     name,
			})

			switch ev.Type {
			case EventToken:
				content += ev.Content
			case EventDone:
				if ev.Err != nil {
					return "", fmt.Errorf("agent: 子 Agent 委派失败: %w", ev.Err)
				}
				return content, nil
			}
		}
		return content, nil
	}
}
