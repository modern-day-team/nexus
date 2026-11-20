package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// RetryOptions 描述模型调用的重试策略。
// 只重试"尚未产生任何输出"的失败（建立连接、限流等瞬时错误）；
// 流式一旦开始接收分片就不再重试，避免重复输出。
type RetryOptions struct {
	// MaxAttempts 为最大尝试次数（含首次）。<=1 时不重试。
	MaxAttempts int
	// BaseDelay 为首次重试的等待时间，按指数退避翻倍。<=0 时取 500ms。
	BaseDelay time.Duration
	// MaxDelay 为单次等待上限。<=0 时取 10s。
	MaxDelay time.Duration
}

func (o *RetryOptions) normalize() *RetryOptions {
	out := *o
	if out.MaxAttempts < 1 {
		out.MaxAttempts = 1
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = 500 * time.Millisecond
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = 10 * time.Second
	}
	return &out
}

// retryingModel 为模型调用加上指数退避重试。
type retryingModel struct {
	inner model.ToolCallingChatModel
	opts  *RetryOptions
}

// newRetryingModel 包装模型；opts 为 nil 或 MaxAttempts<=1 时原样返回。
func newRetryingModel(inner model.ToolCallingChatModel, opts *RetryOptions) model.ToolCallingChatModel {
	if inner == nil || opts == nil || opts.MaxAttempts <= 1 {
		return inner
	}
	return &retryingModel{inner: inner, opts: opts.normalize()}
}

// WithTools 委托内层模型，并保证返回的派生模型同样具备重试能力。
func (m *retryingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	derived, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &retryingModel{inner: derived, opts: m.opts}, nil
}

// Generate 阻塞式调用，失败时按退避策略重试。
func (m *retryingModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var lastErr error
	for attempt := 0; attempt < m.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, m.opts, attempt); err != nil {
				return nil, err
			}
		}
		reply, err := m.inner.Generate(ctx, input, opts...)
		if err == nil {
			return reply, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, fmt.Errorf("agent: 模型调用重试 %d 次后仍失败: %w", m.opts.MaxAttempts, lastErr)
}

// Stream 流式调用。仅在尚未产出任何分片（Stream 调用本身报错）时重试；
// 流一旦建立，中途错误直接返回，交由上层处理。
func (m *retryingModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	var lastErr error
	for attempt := 0; attempt < m.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, m.opts, attempt); err != nil {
				return nil, err
			}
		}
		stream, err := m.inner.Stream(ctx, input, opts...)
		if err == nil {
			return stream, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, fmt.Errorf("agent: 模型流式调用重试 %d 次后仍失败: %w", m.opts.MaxAttempts, lastErr)
}

// sleepBackoff 按尝试序号计算指数退避等待（带抖动），等待期间响应取消。
func sleepBackoff(ctx context.Context, opts *RetryOptions, attempt int) error {
	delay := opts.BaseDelay << (attempt - 1)
	if delay > opts.MaxDelay || delay <= 0 {
		delay = opts.MaxDelay
	}
	// ±20% 抖动，避免并发轮次同步重试。
	jitter := time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
	if jitter <= 0 {
		jitter = delay
	}

	timer := time.NewTimer(jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("agent: 重试等待被取消: %w", ctx.Err())
	}
}
