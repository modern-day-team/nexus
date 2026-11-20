package agent

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// 本文件实现每轮用量回调钩子：在 Chat / ChatStream / ChatStreamEvents
// 的轮次结束后回调 UsageHook，让调用方无需消费事件流即可记录 token 用量。

// UsageHook 在每轮对话（Chat / ChatStream / ChatStreamEvents）结束后被调用，
// 携带本轮 token 用量（Usage 定义见 events.go）。模型未返回用量时不调用。
// 该钩子在轮次的 turn 结束前同步调用，实现应轻量（如仅写日志/指标）；
// 如需耗时操作请自行异步。钩子内部自行处理错误（如打日志），
// 钩子无返回值，其行为不影响本轮结果。为 nil 时零开销跳过。
type UsageHook func(ctx context.Context, usage *Usage)

// usageFromMeta 从模型响应元信息构造 Usage；无用量信息时返回 nil。
func usageFromMeta(meta *schema.ResponseMeta) *Usage {
	if meta == nil || meta.Usage == nil {
		return nil
	}
	return &Usage{
		PromptTokens:     meta.Usage.PromptTokens,
		CompletionTokens: meta.Usage.CompletionTokens,
		TotalTokens:      meta.Usage.TotalTokens,
		CachedTokens:     meta.Usage.PromptTokenDetails.CachedTokens,
	}
}

// notifyUsage 在有钩子且有用量时同步调用钩子。
func (a *Agent) notifyUsage(ctx context.Context, usage *Usage) {
	if a.usageHook == nil || usage == nil {
		return
	}
	a.usageHook(ctx, usage)
}
