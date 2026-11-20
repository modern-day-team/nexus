# 更新日志

## v0.2.0（未发布）

### 新增
- `agent.NewDelegationTool`：把子 Agent 包装为工具，父 Agent 经 tool calling 委派任务，子 Agent 自持会话（Pi agent 式 delegation）。
- `agent.TokenCompactor` / `agent.EstimateTokens`：按 token 预算压缩请求历史，截断点对齐 user 边界（Claude Code auto-compact 的近似版）。
- `agent.Compactor`（v0.1.5）：请求前历史压缩接口，默认实现 `TruncateCompactor`。
- `agent.RetryOptions`（v0.1.5）：模型调用指数退避重试，只重试未产出分片的失败。
- `agent.ToolApprover`（v0.1.5）：工具调用审批钩子，拒绝原因作为工具结果反馈给模型。
- `agent.FileSession`（v0.1.5）：JSONL 文件持久化会话，内存缓存 + 追加写透。
- `llm.NewDeepSeek`（v0.1.5）：DeepSeek V4.1-1017 专项工厂，`Thinking`/`ReasoningEffort`。
- `agent.Usage.CachedTokens`（v0.1.5）：事件流透出上下文缓存命中 token 数。
- `examples/`（v0.2.0）：echo 与 delegation 可运行示例。

### 变更
- `agent` 包内文件按职责拆分：`turn.go`（轮次串行化）、`observe.go`（工具包装器）、`events.go`（事件流）。公共 API 不变。
- `FileSession` 落盘格式由整体 JSON 改为 JSONL 追加写，与旧格式不兼容。

### 性能
- `FileSession`：追加 O(新增条数)（原为重写全历史 O(n²)），`Messages` 零磁盘读。
- 请求前缀字节级稳定，适配 DeepSeek 硬盘上下文缓存等 prompt cache。

## v0.1.4
- 初始公共版本：ReAct Agent 内核、MemorySession、流式事件流、工具调用可视化、OpenAI 兼容模型工厂。
