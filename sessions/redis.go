// Package sessions 提供跨进程可恢复的 agent.Session 参考实现。
// 当前包含基于 Redis 与 SQLite 的两种持久化 Session，适用于
// 多实例部署或进程重启后需要恢复会话历史的场景。
package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/cloudwego/eino/schema"
)

// redisKeyPrefix 是 Redis 键的统一前缀，避免与其他业务键冲突。
const redisKeyPrefix = "nexus:session:"

// RedisSession 把会话消息存为 Redis List，跨进程共享、可恢复。
//
// 每条消息序列化为一个 JSON 字符串 RPUSH 到 List 尾部；
// Messages 用 LRANGE 0 -1 按写入顺序读回；Clear 用 DEL 删除整个键。
// ttl 大于 0 时，每次 Append 后刷新键的过期时间；为 0 表示永不过期。
//
// Redis 自身命令是原子的，因此无需额外锁即可跨进程并发访问
// 同一个 key（消息整体顺序由 RPUSH 的执行顺序决定）。
type RedisSession struct {
	client *redis.Client
	key    string
	ttl    time.Duration
}

// NewRedisSession 创建一个 Redis 会话。
// key 是会话的业务标识（会自动加上 "nexus:session:" 前缀）；
// ttl 为过期时间，<=0 表示不过期。
func NewRedisSession(client *redis.Client, key string, ttl time.Duration) *RedisSession {
	return &RedisSession{
		client: client,
		key:    redisKeyPrefix + key,
		ttl:    ttl,
	}
}

// Messages 按写入顺序返回全部历史消息。
// 键不存在时（新会话或已过期/清空）返回空切片而非错误。
func (s *RedisSession) Messages(ctx context.Context) ([]*schema.Message, error) {
	vals, err := s.client.LRange(ctx, s.key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("sessions: 读取 Redis 会话失败: %w", err)
	}

	msgs := make([]*schema.Message, 0, len(vals))
	for _, v := range vals {
		var msg schema.Message
		if err := json.Unmarshal([]byte(v), &msg); err != nil {
			return nil, fmt.Errorf("sessions: 解析 Redis 会话消息失败: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	return msgs, nil
}

// Append 把消息逐条序列化为 JSON 并 RPUSH 到 List 尾部。
// 空参为 no-op。ttl 大于 0 时同步刷新过期时间。
func (s *RedisSession) Append(ctx context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	vals := make([]any, 0, len(msgs))
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("sessions: 序列化会话消息失败: %w", err)
		}
		vals = append(vals, string(data))
	}

	if err := s.client.RPush(ctx, s.key, vals...).Err(); err != nil {
		return fmt.Errorf("sessions: 追加 Redis 会话消息失败: %w", err)
	}
	if s.ttl > 0 {
		if err := s.client.Expire(ctx, s.key, s.ttl).Err(); err != nil {
			return fmt.Errorf("sessions: 刷新 Redis 会话过期时间失败: %w", err)
		}
	}
	return nil
}

// Clear 删除整个 List，清空会话消息。
func (s *RedisSession) Clear(ctx context.Context) error {
	if err := s.client.Del(ctx, s.key).Err(); err != nil {
		return fmt.Errorf("sessions: 清空 Redis 会话失败: %w", err)
	}
	return nil
}
