// Package config 从环境变量加载 Agent 内核配置。
package config

import (
	"os"
	"strconv"
	"time"

	"github.com/modern-day-team/nexus/agent"
)

const (
	envPrefix         = "NEXUS_"
	defaultBaseURL    = "https://api.openai.com/v1"
	defaultModel      = "gpt-4o-mini"
	defaultTimeout    = 120 * time.Second
	defaultSystemText = "你是 Nexus，一个基于 Eino 构建的通用 Agent 内核。你可以调用工具来获取实时信息或执行操作。" +
		"无法确定的事实时优先使用工具，不要编造。回答使用与用户相同的语言，保持简洁准确。"
)

// Config 是内核的运行配置。
type Config struct {
	APIKey       string
	BaseURL      string
	ModelName    string
	SystemPrompt string
	MaxStep      int
	Temperature  *float32
	MaxTokens    *int
	Timeout      time.Duration
}

// Load 从环境变量读取配置，未设置的项使用默认值。
func Load() Config {
	cfg := Config{
		APIKey:       firstNonEmpty(env("API_KEY"), os.Getenv("OPENAI_API_KEY")),
		BaseURL:      firstNonEmpty(env("BASE_URL"), defaultBaseURL),
		ModelName:    firstNonEmpty(env("MODEL"), defaultModel),
		SystemPrompt: firstNonEmpty(env("SYSTEM_PROMPT"), defaultSystemText),
		MaxStep:      agent.DefaultMaxStep,
		Timeout:      defaultTimeout,
	}

	if v, ok := lookupInt("MAX_STEP"); ok && v > 0 {
		cfg.MaxStep = v
	}
	if v, ok := lookupFloat("TEMPERATURE"); ok {
		cfg.Temperature = &v
	}
	if v, ok := lookupInt("MAX_TOKENS"); ok && v > 0 {
		cfg.MaxTokens = &v
	}
	if v, ok := lookupInt("TIMEOUT_SECONDS"); ok && v > 0 {
		cfg.Timeout = time.Duration(v) * time.Second
	}

	return cfg
}

func env(key string) string {
	return os.Getenv(envPrefix + key)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func lookupInt(key string) (int, bool) {
	raw := env(key)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

func lookupFloat(key string) (float32, bool) {
	raw := env(key)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return 0, false
	}
	return float32(v), true
}
