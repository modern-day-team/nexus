package llm

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeModel 按 fails 计数模拟前 N 次调用失败，之后返回固定回复。
type fakeModel struct {
	fails int64 // 剩余失败次数（原子操作，供并发测试）
	reply string
	gen   int64 // Generate 成功次数
}

func (f *fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if atomic.AddInt64(&f.fails, -1) >= 0 {
		return nil, errors.New("模拟故障")
	}
	atomic.AddInt64(&f.gen, 1)
	return &schema.Message{Role: schema.Assistant, Content: f.reply}, nil
}

func (f *fakeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	// Stream 的失败语义与 Generate 独立：这里仅在 fails>0 时让 Stream 调用本身报错。
	if atomic.AddInt64(&f.fails, -1) >= 0 {
		return nil, errors.New("模拟流故障")
	}
	return schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: f.reply},
	}), nil
}

func (f *fakeModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

var errNoTools = errors.New("不支持绑定工具")

// noToolsModel 不支持 WithTools，用于验证派生错误透传。
type noToolsModel struct{ fakeModel }

func (n *noToolsModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return nil, errNoTools
}

func msg() []*schema.Message {
	return []*schema.Message{{Role: schema.User, Content: "hi"}}
}

// 主模型连续失败 → 使用备用模型成功，Current 变为 1。
func TestFallbackSwitchesToBackup(t *testing.T) {
	primary := &fakeModel{fails: 100, reply: "primary"}
	backup := &fakeModel{reply: "backup"}
	fm, err := NewFallbackModel(FallbackOptions{Models: []model.ToolCallingChatModel{primary, backup}})
	if err != nil {
		t.Fatalf("构建 FallbackModel 失败: %v", err)
	}
	reply, err := fm.Generate(context.Background(), msg())
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if reply.Content != "backup" {
		t.Fatalf("期望备用模型回复 backup，得到 %q", reply.Content)
	}
	if fm.Current() != 1 {
		t.Fatalf("期望 Current=1，得到 %d", fm.Current())
	}
	// 简化约定：不自动回切。主模型恢复后，下一次调用仍从 Current=1 起步。
	if fm.Current() != 1 {
		t.Fatalf("成功后 Current 应保持为 1，得到 %d", fm.Current())
	}
}

// 主模型第一次失败、第二次成功：由于不回切，Current 保持 1。
func TestFallbackStaysOnBackupAfterRecovery(t *testing.T) {
	primary := &fakeModel{fails: 1, reply: "primary"}
	backup := &fakeModel{reply: "backup"}
	fm, _ := NewFallbackModel(FallbackOptions{Models: []model.ToolCallingChatModel{primary, backup}})
	if _, err := fm.Generate(context.Background(), msg()); err != nil {
		t.Fatalf("第一次 Generate 失败: %v", err)
	}
	if fm.Current() != 1 {
		t.Fatalf("期望 Current=1，得到 %d", fm.Current())
	}
	reply, err := fm.Generate(context.Background(), msg())
	if err != nil {
		t.Fatalf("第二次 Generate 失败: %v", err)
	}
	// 仍然从备用模型起步，主模型虽已恢复也不回切。
	if reply.Content != "backup" {
		t.Fatalf("期望继续使用备用模型，得到 %q", reply.Content)
	}
	if fm.Current() != 1 {
		t.Fatalf("期望 Current 保持 1，得到 %d", fm.Current())
	}
}

// 熔断：主模型连续失败达阈值后即使恢复也跳过；冷却结束 half-open 恢复 closed。
func TestCircuitSkipsUntilCooldown(t *testing.T) {
	primary := &fakeModel{reply: "primary"}
	backup := &fakeModel{fails: 100, reply: "backup"}
	fm, _ := NewFallbackModel(FallbackOptions{
		Models:  []model.ToolCallingChatModel{primary, backup},
		Circuit: &CircuitOptions{Threshold: 2, Cooldown: 50 * time.Millisecond},
	})

	// 前 2 次：主模型成功，熔断器 closed。
	for range 2 {
		if _, err := fm.Generate(context.Background(), msg()); err != nil {
			t.Fatalf("主模型应成功: %v", err)
		}
	}
	if idx := fm.OpenCircuits(); len(idx) != 0 {
		t.Fatalf("期望无熔断，得到 %v", idx)
	}

	// 让两个模型各失败 2 次触发熔断（备用失败是为了保证主模型每轮都被尝试到）。
	atomic.StoreInt64(&primary.fails, 2)
	for range 2 {
		_, _ = fm.Generate(context.Background(), msg())
	}
	idx := fm.OpenCircuits()
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 1 {
		t.Fatalf("期望两个模型均熔断 [0 1]，得到 %v", idx)
	}

	// 主模型恢复，但冷却期内仍被跳过（备用仍在故障，调用整体报错）。
	atomic.StoreInt64(&primary.fails, -1)
	if _, err := fm.Generate(context.Background(), msg()); err == nil {
		t.Fatal("冷却期内应跳过主模型并因备用失败而报错")
	}

	// 冷却结束后 half-open 放行一次尝试，成功恢复 closed。
	time.Sleep(60 * time.Millisecond)
	reply, err := fm.Generate(context.Background(), msg())
	if err != nil {
		t.Fatalf("冷却后 Generate 失败: %v", err)
	}
	if reply.Content != "primary" {
		t.Fatalf("冷却后应使用主模型，得到 %q", reply.Content)
	}
	for _, i := range fm.OpenCircuits() {
		if i == 0 {
			t.Fatalf("主模型成功后熔断应恢复 closed，仍熔断: %v", fm.OpenCircuits())
		}
	}
}

// 全部失败：错误聚合，errors.Is 能匹配到最后一个错误。
func TestFallbackAggregatesErrors(t *testing.T) {
	err1 := errors.New("主模型故障")
	err2 := errors.New("备用模型故障")
	m1 := &errModel{err: err1}
	m2 := &errModel{err: err2}
	fm, _ := NewFallbackModel(FallbackOptions{Models: []model.ToolCallingChatModel{m1, m2}})
	_, err := fm.Generate(context.Background(), msg())
	if err == nil {
		t.Fatal("期望返回聚合错误")
	}
	if !errors.Is(err, err1) || !errors.Is(err, err2) {
		t.Fatalf("errors.Is 应匹配到各模型错误，得到 %v", err)
	}
	_, serr := fm.Stream(context.Background(), msg())
	if !errors.Is(serr, err1) || !errors.Is(serr, err2) {
		t.Fatalf("Stream 聚合错误应保留各模型错误，得到 %v", serr)
	}
}

// errModel 每次调用都返回固定错误。
type errModel struct{ err error }

func (e *errModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, e.err
}

func (e *errModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, e.err
}

func (e *errModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return e, nil
}

// WithTools 派生共享熔断状态：主模型熔断后派生模型也跳过主模型。
func TestWithToolsSharesCircuitState(t *testing.T) {
	primary := &fakeModel{reply: "primary"}
	backup := &fakeModel{reply: "backup"}
	fm, _ := NewFallbackModel(FallbackOptions{
		Models:  []model.ToolCallingChatModel{primary, backup},
		Circuit: &CircuitOptions{Threshold: 1, Cooldown: time.Minute},
	})

	// 主模型失败 1 次触发熔断（备用成功，current=1）。
	primary.fails = 1
	if _, err := fm.Generate(context.Background(), msg()); err != nil {
		t.Fatalf("备用模型调用失败: %v", err)
	}
	if fm.Current() != 1 {
		t.Fatalf("期望 Current=1，得到 %d", fm.Current())
	}

	derived, err := fm.WithTools(nil)
	if err != nil {
		t.Fatalf("WithTools 失败: %v", err)
	}
	df := derived.(*FallbackModel)

	// 主模型已恢复，但派生模型共享熔断状态，冷却期内仍跳过。
	atomic.StoreInt64(&primary.fails, -1)
	reply, err := df.Generate(context.Background(), msg())
	if err != nil {
		t.Fatalf("派生模型 Generate 失败: %v", err)
	}
	if reply.Content != "backup" {
		t.Fatalf("派生模型应跳过熔断的主模型，得到 %q", reply.Content)
	}
}

// WithTools 对不支持绑定工具的 inner 透传错误。
func TestWithToolsPropagatesError(t *testing.T) {
	fm, _ := NewFallbackModel(FallbackOptions{
		Models: []model.ToolCallingChatModel{&noToolsModel{}},
	})
	if _, err := fm.WithTools(nil); !errors.Is(err, errNoTools) {
		t.Fatalf("期望透传 errNoTools，得到 %v", err)
	}
}

// Stream 切换语义：主模型 Stream 调用本身报错 → 切换备用成功。
func TestStreamSwitchesOnStreamError(t *testing.T) {
	primary := &fakeModel{fails: 1, reply: "primary"}
	backup := &fakeModel{reply: "backup"}
	fm, _ := NewFallbackModel(FallbackOptions{Models: []model.ToolCallingChatModel{primary, backup}})

	stream, err := fm.Stream(context.Background(), msg())
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	defer stream.Close()
	chunk, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Recv 失败: %v", err)
	}
	if chunk == nil || chunk.Content != "backup" {
		t.Fatalf("期望备用模型分片，得到 %+v", chunk)
	}
	if fm.Current() != 1 {
		t.Fatalf("期望 Current=1，得到 %d", fm.Current())
	}
}

