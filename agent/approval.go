package agent

import (
	"crypto/rand"
	"encoding/hex"
)

// 本文件定义 HITL（human-in-the-loop）审批的数据契约：
// 审批请求经事件流发给外部，外部以 ApprovalDecision 回应。
// 交互式审批桥 InteractiveApprover 见 interactive.go。

// ApprovalRequest 描述一次待审批的工具调用。
type ApprovalRequest struct {
	CallID    string // 本次调用的唯一 ID（request 内部生成）
	ToolName  string
	Arguments string
}

// ApprovalDecision 是外部对审批请求的决定。
type ApprovalDecision struct {
	Allow  bool
	Reason string // 拒绝原因，作为工具结果反馈给模型
}

// newApprovalRequest 生成携带随机唯一 ID 的审批请求。
func newApprovalRequest(toolName, arguments string) *ApprovalRequest {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退化为 ID 为空串，不影响审批语义。
		return &ApprovalRequest{ToolName: toolName, Arguments: arguments}
	}
	return &ApprovalRequest{
		CallID:    "approval_" + hex.EncodeToString(b[:]),
		ToolName:  toolName,
		Arguments: arguments,
	}
}
