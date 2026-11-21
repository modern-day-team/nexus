package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// 本文件提供会话快照的导出/导入：把会话消息序列化为 JSONL 字节流
// （每行一条 schema.Message，与 FileSession 落盘格式一致），用于调试
// 回放、跨实现迁移与备份。工具包装器与快照互不依赖。

// snapshotMaxLineSize 是单行消息的最大字节数，与 FileSession 的
// readFile 保持一致（单行消息可能较大）。
const snapshotMaxLineSize = 16 * 1024 * 1024

// ExportSession 把会话全部消息序列化为 JSONL 字节流（每行一条
// schema.Message，与 FileSession 落盘格式一致），用于调试回放、迁移与备份。
func ExportSession(ctx context.Context, sess Session) ([]byte, error) {
	if sess == nil {
		return nil, fmt.Errorf("agent: 会话不能为空")
	}
	msgs, err := sess.Messages(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读取会话消息失败: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, msg := range msgs {
		if err := enc.Encode(msg); err != nil {
			return nil, fmt.Errorf("agent: 序列化会话消息失败: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// ImportSession 把 JSONL 字节流导入会话（追加模式：不清空现有消息）。
// 逐行解析，遇非法行返回错误（带行号）；空行跳过。
func ImportSession(ctx context.Context, sess Session, data []byte) error {
	if sess == nil {
		return fmt.Errorf("agent: 会话不能为空")
	}

	var msgs []*schema.Message
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), snapshotMaxLineSize)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg schema.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("agent: 第 %d 行非法（%w）", lineNo, err)
		}
		msgs = append(msgs, &msg)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("agent: 读取快照失败: %w", err)
	}

	if err := sess.Append(ctx, msgs...); err != nil {
		return fmt.Errorf("agent: 写入会话失败: %w", err)
	}
	return nil
}
