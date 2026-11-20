// echo 示例：最小可运行的 Nexus Agent。
//
// 运行前设置环境变量：
//
//	export NEXUS_API_KEY=sk-xxx          # 必填
//	export NEXUS_BASE_URL=...            # 可选，默认 https://api.deepseek.com/v1
//	export NEXUS_MODEL=...               # 可选，默认 deepseek-chat
//
// 运行：
//
//	go run ./examples/echo
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/modern-day-team/nexus/agent"
	"github.com/modern-day-team/nexus/llm"
)

// echoInput 是 echo 工具的入参。
type echoInput struct {
	Text string `json:"text" jsonschema:"required" jsonschema_description:"要回显的文本"`
}

// main 演示构建模型与 Agent、一次性对话与流式事件消费。
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

	// 2. 构建 OpenAI 兼容 ChatModel；APIKey 为空时提示后退出，不 panic。
	apiKey := os.Getenv("NEXUS_API_KEY")
	if apiKey == "" {
		fmt.Println("提示: 未设置 NEXUS_API_KEY 环境变量，无法调用模型。")
		fmt.Println("请先 export NEXUS_API_KEY=sk-xxx（本地服务可设置 NEXUS_API_KEY=sk-local）后重试。")
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

	// 3. 注册本地回显工具：入参 {"text": string}，返回 "echo: <text>"。
	echoTool, err := utils.InferTool("echo", "回显输入文本",
		func(_ context.Context, in echoInput) (string, error) {
			return "echo: " + in.Text, nil
		})
	if err != nil {
		fmt.Println("构建 echo 工具失败:", err)
		os.Exit(1)
	}
	tools := []tool.BaseTool{echoTool}

	// 4. 构建 Agent 与内存会话。
	a, err := agent.New(ctx, agent.Options{
		ChatModel:    cm,
		Tools:        tools,
		SystemPrompt: "你是一个简洁的助手",
		Session:      agent.NewMemorySession(),
	})
	if err != nil {
		fmt.Println("构建 Agent 失败:", err)
		os.Exit(1)
	}

	// 5. 一次性对话。
	reply, err := a.Chat(ctx, "用一句话介绍你自己")
	if err != nil {
		fmt.Println("Chat 失败:", err)
		os.Exit(1)
	}
	fmt.Println("=== Chat 回复 ===")
	fmt.Println(reply.Content)

	// 6. 流式事件：正文、思考过程、工具调用与结束事件。
	events, err := a.ChatStreamEvents(ctx, "调用 echo 工具回显一句“你好，Nexus”")
	if err != nil {
		fmt.Println("ChatStreamEvents 失败:", err)
		os.Exit(1)
	}
	defer events.Close() // 事件流必须消费完或 Close

	fmt.Println("=== 流式事件 ===")
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
			fmt.Printf("\n[工具开始] %s(%s)\n", ev.ToolName, ev.Arguments)
		case agent.EventToolEnd:
			if ev.Err != nil {
				fmt.Printf("[工具结束] %s 失败: %v\n", ev.ToolName, ev.Err)
			} else {
				fmt.Printf("[工具结束] %s => %s\n", ev.ToolName, ev.Result)
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
