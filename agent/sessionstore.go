package agent

import "context"

// SessionStore 按会话 ID 管理一组 Session，供多用户/多租户场景使用。
//
// 每个 conversationID 对应一个独立的会话空间：不同 ID 的历史互不可见，
// 同一 ID 的多次调用返回等价的存储（实现须保证读写一致）。
// 典型用法见 NewWithStore。
type SessionStore interface {
	// Session 返回指定会话的 Session 实例（不存在时创建）。
	// 同一 id 的多次调用应返回等价的存储（实现须保证读写一致）。
	Session(ctx context.Context, conversationID string) (Session, error)
	// Delete 删除指定会话。
	Delete(ctx context.Context, conversationID string) error
	// Close 释放底层资源（后台清理 goroutine、连接等）。
	Close() error
}
