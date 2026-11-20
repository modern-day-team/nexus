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
├── agent/    # 内核：ReAct 封装、Session 会话、流式事件
├── llm/      # 模型工厂：OpenAI 兼容 ChatModel（DeepSeek / Qwen / Ollama / vLLM 等）
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
