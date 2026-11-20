package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"

	"github.com/modern-day-team/nexus/agent"
)

// 编译期断言：本包实现 agent.SessionStore。
var (
	_ agent.SessionStore = (*RedisStore)(nil)
	_ agent.SessionStore = (*SQLiteStore)(nil)
)

// errEmptyConversationID 表示会话 ID 为空。
var errEmptyConversationID = errors.New("sessions: 会话 ID 不能为空")

// validateConversationID 校验会话 ID：
// 非空、长度不超过 256、不含空白字符与控制字符。
// 该校验同时保证 Redis 键与（经转义后的）SQLite 查询参数安全。
func validateConversationID(id string) error {
	if id == "" {
		return errEmptyConversationID
	}
	if len(id) > 256 {
		return fmt.Errorf("sessions: 会话 ID 过长: %d 字节", len(id))
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("sessions: 会话 ID 含空白或控制字符 %q", id)
		}
	}
	return nil
}

// RedisStore 按会话 ID 管理一组 RedisSession。
//
// 内部为每个 id 缓存一个 *RedisSession（复用 RedisSession 现有实现，
// key = "nexus:session:" + id），per-id 互斥锁双检保证并发取同一 id
// 返回同一实例。TTL 过期直接复用 RedisSession 的 Expire 机制，
// Redis 自身负责淘汰，无需后台清理 goroutine。
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration

	mu       sync.Mutex
	sessions map[string]*RedisSession
}

// NewRedisStore 创建一个 Redis 会话管理器。
// ttl 透传给底层 RedisSession：>0 时每次 Append 刷新过期时间，<=0 不过期。
func NewRedisStore(client *redis.Client, ttl time.Duration) *RedisStore {
	return &RedisStore{
		client:   client,
		ttl:      ttl,
		sessions: make(map[string]*RedisSession),
	}
}

// Session 返回指定会话的 Session 实例（不存在时创建）。
func (s *RedisStore) Session(_ context.Context, conversationID string) (agent.Session, error) {
	if err := validateConversationID(conversationID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[conversationID]; ok {
		return sess, nil
	}
	sess := NewRedisSession(s.client, conversationID, s.ttl)
	s.sessions[conversationID] = sess
	return sess, nil
}

// Delete 删除指定会话（DEL 整个键）并移除缓存实例。
func (s *RedisStore) Delete(ctx context.Context, conversationID string) error {
	if err := validateConversationID(conversationID); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.sessions, conversationID)
	s.mu.Unlock()

	if err := s.client.Del(ctx, redisKeyPrefix+conversationID).Err(); err != nil {
		return fmt.Errorf("sessions: 删除 Redis 会话失败: %w", err)
	}
	return nil
}

// Close 清空内部缓存。Redis 连接由调用方持有与关闭。
func (s *RedisStore) Close() error {
	s.mu.Lock()
	s.sessions = make(map[string]*RedisSession)
	s.mu.Unlock()
	return nil
}

// SQLiteStore 按会话 ID 管理一组基于 SQLite 的会话。
//
// 设计取舍：采用单表 + conversation_id 列，而不是为每个会话 ID 建独立表。
// 原因：会话 ID 由调用方传入、数量不可控，动态建表既带来 SQL 注入/表名
// 校验负担，也使 schema 演进（如新增 last_active 列）难以统一迁移；
// 单表配合 (conversation_id, id) 索引即可高效按会话读写。因此现有
// SQLiteSession（独立表实现）保持不动，本 Store 用内部视图类型
// sqliteStoreSession 实现同一 Session 接口。
//
// 过期策略：每次读写触碰会话时更新 last_active（Unix 秒），
// 可选 WithCleanupInterval 启动后台 goroutine，定期删除闲置超过
// idle 时长的行；Close 停止该 goroutine。
type SQLiteStore struct {
	db *sql.DB

	cleanupInterval time.Duration
	idleDuration    time.Duration

	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	sessions map[string]*sqliteStoreSession
}

// SQLiteStoreOption 是 SQLiteStore 的可选项。
type SQLiteStoreOption func(*SQLiteStore)

// WithCleanupInterval 启用后台清理：每 interval 触发一次清理，
// 删除 last_active 距今超过 idle 时长的会话行。idle 未单独设置时
// 默认等于 interval。
func WithCleanupInterval(interval time.Duration) SQLiteStoreOption {
	return func(s *SQLiteStore) {
		s.cleanupInterval = interval
	}
}

// WithIdleDuration 单独指定会话闲置判定时长（配合 WithCleanupInterval 使用）。
func WithIdleDuration(idle time.Duration) SQLiteStoreOption {
	return func(s *SQLiteStore) {
		s.idleDuration = idle
	}
}

// storeTableName 是 SQLiteStore 的单表表名（固定合法标识符，无注入风险）。
const storeTableName = "nexus_sessions"

