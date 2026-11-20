# delegation 示例

父 Agent 通过 `agent.NewDelegationTool` 把子 Agent 包装成工具（`ask_time_agent`），
演示父 Agent 委派子 Agent 查询当前时间。

## 依赖分支

本示例依赖 **`feat/delegation` 分支**中的 `agent.NewDelegationTool`（API 契约为
`agent.DelegationOptions{Name, Description, SubAgent *Agent}`，返回 `tool.InvokableTool`）。
该 API 尚未合入 `feat/examples` 分支，合入前本示例不能编译；请先合并/切换到
`feat/delegation` 分支后再运行。

## 运行

```bash
export NEXUS_API_KEY=sk-xxx          # 必填
export NEXUS_BASE_URL=...            # 可选，默认 https://api.deepseek.com/v1
export NEXUS_MODEL=...               # 可选，默认 deepseek-chat

go run ./examples/delegation
```
