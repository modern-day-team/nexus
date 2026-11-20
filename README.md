# Nexus

基于 [Eino](https://github.com/cloudwego/eino) 的通用 Agent 内核（Go 库）。默认编排形态为 ReAct Agent + 工具调用，模型层对接任意 OpenAI 兼容接口。

定位：小而可嵌入的内核，只提供运行循环、模型/工具抽象、会话存储与事件流

## 安装

```bash
go get github.com/modern-day-team/nexus
```

## 目录结构

```
.
├── agent/    # 内核：ReAct 封装、Session 会话、流式事件、委派、重试、审批、压缩
├── llm/      # 模型工厂：OpenAI 兼容 ChatModel（DeepSeek / Qwen / Ollama / vLLM 等）
├── examples/ # 可运行示例：echo、delegation
├── CHANGELOG.md
├── Makefile
└── go.mod
```

## 快速开始

```go
import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/modern-day-team/nexus/agent"
	"github.com/modern-day-team/nexus/llm"
)

func main() {
	ctx := context.Background()

	cm, err := llm.NewOpenAI(ctx, llm.Config{
		APIKey:  os.Getenv("NEXUS_API_KEY"),
		BaseURL: "https://api.deepseek.com/v1", // 任意 OpenAI 兼容地址
		Name:    "deepseek-chat",
	})
	if err != nil {
		panic(err)
	}

	// DeepSeek 专项工厂：思考模式、推理力度、上下文缓存命中率（Usage.CachedTokens）
	// ds, err := llm.NewDeepSeek(ctx, llm.DeepSeekConfig{
	// 	APIKey:          os.Getenv("DEEPSEEK_API_KEY"),
	// 	Thinking:        ptr(true),          // 启用思考模式
	// 	ReasoningEffort: "high",
	// })

	a, err := agent.New(ctx, agent.Options{
		ChatModel:    cm,
		Tools:        myTools, // []tool.BaseTool，可为空
		SystemPrompt: "你是一个助手",
		Session:      agent.NewMemorySession(), // 可省略；持久化可用 agent.NewFileSession("chat.json")（内存缓存 + JSONL 追加写透），或自行实现 agent.Session
	})
	if err != nil {
		panic(err)
	}

	// 一次性返回
	reply, err := a.Chat(ctx, "你好")

	// 流式返回，必须消费完或 Close
	stream, err := a.ChatStream(ctx, "你好")
	defer stream.Close()

	// 事件流：正文、思考过程、工具调用过程
	events, err := a.ChatStreamEvents(ctx, "现在北京时间几点")
	defer events.Close()
	for {
		ev, err := events.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		switch ev.Type {
		case agent.EventToken:     // ev.Content 正文分片
		case agent.EventReasoning: // ev.Content 思考分片
		case agent.EventToolStart: // ev.ToolName、ev.Arguments
		case agent.EventToolEnd:   // ev.Result、ev.Err
		case agent.EventError:     // ev.Err
		case agent.EventDone:      // 本轮结束：历史已落库，ev.Usage、ev.Err
		}
	}
}
```

## DeepSeek V4.1-1017 专项适配

- **工厂**：`llm.NewDeepSeek`，默认模型 `deepseek-flash`（V4.1-Flash-1017），支持 `Thinking`（思考模式）与 `ReasoningEffort`（推理力度 low/medium/high）。
- **上下文缓存**：DeepSeek 的硬盘级上下文缓存按请求前缀自动命中。内核保证多轮间消息前缀字节级稳定（系统提示词 → 只追加的历史），无需额外配置即可命中；不要在内核外改写历史消息。
- **命中率监控**：`ChatStreamEvents` 的 `EventDone.Usage.CachedTokens` 透出本轮命中缓存的 prompt token 数，`CachedTokens / PromptTokens` 即缓存命中率。

## 内核增强选项（借鉴 Codex / Claude Code）

以下均为 `agent.Options` 可选项，默认关闭：

- **历史压缩**（对应 Claude Code auto-compact）：`Compactor` 在每次请求前压缩历史，不回写会话。默认实现 `agent.NewTruncateCompactor(maxMessages, keepHead)`：保留开头固定上下文与最近消息，截断点对齐 user 边界以免切开工具调用对，并插入截断说明。
- **模型重试**（对应 Codex 的 transient error retry）：`Retry: &agent.RetryOptions{MaxAttempts: 3, BaseDelay: 500ms, MaxDelay: 10s}`。指数退避 + 抖动，只重试未产出任何分片的失败，流建立后中断不重试。
- **工具审批**（对应 Claude Code permission mode）：`ToolApprover` 在每次工具调用前被询问；返回错误即拒绝，拒绝原因作为工具结果反馈给模型（本轮不中断），模型可据此调整后续动作。
- **token 压缩器**：`agent.NewTokenCompactor(maxTokens, keepHeadTokens)`，按估算 token 预算截断历史（`agent.EstimateTokens`），同样对齐 user 边界；与 `TruncateCompactor` 二选一。

## 多 Agent 委派

把子 Agent 包装成一个工具交给父 Agent，子 Agent 自持独立会话与工具集（历史跨多次委派保留）：

```go
sub, _ := agent.New(ctx, agent.Options{ChatModel: cm, Tools: timeTools})
askTime, _ := agent.NewDelegationTool(agent.DelegationOptions{
	Name:        "ask_time_agent",
	Description: "向时间助手子 Agent 提问",
	SubAgent:    sub,
})
parent, _ := agent.New(ctx, agent.Options{ChatModel: cm, Tools: []tool.BaseTool{askTime}})
```

## 示例

[examples/](examples/) 下有两个可运行示例：`echo`（最小对话 + 本地工具 + 事件流）与 `delegation`（父 Agent 委派时间助手子 Agent），运行方式见 [examples/README.md](examples/README.md)。

## 事件契约

- `EventDone` 总是最后一个事件，到达时本轮历史已写入，可安全发起下一轮。
- 出错时 `EventError` 先到，`EventDone.Err` 携带同一错误。
- 同一个 `Agent` 实例上的轮次串行执行；流式调用必须消费完或 `Close`，否则后续轮次会被阻塞。
- `Reset` 会等待进行中的轮次结束后再清空。

## 自定义工具

使用 Eino 的 `tool/utils.InferTool` 从结构体推断参数 schema，再传入 `agent.Options.Tools`：

```go
type echoInput struct {
	Text string `json:"text" jsonschema:"required" jsonschema_description:"要回显的文本"`
}

echo, err := toolutils.InferTool("echo", "回显输入文本",
	func(_ context.Context, in echoInput) (string, error) { return in.Text, nil })
```

实现 `tool.InvokableTool` 的工具会被自动包装并上报 `tool_start` / `tool_end` 事件。

## 开发

```bash
make fmt    # 格式化
make vet    # 静态检查
make test   # 运行测试
make race   # 竞态检测
```
