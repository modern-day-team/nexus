# 更新日志

## v0.7.0
- `agent.GenerateInto`：结构化输出——要求模型返回 JSON 并解析到任意 Go 类型，解析失败自动把错误反馈给模型重试（最多 3 轮，中间轮不落库），成功后与 Chat 同样的历史/UsageHook/Trace 语义。
- `agent.ExtractJSON` / `agent.ErrNoJSON`：从模型输出中剥离 markdown 围栏与杂文本、提取 JSON 子串的导出工具。

## v0.6.0
- `llm.NewFallbackModel`：多模型顺序降级链，每个候选独立熔断器（closed→open→half-open，连续失败达阈值熔断、冷却后半开恢复）；`Current()`/`OpenCircuits()` 观测；`WithTools` 派生共享熔断状态；流建立后不切换模型。

## v0.5.0
- `agent.Options.TraceHook`：内核追踪钩子（轮次开始/结束、工具调用），纯标准库零依赖。
- `otelx` 包：OpenTelemetry GenAI 语义约定映射（`gen_ai.*.agent.run` / `execute_tool` span、token 用量与错误状态），仅依赖 otel API。

## v0.4.0
- `sessions` 包：`RedisSession`（go-redis，TTL 可选）与 `SQLiteSession`（modernc.org/sqlite 纯 Go，WAL+busy_timeout）两种 `agent.Session` 参考实现，跨进程恢复会话；测试用 miniredis，无需真实外部服务。

## v0.3.0
- `agent.NewDelegationStreamTool`：委派工具的流式事件透传——子 Agent 的 token/思考/工具事件经父事件流透出，`Event.Agent` 标识来源；父 ctx 无 emitter 时退化为普通委派。

## v0.2.0

### 新增
- `agent.NewDelegationTool`：把子 Agent 包装为工具，父 Agent 经 tool calling 委派任务，子 Agent 自持会话（Pi agent 式 delegation）。
- `agent.TokenCompactor` / `agent.EstimateTokens`：按 token 预算压缩请求历史，截断点对齐 user 边界（Claude Code auto-compact 的近似版）。
- `agent.UsageHook`：每轮对话（Chat/ChatStream/ChatStreamEvents）结束后的用量回调，无需消费事件流即可记录 token 与缓存命中率。
- CI：GitHub Actions（gofmt 检查、vet、`-race` 测试）与 Makefile `check`/`cover`/`ci` 目标。
- 属性测试：压缩器与委派工具的随机化不变量测试（工具对完整性、条数/token 预算、头部保留一致性）。
- `examples/`：echo 与 delegation 可运行示例。
- `agent.Compactor`（v0.1.5）：请求前历史压缩接口，默认实现 `TruncateCompactor`。
- `agent.RetryOptions`（v0.1.5）：模型调用指数退避重试，只重试未产出分片的失败。
- `agent.ToolApprover`（v0.1.5）：工具调用审批钩子，拒绝原因作为工具结果反馈给模型。
- `agent.FileSession`（v0.1.5）：JSONL 文件持久化会话，内存缓存 + 追加写透。
- `llm.NewDeepSeek`（v0.1.5）：DeepSeek V4.1-1017 专项工厂，`Thinking`/`ReasoningEffort`。
- `agent.Usage.CachedTokens`（v0.1.5）：事件流透出上下文缓存命中 token 数。

### 修复
- 压缩器头部保留区可能切开 assistant 工具调用与 tool 响应对（两个压缩器均受影响）。
- 压缩器尾段窗口无 user 边界时向前回退导致消息重复保留、条数/token 预算失控；现改为丢弃整个尾段窗口。

### 变更
- `agent` 包内文件按职责拆分：`turn.go`（轮次串行化）、`observe.go`（工具包装器）、`events.go`（事件流）。公共 API 不变。
- `FileSession` 落盘格式由整体 JSON 改为 JSONL 追加写，与旧格式不兼容。

### 性能
- `FileSession`：追加 O(新增条数)（原为重写全历史 O(n²)），`Messages` 零磁盘读。
- 请求前缀字节级稳定，适配 DeepSeek 硬盘上下文缓存等 prompt cache。

## v0.1.4
- 初始公共版本：ReAct Agent 内核、MemorySession、流式事件流、工具调用可视化、OpenAI 兼容模型工厂。
