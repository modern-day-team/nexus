package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// CircuitOptions 描述每个候选模型的独立熔断参数。
type CircuitOptions struct {
	// Threshold 为连续失败多少次后熔断。<=0 时取 3。
	Threshold int
	// Cooldown 为熔断后的冷却时长，期间跳过该模型。<=0 时取 30s。
	Cooldown time.Duration
}

func (o *CircuitOptions) normalize() *CircuitOptions {
	out := *o
	if out.Threshold <= 0 {
		out.Threshold = 3
	}
	if out.Cooldown <= 0 {
		out.Cooldown = 30 * time.Second
	}
	return &out
}

// FallbackOptions 描述 fallback 链的配置。
type FallbackOptions struct {
	// Models 为按优先级排列的候选模型（>=1 个，主模型在前）。
	Models []model.ToolCallingChatModel
	// Circuit 为每个模型的熔断参数，nil 时不熔断（仅顺序降级）。
	Circuit *CircuitOptions
}

// circuitState 为单个模型的熔断状态机：
//
//	closed --连续失败达 Threshold--> open --冷却结束--> half-open --成功--> closed
//	                                                    \--失败--> open（重新计时）
type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

type circuit struct {
	opts *CircuitOptions

	mu           sync.Mutex
	state        circuitState
	consecFails  int
	openedAt     time.Time
	halfOpenUsed bool // half-open 状态下只放行一次尝试
}

func newCircuit(opts *CircuitOptions) *circuit {
	if opts == nil {
		return nil
	}
	return &circuit{opts: opts.normalize()}
}

// allow 判断当前是否放行一次尝试；open 且冷却结束则转入 half-open 并放行一次。
func (c *circuit) allow() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case circuitClosed:
		return true
	case circuitHalfOpen:
		return !c.halfOpenUsed
	case circuitOpen:
		if time.Since(c.openedAt) >= c.opts.Cooldown {
			c.state = circuitHalfOpen
			c.halfOpenUsed = true
			return true
		}
		return false
	}
	return true
}

// onSuccess 记录一次成功：half-open 恢复 closed，closed 清零计数。
func (c *circuit) onSuccess() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = circuitClosed
	c.consecFails = 0
	c.halfOpenUsed = false
}

// onFailure 记录一次失败：half-open 立即重新 open；closed 达阈值转 open。
func (c *circuit) onFailure() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == circuitHalfOpen {
		c.state = circuitOpen
		c.openedAt = time.Now()
		return
	}
	c.consecFails++
	if c.consecFails >= c.opts.Threshold {
		c.state = circuitOpen
		c.openedAt = time.Now()
		c.consecFails = 0
	}
}

// isOpened 报告当前是否处于 open/half-open 状态（仅供观测）。
func (c *circuit) isOpened() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == circuitOpen || c.state == circuitHalfOpen
}

// FallbackModel 按顺序尝试多个模型：当前模型失败（仅限尚未产出任何分片的失败）时
// 切换到下一个；全部失败返回最后一个错误。
//
// 每个模型配独立熔断器：连续失败达阈值后进入 open 状态，冷却时间内直接跳过该模型，
// 冷却结束进入 half-open 放行一次尝试，成功则恢复 closed。
//
// 简化约定：备用模型成功后 Current 保持在备用下标，不自动回切主模型；
// 主模型需等自身熔断器 half-open 恢复后才会重新参与候选（若未熔断则始终首选）。
// WithTools 派生的新 FallbackModel 与原模型共享同一组熔断状态。
type FallbackModel struct {
	mu       sync.Mutex
	models   []model.ToolCallingChatModel
	circuits []*circuit
	current  int // 当前首选模型下标；成功后保持，不自动回切更优先的模型
}

