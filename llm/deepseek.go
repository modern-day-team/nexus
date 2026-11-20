package llm

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/components/model"
	openai "github.com/cloudwego/eino-ext/components/model/openai"
)

// DeepSeek 默认连接参数。
const (
	// DefaultDeepSeekBaseURL 是 DeepSeek OpenAI 兼容接口地址。
	DefaultDeepSeekBaseURL = "https://api.deepseek.com/v1"
	// DefaultDeepSeekModel 是默认模型名（V4.1-Flash-1017 起以 deepseek-flash 提供）。
	DefaultDeepSeekModel = "deepseek-flash"
)

// DeepSeekConfig 描述 DeepSeek 模型的连接与推理参数。
// 针对 DeepSeek V4.1-1017（deepseek-flash / deepseek-v4-pro）专项适配：
//   - 硬盘级上下文缓存（Context Caching on Disk）按请求前缀自动命中，
//     内核保证多轮间前缀字节级稳定（系统提示词 → 只追加的历史），无需额外配置；
//   - 思考模式通过 thinking 参数开关，ReasoningEffort 控制推理力度；
//   - 命中缓存的 prompt token 通过 agent 事件流的 EventDone.Usage.CachedTokens 透出，
//     可据此监控缓存命中率。
type DeepSeekConfig struct {
	// APIKey 为 DeepSeek API 密钥。
	APIKey string
	// Model 为模型名，默认 deepseek-flash（V4.1-Flash-1017）。
	// deepseek-v4-pro 同样受支持。
	Model string
	// Thinking 是否启用思考模式。nil 时不传递该参数，使用服务端默认值。
	// 启用后模型返回 reasoning_content，agent 事件流会以 EventReasoning 透出。
	Thinking *bool
	// ReasoningEffort 为推理力度（low / medium / high），Thinking 启用时生效。
	ReasoningEffort string
	// Temperature 采样温度。
	Temperature *float32
	// MaxTokens 单次回复最大 token 数。
	MaxTokens *int
	// Timeout 请求超时。
	Timeout time.Duration
}

// NewDeepSeek 创建对接 DeepSeek 的 ChatModel。
func NewDeepSeek(ctx context.Context, cfg DeepSeekConfig) (model.ToolCallingChatModel, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("llm: APIKey 未设置（可设置 DEEPSEEK_API_KEY 环境变量）")
	}

	name := cfg.Model
	if name == "" {
		name = DefaultDeepSeekModel
	}

	openaiCfg := openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		BaseURL: DefaultDeepSeekBaseURL,
		Model:   name,
	}
	// thinking 参数：仅当调用方显式给出时透传，避免覆盖服务端默认行为。
	if cfg.Thinking != nil {
		mode := "disabled"
		if *cfg.Thinking {
			mode = "enabled"
		}
		openaiCfg.ExtraFields = map[string]any{
			"thinking": map[string]any{"type": mode},
		}
	}
	if cfg.ReasoningEffort != "" {
		openaiCfg.ReasoningEffort = openai.ReasoningEffortLevel(cfg.ReasoningEffort)
	}
	openaiCfg.Temperature = cfg.Temperature
	openaiCfg.MaxTokens = cfg.MaxTokens
	openaiCfg.Timeout = cfg.Timeout

	return openai.NewChatModel(ctx, &openaiCfg)
}
