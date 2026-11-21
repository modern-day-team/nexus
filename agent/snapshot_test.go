package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := NewMemorySession()
	msgs := []*schema.Message{
		schema.UserMessage("你好，帮我查一下天气"),
		{
			Role:    schema.Assistant,
			Content: "好的",
			ToolCalls: []schema.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "weather", Arguments: `{"city":"北京"}`},
			}},
		},
		schema.ToolMessage("晴，25 度", "call_1"),
		schema.AssistantMessage("北京今天晴。", nil),
		schema.UserMessage("Berlin is cloudy"),
	}
	if err := src.Append(ctx, msgs...); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	data, err := ExportSession(ctx, src)
	if err != nil {
		t.Fatalf("ExportSession 失败: %v", err)
	}
	if strings.Count(string(data), "\n") != len(msgs) {
		t.Fatalf("JSONL 行数不符: %q", string(data))
	}

	dst := NewMemorySession()
	if err := ImportSession(ctx, dst, data); err != nil {
		t.Fatalf("ImportSession 失败: %v", err)
	}

	got, err := dst.Messages(ctx)
	if err != nil {
		t.Fatalf("Messages 失败: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("导入条数 = %d, want %d", len(got), len(msgs))
	}
	for i, want := range msgs {
		m := got[i]
		if m.Role != want.Role || m.Content != want.Content {
			t.Fatalf("第 %d 条不符: got %s/%q, want %s/%q", i, m.Role, m.Content, want.Role, want.Content)
		}
		if len(m.ToolCalls) != len(want.ToolCalls) {
			t.Fatalf("第 %d 条 ToolCalls 数不符: %+v vs %+v", i, m.ToolCalls, want.ToolCalls)
		}
		for j, tc := range want.ToolCalls {
			if m.ToolCalls[j].Function.Name != tc.Function.Name ||
				m.ToolCalls[j].Function.Arguments != tc.Function.Arguments {
				t.Fatalf("第 %d 条 ToolCall[%d] 不符: %+v vs %+v", i, j, m.ToolCalls[j], tc)
			}
		}
	}
}

func TestImportAppendsWithoutClearing(t *testing.T) {
	ctx := context.Background()
	sess := NewMemorySession()
	if err := sess.Append(ctx, schema.UserMessage("已有消息")); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	empty, err := ExportSession(ctx, mustSession(t, schema.UserMessage("导入的一条")))
	if err != nil {
		t.Fatalf("ExportSession 失败: %v", err)
	}
	if err := ImportSession(ctx, sess, empty); err != nil {
		t.Fatalf("ImportSession 失败: %v", err)
	}

	got, _ := sess.Messages(ctx)
	if len(got) != 2 || got[0].Content != "已有消息" || got[1].Content != "导入的一条" {
		t.Fatalf("追加导入结果 = %+v", got)
	}
}

// mustSession 构建含单条消息的会话。
func mustSession(t *testing.T, msg *schema.Message) Session {
	t.Helper()
	s := NewMemorySession()
	if err := s.Append(context.Background(), msg); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	return s
}

func TestImportInvalidLineHasLineNumber(t *testing.T) {
	ctx := context.Background()
	sess := NewMemorySession()
	data := []byte(`{"role":"user","content":"ok"}
{"role":"broken","content": not-json}
`)

	err := ImportSession(ctx, sess, data)
	if err == nil {
		t.Fatal("非法行应报错")
	}
	if !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("错误应带行号 2, got %v", err)
	}
}

func TestImportSkipsEmptyLines(t *testing.T) {
	ctx := context.Background()
	sess := NewMemorySession()
	data := []byte("\n\n{\"role\":\"user\",\"content\":\"a\"}\n\n")
	if err := ImportSession(ctx, sess, data); err != nil {
		t.Fatalf("ImportSession 失败: %v", err)
	}
	got, _ := sess.Messages(ctx)
	if len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("消息 = %+v", got)
	}
}

func TestExportImportWithFileSession(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/snap_session.jsonl"
	fs, err := NewFileSession(path)
	if err != nil {
		t.Fatalf("NewFileSession 失败: %v", err)
	}
	if err := fs.Append(ctx, schema.UserMessage("hi"), schema.AssistantMessage("hello", nil)); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	data, err := ExportSession(ctx, fs)
	if err != nil {
		t.Fatalf("ExportSession 失败: %v", err)
	}

	mem := NewMemorySession()
	if err := ImportSession(ctx, mem, data); err != nil {
		t.Fatalf("ImportSession 失败: %v", err)
	}
	got, _ := mem.Messages(ctx)
	if len(got) != 2 || got[1].Content != "hello" {
		t.Fatalf("消息 = %+v", got)
	}
}
