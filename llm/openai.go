// Package llm 提供 Agent 内核使用的模型工厂。
package llm

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/components/model"
	openai "github.com/cloudwego/eino-ext/components/model/openai"
)

// Config 描述一个 OpenAI 兼容的 ChatModel 连接参数。
type Config struct {
	APIKey      string
	BaseURL     string
	Name        string
	Temperature *float32
	MaxTokens   *int
	Timeout     time.Duration
}

// NewOpenAI 创建 OpenAI 兼容的 ChatModel，支持 DeepSeek、Qwen、Ollama、vLLM 等服务。
func NewOpenAI(ctx context.Context, cfg Config) (model.ToolCallingChatModel, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("llm: APIKey 未设置（本地服务可设置 NEXUS_API_KEY=sk-local）")
	}
	if cfg.Name == "" {
		return nil, errors.New("llm: 模型名称未设置")
	}

	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:      cfg.APIKey,
		BaseURL:     cfg.BaseURL,
		Model:       cfg.Name,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
		Timeout:     cfg.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return cm, nil
}
