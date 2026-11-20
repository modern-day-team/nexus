package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"

	// 注册 database/sql 驱动（纯 Go 实现，无需 CGO）。
	_ "modernc.org/sqlite"
)

// SQLiteSession 把会话消息存入 SQLite 表，跨进程恢复。
//
// 表结构由 NewSQLiteSession 自动创建：id INTEGER PRIMARY KEY AUTOINCREMENT
// 兼作消息顺序（seq），data 列存每条消息的 JSON 文本。Append 在单个
// 事务内逐条 INSERT；Messages 按 id 升序 SELECT；Clear 执行 DELETE。
//
// *sql.DB 自带连接池且并发安全；跨进程并发写依赖 OpenSQLite 设置的
// busy_timeout，遇到锁冲突会自动等待重试。
type SQLiteSession struct {
	db    *sql.DB
	table string
}

// NewSQLiteSession 创建（或打开）一个 SQLite 会话，并自动建表。
// table 是消息表名；为安全起见只允许合法标识符。
func NewSQLiteSession(db *sql.DB, table string) (*SQLiteSession, error) {
	if db == nil {
		return nil, fmt.Errorf("sessions: db 不能为 nil")
	}
	if !validIdentifier(table) {
		return nil, fmt.Errorf("sessions: 非法表名 %q", table)
	}

	// 表已存在时跳过（IF NOT EXISTS），重复打开同一会话安全。
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			data TEXT NOT NULL
		)`, table)
	if _, err := db.Exec(stmt); err != nil {
		return nil, fmt.Errorf("sessions: 创建会话表失败: %w", err)
	}
	return &SQLiteSession{db: db, table: table}, nil
}

// OpenSQLite 打开（不存在则创建）一个 SQLite 数据库文件，
// 并设置常用 pragmas：busy_timeout 避免锁冲突报错，WAL 提升
// 读写并发性能，foreign_keys 保持约束语义。返回的 *sql.DB 并发安全。
func OpenSQLite(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sessions: 数据库路径不能为空")
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sessions: 打开 SQLite 数据库失败: %w", err)
	}
	// modernc.org/sqlite 是纯 Go 驱动，没有 C 库的连接限制，
	// 但 SQLite 同一时刻只允许一个写者，限制连接数减少锁竞争。
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sessions: 连接 SQLite 数据库失败: %w", err)
	}
	return db, nil
}

// Messages 按 id 顺序（即写入顺序）返回全部历史消息。
func (s *SQLiteSession) Messages(ctx context.Context) ([]*schema.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT data FROM %s ORDER BY id ASC`, s.table))
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
	return msgs, nil
}

// Append 在单个事务内逐条 INSERT 新消息，保证整批原子写入。
// 空参为 no-op。
func (s *SQLiteSession) Append(ctx context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (data) VALUES (?)`, s.table))
	if err != nil {
		return fmt.Errorf("sessions: 准备插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("sessions: 序列化会话消息失败: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, string(data)); err != nil {
			return fmt.Errorf("sessions: 追加会话消息失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions: 提交事务失败: %w", err)
	}
	return nil
}

// Clear 删除表内全部消息。
func (s *SQLiteSession) Clear(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s`, s.table)); err != nil {
		return fmt.Errorf("sessions: 清空 SQLite 会话失败: %w", err)
	}
	return nil
}

// validIdentifier 只允许字母/数字/下划线且不以数字开头，
// 防止表名拼接引入 SQL 注入。
func validIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
