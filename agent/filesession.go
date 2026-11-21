package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// FileSession 是基于 JSONL 文件的 Session 实现，进程重启后会话可恢复。
//
// 内存为真相源：Messages/Append 直接读写内存缓存。落盘采用追加写：
// Append 只把新增消息以 JSONL 行追加到文件末尾，复杂度为 O(新增条数)，
// 不重写历史。进程崩溃最多丢失最后一条未完成落盘的消息；如需更强
// 持久性保证，请自建数据库实现 Session。
//
// 注意：文件格式为 JSONL（每行一条消息），与早期整体 JSON 格式不兼容。
//
// 同一进程内并发安全；跨进程访问同一文件需自行加锁。
type FileSession struct {
	path string
	mu   sync.RWMutex
	msgs []*schema.Message
}

// NewFileSession 创建（或打开）一个文件会话。
// path 的父目录不存在时自动创建；已有文件时将其中的历史消息加载进内存。
func NewFileSession(path string) (*FileSession, error) {
	if path == "" {
		return nil, errors.New("agent: FileSession 路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("agent: 创建会话目录失败: %w", err)
		}
	}

	s := &FileSession{path: path}
	msgs, err := s.readFile()
	if err != nil {
		return nil, err
	}
	s.msgs = msgs
	return s, nil
}

// Messages 返回历史消息的副本。
func (s *FileSession) Messages(context.Context) ([]*schema.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*schema.Message, len(s.msgs))
	for i, msg := range s.msgs {
		out[i] = cloneMessage(msg)
	}
	return out, nil
}

// cloneMessage 深拷贝消息，避免调用方修改影响会话缓存。
//
// 性能说明：曾考虑用 "上一次 Messages 输出" 的快照缓存 + generation
// 计数省掉重复深拷贝，但因调用方可能修改返回副本、而元素指针无法
// 无污染地复用（复用会被外部修改污染缓存与内部状态），深拷贝是语义
// 正确性的必要代价，故不做缓存。实测成本约为每条消息 1 次切片分配
// + O(len) 拷贝（1000 条历史 ≈ 170µs/次、1001 allocs/op，
// 见 BenchmarkFileSessionMessagesLarge），随历史长度线性增长。
// cloneMessage 本身已是最小实现：只浅拷贝结构体并按需拷贝
// ToolCalls/MultiContent 切片，纯文本消息零分配。
func cloneMessage(msg *schema.Message) *schema.Message {
	if msg == nil {
		return nil
	}
	clone := *msg
	if len(msg.ToolCalls) > 0 {
		clone.ToolCalls = append([]schema.ToolCall(nil), msg.ToolCalls...)
	}
	if len(msg.MultiContent) > 0 {
		clone.MultiContent = append([]schema.ChatMessagePart(nil), msg.MultiContent...)
	}
	return &clone
}

// Append 追加消息并追加落盘。
func (s *FileSession) Append(_ context.Context, msgs ...*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.appendFile(msgs); err != nil {
		// 落盘失败时从文件重载，保证内存与磁盘一致。
		if reloadErr := s.reload(); reloadErr != nil {
			return errors.Join(err, reloadErr)
		}
		return err
	}
	s.msgs = append(s.msgs, msgs...)
	return nil
}

// Clear 清空消息并截断落盘。
func (s *FileSession) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.WriteFile(s.path, nil, 0o644); err != nil {
		return fmt.Errorf("agent: 清空会话文件失败: %w", err)
	}
	s.msgs = nil
	return nil
}

// appendFile 把新增消息序列化为 JSONL 行并追加到文件末尾。
func (s *FileSession) appendFile(msgs []*schema.Message) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, msg := range msgs {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("agent: 序列化会话消息失败: %w", err)
		}
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("agent: 打开会话文件失败: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("agent: 追加会话文件失败: %w", err)
	}
	return nil
}

// reload 从文件重载内存缓存。
func (s *FileSession) reload() error {
	msgs, err := s.readFile()
	if err != nil {
		return err
	}
	s.msgs = msgs
	return nil
}

// readFile 读取并解析 JSONL 文件，文件不存在或为空时返回空。
func (s *FileSession) readFile() ([]*schema.Message, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: 读取会话文件失败: %w", err)
	}
	defer f.Close()

	var msgs []*schema.Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024) // 单行消息可能较大
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg schema.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("agent: 解析会话文件失败: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("agent: 读取会话文件失败: %w", err)
	}
	return msgs, nil
}
