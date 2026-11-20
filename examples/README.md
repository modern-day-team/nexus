# examples

Nexus 可运行示例。均为本 module 内的独立 main 包。

## 公共环境变量

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `NEXUS_API_KEY` | 是 | 无 | API 密钥；本地服务可设 `NEXUS_API_KEY=sk-local` |
| `NEXUS_BASE_URL` | 否 | `https://api.deepseek.com/v1` | 任意 OpenAI 兼容接口地址 |
| `NEXUS_MODEL` | 否 | `deepseek-chat` | 模型名 |

## echo —— 最小示例

构建 ChatModel 与 Agent（SystemPrompt "你是一个简洁的助手"），演示一次性 `Chat`
与 `ChatStreamEvents` 事件流（正文/思考/工具调用/结束用量），并注册本地回显工具 `echo`。

```bash
cd examples 仓根目录
export NEXUS_API_KEY=sk-xxx
go run ./examples/echo
```

## delegation —— 子 Agent 委派示例

子 Agent（时间助手，工具 `time_now`）被 `agent.NewDelegationTool` 包装成
`ask_time_agent` 工具供父 Agent 调用，演示父 Agent 委派子 Agent 查询时间。

注意：依赖 `feat/delegation` 分支的 `agent.NewDelegationTool`，详见
[delegation/README.md](delegation/README.md)。

```bash
export NEXUS_API_KEY=sk-xxx
go run ./examples/delegation
```
