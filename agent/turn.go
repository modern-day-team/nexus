package agent

import (
	"context"
)

// turnGate 是容量为 1 的信号量，负责同一 Agent 实例上轮次的串行化。
// 与业务逻辑解耦：Agent 各入口只调用 begin/end。
type turnGate chan struct{}

func newTurnGate() turnGate {
	return make(chan struct{}, 1)
}

// begin 获取本轮执行权；已有轮次进行中时阻塞等待，ctx 取消时返回错误。
func (g turnGate) begin(ctx context.Context) error {
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// end 释放本轮执行权。必须与 begin 配对调用。
func (g turnGate) end() {
	<-g
}
