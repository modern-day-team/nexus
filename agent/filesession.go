package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// FileSession 是基于 JSON 文件的 Session 实现，进程重启后会话可恢复。
// 每次 Append/Clear 都会整体落盘，适合轻量持久化场景；高频写入请自建数据库实现。
// 同一进程内并发安全；跨进程访问同一文件需自行加锁。
type FileSession struct {
	path string
	mu   sync.Mutex
}

// NewFileSession 创建（或打开）一个文件会话。
// path 的父目录不存在时自动创建；已有文件时加载其中的历史消息。
func NewFileSession(path string) (*FileSession, error) {
	if path == "" {
		return nil, errors.New("agent: FileSession 路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("agent: 创建会话目录失败: %w", err)
		}
	}
	return &FileSession{path: path}, nil
}

// Messages 返回当前会话的全部消息。
func (s *FileSession) Messages(context.Context) ([]*schema.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.load()
}

// Append 追加消息并落盘。
func (s *FileSession) Append(_ context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	history, err := s.load()
	if err != nil {
		return err
	}
	history = append(history, msgs...)
	return s.save(history)
}

// Clear 清空消息并落盘。
func (s *FileSession) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.save(nil)
}

// load 读取并反序列化文件，文件不存在或为空时返回空。调用方必须持有锁。
func (s *FileSession) load() ([]*schema.Message, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: 读取会话文件失败: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var msgs []*schema.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("agent: 解析会话文件失败: %w", err)
	}
	return msgs, nil
}

// save 序列化并通过临时文件原子写入。调用方必须持有锁。
func (s *FileSession) save(msgs []*schema.Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("agent: 序列化会话失败: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("agent: 写入会话文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("agent: 替换会话文件失败: %w", err)
	}
	return nil
}
