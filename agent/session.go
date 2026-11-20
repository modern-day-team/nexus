package agent

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Session 抽象一次会话的消息存储。
// 内核只依赖该接口，持久化（文件、数据库等）由上层自行实现。
type Session interface {
	// Messages 按时间顺序返回当前会话的全部消息。
	Messages(ctx context.Context) ([]*schema.Message, error)
	// Append 追加消息到会话末尾。
	Append(ctx context.Context, msgs ...*schema.Message) error
	// Clear 清空会话消息。
	Clear(ctx context.Context) error
}

// MemorySession 是基于内存的 Session 实现，并发安全。
type MemorySession struct {
	mu   sync.RWMutex
	msgs []*schema.Message
}

// NewMemorySession 创建一个空的内存会话。
func NewMemorySession() *MemorySession {
	return &MemorySession{}
}

// Messages 返回历史消息的副本。
func (s *MemorySession) Messages(context.Context) ([]*schema.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*schema.Message, len(s.msgs))
	copy(out, s.msgs)
	return out, nil
}

// Append 追加消息。
func (s *MemorySession) Append(_ context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.msgs = append(s.msgs, msgs...)
	return nil
}

// Clear 清空消息。
func (s *MemorySession) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.msgs = nil
	return nil
}