// NewSQLiteStore 创建（或打开）一个 SQLite 会话管理器，自动建表与索引。
func NewSQLiteStore(db *sql.DB, opts ...SQLiteStoreOption) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("sessions: db 不能为 nil")
	}

	s := &SQLiteStore{
		db:       db,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		sessions: make(map[string]*sqliteStoreSession),
	}
	for _, opt := range opts {
		opt(s)
	}

	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL,
		data            TEXT NOT NULL,
		last_active     REAL NOT NULL
	)`, storeTableName)
	if _, err := db.Exec(stmt); err != nil {
		return nil, fmt.Errorf("sessions: 创建会话存储表失败: %w", err)
	}
	idx := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_%s_conv ON %s (conversation_id, id)`,
		storeTableName, storeTableName)
	if _, err := db.Exec(idx); err != nil {
		return nil, fmt.Errorf("sessions: 创建会话存储索引失败: %w", err)
	}

	if s.cleanupInterval > 0 {
		if s.idleDuration <= 0 {
			s.idleDuration = s.cleanupInterval
		}
		go s.cleanupLoop()
	}
	return s, nil
}

// Session 返回指定会话的 Session 实例（不存在时创建）。
func (s *SQLiteStore) Session(_ context.Context, conversationID string) (agent.Session, error) {
	if err := validateConversationID(conversationID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[conversationID]; ok {
		return sess, nil
	}
	sess := &sqliteStoreSession{store: s, id: conversationID}
	s.sessions[conversationID] = sess
	return sess, nil
}

// Delete 删除指定会话的全部行并移除缓存实例。
func (s *SQLiteStore) Delete(ctx context.Context, conversationID string) error {
	if err := validateConversationID(conversationID); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.sessions, conversationID)
	s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE conversation_id = ?`, storeTableName),
		conversationID)
	if err != nil {
		return fmt.Errorf("sessions: 删除 SQLite 会话失败: %w", err)
	}
	return nil
}

// Close 停止后台清理 goroutine 并清空缓存。底层 *sql.DB 由调用方关闭。
func (s *SQLiteStore) Close() error {
	if s.cleanupInterval > 0 {
		select {
		case <-s.stop: // 已关闭
		default:
			close(s.stop)
			<-s.done
		}
	}

	s.mu.Lock()
	s.sessions = make(map[string]*sqliteStoreSession)
	s.mu.Unlock()
	return nil
}

// cleanupLoop 周期性删除闲置会话行，直到 Close。
func (s *SQLiteStore) cleanupLoop() {
	defer close(s.done)

	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.cleanupOnce()
		}
	}
}

// cleanupOnce 删除 last_active 距今超过 idleDuration 的行。
// 清理失败仅忽略，等待下一轮（后台任务不应中断服务）。
func (s *SQLiteStore) cleanupOnce() {
	cutoff := float64(time.Now().Add(-s.idleDuration).Unix())
	_, _ = s.db.ExecContext(context.Background(),
		fmt.Sprintf(`DELETE FROM %s WHERE last_active < ?`, storeTableName),
		cutoff)
}

// sqliteStoreSession 是 SQLiteStore 内部的会话视图：
// 与 SQLiteSession（独立表）不同，它按 conversation_id 过滤单表行，
// 并在读写时触碰 last_active 以支持闲置清理。
type sqliteStoreSession struct {
	store *SQLiteStore
	id    string
}

// Messages 按 id 顺序返回该会话的全部历史消息，并刷新 last_active。
func (s *sqliteStoreSession) Messages(ctx context.Context) ([]*schema.Message, error) {
	rows, err := s.store.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT data FROM %s WHERE conversation_id = ? ORDER BY id ASC`, storeTableName),
		s.id)
	if err != nil {
		return nil, fmt.Errorf("sessions: 读取 SQLite 会话失败: %w", err)
	}
	defer rows.Close()

	var msgs []*schema.Message
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("sessions: 读取 SQLite 会话行失败: %w", err)
		}
		var msg schema.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return nil, fmt.Errorf("sessions: 解析 SQLite 会话消息失败: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessions: 读取 SQLite 会话失败: %w", err)
	}

	s.touch(ctx)
	return msgs, nil
}

// Append 在单个事务内逐条 INSERT 新消息并刷新 last_active。
// 空参为 no-op。
func (s *sqliteStoreSession) Append(ctx context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (conversation_id, data, last_active) VALUES (?, ?, ?)`, storeTableName))
	if err != nil {
		return fmt.Errorf("sessions: 准备插入语句失败: %w", err)
	}
	defer stmt.Close()

	now := float64(time.Now().Unix())
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("sessions: 序列化会话消息失败: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, s.id, string(data), now); err != nil {
			return fmt.Errorf("sessions: 追加会话消息失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions: 提交事务失败: %w", err)
	}
	return nil
}

// Clear 删除该会话的全部行。
func (s *sqliteStoreSession) Clear(ctx context.Context) error {
	_, err := s.store.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE conversation_id = ?`, storeTableName),
		s.id)
	if err != nil {
		return fmt.Errorf("sessions: 清空 SQLite 会话失败: %w", err)
	}
	return nil
}

// touch 刷新会话的 last_active；失败静默忽略（不影响读取结果）。
func (s *sqliteStoreSession) touch(ctx context.Context) {
	_, _ = s.store.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET last_active = ? WHERE conversation_id = ?`, storeTableName),
		float64(time.Now().Unix()), s.id)
}
