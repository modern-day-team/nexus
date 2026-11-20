# Nexus

基于 [Eino](https://github.com/cloudwego/eino) 的通用 Agent 内核。默认编排形态为 ReAct Agent + 工具调用，模型层对接任意 OpenAI 兼容接口。

## 目录结构

```
.
├── agent/            # 内核：ReAct Agent 封装、多轮历史、流式对话
├── llm/              # 模型工厂：OpenAI 兼容 ChatModel（DeepSeek / Qwen / Ollama / vLLM 等）
├── internal/
│   ├── config/       # 环境变量配置
│   └── tools/        # 内置工具：current_time、http_fetch
├── cmd/nexus/        # CLI 交互入口
├── Makefile
└── go.mod
```

## 快速开始

```bash
# OpenAI
export NEXUS_API_KEY=sk-xxx
export NEXUS_MODEL=gpt-4o-mini

# DeepSeek（OpenAI 兼容）
export NEXUS_API_KEY=sk-xxx
export NEXUS_BASE_URL=https://api.deepseek.com/v1
export NEXUS_MODEL=deepseek-chat

# 本地 Ollama / vLLM
export NEXUS_API_KEY=sk-local
export NEXUS_BASE_URL=http://localhost:11434/v1
export NEXUS_MODEL=qwen2.5:7b

make run
```

CLI 会以 `[thinking]`、`[tool]` 前缀实时展示模型思考与工具调用过程。

Windows PowerShell：

```powershell
$env:NEXUS_API_KEY="sk-xxx"
go run ./cmd/nexus
```

## 配置项

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `NEXUS_API_KEY` | API Key，未设置时回退 `OPENAI_API_KEY` | 无（必填） |
| `NEXUS_BASE_URL` | OpenAI 兼容接口地址 | `https://api.openai.com/v1` |
| `NEXUS_MODEL` | 模型 ID | `gpt-4o-mini` |
| `NEXUS_SYSTEM_PROMPT` | 系统提示词 | 内置默认值 |
| `NEXUS_MAX_STEP` | ReAct 最大步数 | `12` |
| `NEXUS_TEMPERATURE` | 采样温度 | 模型默认 |
| `NEXUS_MAX_TOKENS` | 最大输出 token | 模型默认 |
| `NEXUS_TIMEOUT_SECONDS` | 单次模型请求超时（秒） | `120` |

## 作为库使用

```go
import (
	"github.com/modern-day-team/nexus/agent"
	"github.com/modern-day-team/nexus/llm"
)

cm, err := llm.NewOpenAI(ctx, llm.Config{
	APIKey:  os.Getenv("NEXUS_API_KEY"),
	BaseURL: "https://api.deepseek.com/v1",
	Name:    "deepseek-chat",
})

a, err := agent.New(ctx, agent.Options{
	ChatModel:    cm,
	Tools:        myTools, // []tool.BaseTool
	SystemPrompt: "你是一个助手",
	Session:      agent.NewMemorySession(), // 可省略；也可实现 agent.Session 接入文件/数据库
})

reply, err := a.Chat(ctx, "你好")            // 一次性返回
stream, err := a.ChatStream(ctx, "你好")     // 流式返回，需 Close

// 事件流：同时获得正文分片、思考过程与工具调用过程
events, err := a.ChatStreamEvents(ctx, "现在北京时间几点")
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
	}
}
events.Close()
```

自定义工具可直接使用 Eino 的 `tool/utils.InferTool` 从结构体推断参数 schema，再传入 `agent.Options.Tools`。实现 `tool.InvokableTool` 的工具会被自动包装并上报 `tool_start` / `tool_end` 事件。

## CLI 命令

| 命令 | 说明 |
| --- | --- |
| `/help` | 查看命令 |
| `/reset` | 清空会话历史 |
| `/history` | 查看历史消息条数 |
| `/exit` | 退出 |

## 开发

```bash
make build   # 编译到 bin/nexus
make vet     # 静态检查
make fmt     # 格式化
make test    # 运行测试
```