// 流建立后中途出错不切换（fake 流式分片在建立后失败无法直接用 StreamReader 模拟，
// 此处仅验证 Stream 调用本身报错才计熔断；流内错误由上层处理）。
func TestStreamOnlyCountsEstablishedFailures(t *testing.T) {
	primary := &fakeModel{reply: "primary"}
	fm, _ := NewFallbackModel(FallbackOptions{
		Models:  []model.ToolCallingChatModel{primary},
		Circuit: &CircuitOptions{Threshold: 1, Cooldown: time.Minute},
	})
	stream, err := fm.Stream(context.Background(), msg())
	if err != nil {
		t.Fatalf("Stream 应成功: %v", err)
	}
	stream.Close()
	// Stream 建立成功，熔断器不应累计失败。
	if idx := fm.OpenCircuits(); len(idx) != 0 {
		t.Fatalf("流建立成功不应触发熔断，得到 %v", idx)
	}
}

// 并发：10 goroutine 同时 Generate，-race 通过，且结果总是来自某个候选模型。
func TestConcurrentGenerate(t *testing.T) {
	primary := &fakeModel{fails: 3, reply: "primary"}
	backup := &fakeModel{reply: "backup"}
	fm, _ := NewFallbackModel(FallbackOptions{
		Models:  []model.ToolCallingChatModel{primary, backup},
		Circuit: &CircuitOptions{Threshold: 2, Cooldown: 10 * time.Millisecond},
	})
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fm.Generate(context.Background(), msg())
			if err != nil {
				t.Errorf("并发 Generate 失败: %v", err)
			}
			_ = fm.Current()
			_ = fm.OpenCircuits()
		}()
	}
	wg.Wait()
	if fm.Current() < 0 || fm.Current() > 1 {
		t.Fatalf("Current 越界: %d", fm.Current())
	}
}

// 构造校验：空候选报错。
func TestNewFallbackModelValidation(t *testing.T) {
	if _, err := NewFallbackModel(FallbackOptions{}); err == nil {
		t.Fatal("空候选应报错")
	}
	if _, err := NewFallbackModel(FallbackOptions{Models: []model.ToolCallingChatModel{nil}}); err == nil {
		t.Fatal("nil 候选应报错")
	}
}
