package agent

import (
	"context"
	"errors"
	"sync"
)

// errEmptyConversationID 表示会话 ID 为空。
var errEmptyConversationID = errors.New("agent: 会话 ID 不能为空")

// MemoryStore 是基于进程内内存的 SessionStore 实现。
//
// 内部用 sync.Map 保存 conversationID -> *MemorySession 的映射；
// 取会话时经 per-id 双检锁保证并发下同一 id 返回同一个实例，
// 不同 id 之间天然隔离。无 TTL/持久化，进程退出即丢失。
type MemoryStore struct {
	// sessions 保存 conversationID -> *MemorySession。
	sessions sync.Map
	// locks 为每个 id 提供创建互斥，保证 per-id 单次创建。
	locks sync.Map // conversationID -> *sync.Mutex
}

// NewMemoryStore 创建一个内存会话管理器。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// Session 返回指定会话的 Session 实例（不存在时创建）。
// conversationID 为空报错。
func (m *MemoryStore) Session(_ context.Context, conversationID string) (Session, error) {
	if conversationID == "" {
		return nil, errEmptyConversationID
	}

	// 快路径：已存在直接返回。
	if s, ok := m.sessions.Load(conversationID); ok {
		return s.(*MemorySession), nil
	}

	// 慢路径：per-id 锁内双检，保证并发取同一 id 只创建一次、
	// 各 goroutine 拿到同一实例。
	lock, _ := m.locks.LoadOrStore(conversationID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	if s, ok := m.sessions.Load(conversationID); ok {
		return s.(*MemorySession), nil
	}
	s := NewMemorySession()
	m.sessions.Store(conversationID, s)
	return s, nil
}

// Delete 删除指定会话。会话不存在时为 no-op。
func (m *MemoryStore) Delete(_ context.Context, conversationID string) error {
	if conversationID == "" {
		return errEmptyConversationID
	}
	m.sessions.Delete(conversationID)
	m.locks.Delete(conversationID)
	return nil
}

// Close 释放资源。内存实现无后台资源，总是返回 nil。
func (m *MemoryStore) Close() error {
	return nil
}