// NewFallbackModel 创建 fallback 链；Models 为空时报错。
func NewFallbackModel(opts FallbackOptions) (*FallbackModel, error) {
	if len(opts.Models) == 0 {
		return nil, errors.New("llm: fallback 需要至少一个候选模型")
	}
	for i, m := range opts.Models {
		if m == nil {
			return nil, fmt.Errorf("llm: fallback 候选模型 %d 为 nil", i)
		}
	}
	m := &FallbackModel{models: opts.Models, current: 0}
	for range opts.Models {
		m.circuits = append(m.circuits, newCircuit(opts.Circuit))
	}
	return m, nil
}

// Current 返回当前首选模型下标（便于观测）。仅在下标内切换，不自动回切。
func (m *FallbackModel) Current() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// OpenCircuits 返回当前处于 open/half-open 状态的模型下标（便于观测）。
func (m *FallbackModel) OpenCircuits() []int {
	var out []int
	for i, c := range m.circuits {
		if c.isOpened() {
			out = append(out, i)
		}
	}
	return out
}

// WithTools 返回共享同一熔断状态的新 FallbackModel：对每个 inner 调用 WithTools，
// 任一 inner 不支持则透传错误。
func (m *FallbackModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.mu.Lock()
	start := m.current
	m.mu.Unlock()

	derived := make([]model.ToolCallingChatModel, len(m.models))
	for i, inner := range m.models {
		d, err := inner.WithTools(tools)
		if err != nil {
			return nil, fmt.Errorf("llm: fallback 候选模型 %d 绑定工具失败: %w", i, err)
		}
		derived[i] = d
	}
	nm := &FallbackModel{models: derived, circuits: m.circuits, current: start}
	return nm, nil
}

// Generate 阻塞式调用：从当前首选模型起按顺序尝试，失败（含被熔断跳过）则切换下一个。
func (m *FallbackModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	start := m.current
	m.mu.Unlock()

	var errs []error
	for i := 0; i < len(m.models); i++ {
		idx := (start + i) % len(m.models)
		if !m.circuits[idx].allow() {
			errs = append(errs, fmt.Errorf("模型 %d 熔断中，已跳过", idx))
			continue
		}
		reply, err := m.models[idx].Generate(ctx, input, opts...)
		if err == nil {
			m.circuits[idx].onSuccess()
			m.mu.Lock()
			m.current = idx
			m.mu.Unlock()
			return reply, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("llm: fallback 调用被取消: %w", ctx.Err())
		}
		m.circuits[idx].onFailure()
		errs = append(errs, fmt.Errorf("模型 %d 失败: %w", idx, err))
	}
	if len(errs) == 0 {
		// 理论不可达：候选 >=1 且每个模型要么放行要么记入 errs。
		errs = append(errs, errors.New("无可用候选模型"))
	}
	return nil, fmt.Errorf("llm: fallback 全部 %d 个模型调用失败: %w", len(errs), errors.Join(errs...))
}

// Stream 流式调用：仅在 Stream 调用本身返回错误（尚未产出任何分片）时切换模型；
// 流建立后出错不切换，交由上层处理（与 agent/retry.go 语义一致）。
func (m *FallbackModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	start := m.current
	m.mu.Unlock()

	var errs []error
	for i := 0; i < len(m.models); i++ {
		idx := (start + i) % len(m.models)
		if !m.circuits[idx].allow() {
			errs = append(errs, fmt.Errorf("模型 %d 熔断中，已跳过", idx))
			continue
		}
		stream, err := m.models[idx].Stream(ctx, input, opts...)
		if err == nil {
			m.circuits[idx].onSuccess()
			m.mu.Lock()
			m.current = idx
			m.mu.Unlock()
			return stream, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("llm: fallback 流式调用被取消: %w", ctx.Err())
		}
		m.circuits[idx].onFailure()
		errs = append(errs, fmt.Errorf("模型 %d 失败: %w", idx, err))
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("无可用候选模型"))
	}
	return nil, fmt.Errorf("llm: fallback 全部 %d 个模型流式调用失败: %w", len(errs), errors.Join(errs...))
}
