// delegation 示例：通过 NewDelegationTool 把子 Agent 包装成工具，演示父 Agent 委派子 Agent。
//
// 注意：该示例依赖 agent.NewDelegationTool（DelegationOptions{Name, Description, SubAgent}），
// 该 API 正在 feat/delegation 分支开发，合入前本示例暂不能编译。
//
// 运行前设置环境变量（同 echo 示例）：
//
//	export NEXUS_API_KEY=sk-xxx          # 必填
//	export NEXUS_BASE_URL=...            # 可选，默认 https://api.deepseek.com/v1
//	export NEXUS_MODEL=...               # 可选，默认 deepseek-chat
//
// 运行：
//
//	go run ./examples/delegation
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/modern-day-team/nexus/agent"
	"github.com/modern-day-team/nexus/llm"
)

// timeInput 是 time_now 工具的入参（无实际字段，仅用于满足 InferTool 签名）。
type timeInput struct{}

// main 构建父子两个 Agent，并演示父 Agent 委派子 Agent 查询时间。
func main() {
	ctx := context.Background()

	// 1. 从环境变量读取连接参数。
	baseURL := os.Getenv("NEXUS_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	modelName := os.Getenv("NEXUS_MODEL")
	if modelName == "" {
		modelName = "deepseek-chat"
	}

	// 2. 构建 ChatModel；APIKey 为空时提示后退出，不 panic。
	apiKey := os.Getenv("NEXUS_API_KEY")
	if apiKey == "" {
		fmt.Println("提示: 未设置 NEXUS_API_KEY 环境变量，无法调用模型。")
		fmt.Println("请先 export NEXUS_API_KEY=sk-xxx 后重试。")
		os.Exit(1)
	}
	cm, err := llm.NewOpenAI(ctx, llm.Config{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Name:    modelName,
	})
	if err != nil {
		fmt.Println("构建 ChatModel 失败:", err)
		os.Exit(1)
	}

	// 3. 子 Agent：时间助手，工具为本地时间查询。
	timeTool, err := utils.InferTool("time_now", "查询当前本地时间",
		func(_ context.Context, _ timeInput) (string, error) {
			return time.Now().Format(time.RFC3339), nil
		})
	if err != nil {
		fmt.Println("构建 time_now 工具失败:", err)
		os.Exit(1)
	}
	subAgent, err := agent.New(ctx, agent.Options{
		ChatModel:    cm,
		Tools:        []tool.BaseTool{timeTool},
		SystemPrompt: "你是时间助手",
	})
	if err != nil {
		fmt.Println("构建子 Agent 失败:", err)
		os.Exit(1)
	}

	// 4. 父 Agent：把子 Agent 包装成 ask_time_agent 工具传入。
	//    依赖 feat/delegation 分支的 agent.NewDelegationTool。
	askTimeTool := agent.NewDelegationTool(agent.DelegationOptions{
		Name:        "ask_time_agent",
		Description: "向时间助手子 Agent 提问以查询当前时间",
		SubAgent:    subAgent,
	})
	parentAgent, err := agent.New(ctx, agent.Options{
		ChatModel:    cm,
		Tools:        []tool.BaseTool{askTimeTool},
		SystemPrompt: "你是调度助手，需要时间信息时调用 ask_time_agent 工具委派时间助手查询",
	})
	if err != nil {
		fmt.Println("构建父 Agent 失败:", err)
		os.Exit(1)
	}

	// 5. 流式事件：观察父 Agent 委派子 Agent 查询时间的完整过程。
	events, err := parentAgent.ChatStreamEvents(ctx, "现在几点了？")
	if err != nil {
		fmt.Println("ChatStreamEvents 失败:", err)
		os.Exit(1)
	}
	defer events.Close() // 事件流必须消费完或 Close

	fmt.Println("=== 父 Agent 委派过程 ===")
	for {
		ev, err := events.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Println("接收事件失败:", err)
			break
		}
		switch ev.Type {
		case agent.EventToken:
			fmt.Print(ev.Content)
		case agent.EventReasoning:
			fmt.Printf("\n[思考] %s", ev.Content)
		case agent.EventToolStart:
			fmt.Printf("\n[委派开始] %s(%s)\n", ev.ToolName, ev.Arguments)
		case agent.EventToolEnd:
			if ev.Err != nil {
				fmt.Printf("[委派结束] %s 失败: %v\n", ev.ToolName, ev.Err)
			} else {
				fmt.Printf("[委派结束] %s => %s\n", ev.ToolName, ev.Result)
			}
		case agent.EventError:
			fmt.Printf("\n[错误] %v", ev.Err)
		case agent.EventDone:
			fmt.Println()
			if ev.Err != nil {
				fmt.Println("本轮出错:", ev.Err)
			}
			if ev.Usage != nil {
				fmt.Printf("=== 本轮用量: prompt=%d completion=%d total=%d cached=%d ===\n",
					ev.Usage.PromptTokens, ev.Usage.CompletionTokens,
					ev.Usage.TotalTokens, ev.Usage.CachedTokens)
			}
		}
	}
}
