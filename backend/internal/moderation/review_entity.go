// package moderation：本文件定义"人工审核台"的请求结构、动作常量、
// 以及处置执行接口（D7）。
//
// 分层要点：本文件定义 ReviewActionExecutor【接口】，由 agent.ActionExecutor
// 在装配时隐式实现（Go 接口不需要 implements）。这样 moderation 包仍然
// 【不反向依赖 agent 包】——它只认识自己声明的接口。
package moderation

// import 导入 context：接口方法第一个参数。
import "context"

// 审核动作常量：ReviewRequest.Action 的合法取值。
const (
	// ReviewApprove 审核员同意 Agent 的处置建议。
	ReviewApprove = "approve"
	// ReviewReject 审核员驳回（内容没问题，不处置）。
	ReviewReject = "reject"
	// ReviewOverride 审核员改判（自己指定最终动作）。
	ReviewOverride = "override"
)

// ListCasesRequest 是"工单列表"接口的请求体。
type ListCasesRequest struct {
	// Status 按工单状态过滤；空串表示全部。
	Status string `json:"status"`
	// Page 页码，从 1 开始；空/非法由 Service 兜底为 1。
	Page int `json:"page"`
	// PageSize 每页条数；空/非法兜底，最大 100。
	PageSize int `json:"page_size"`
}

// ReviewRequest 是"人工终审"接口的请求体。
type ReviewRequest struct {
	// CaseID 要终审的工单编号（必填）。
	CaseID uint `json:"case_id"`
	// Action 审核动作 approve/reject/override（必填）。
	Action string `json:"action"`
	// FinalDecision 仅 override 时使用：审核员指定的最终动作
	// （safe/remove/warn；ban 二期不支持）。
	FinalDecision string `json:"final_decision"`
}

// ReviewActionExecutor 是"终审处置"所需能力的接口。
//
// agent.ActionExecutor 拥有这三个同签名方法，即自动实现本接口。
// 所有方法内部都必须走 Redis 防重复锁。
type ReviewActionExecutor interface {
	// HumanRemoveVideo 系统级删除视频（跳过作者归属校验）。
	HumanRemoveVideo(ctx context.Context, id uint) error
	// HumanRemoveComment 系统级删除评论。
	HumanRemoveComment(ctx context.Context, id uint) error
	// HumanWarn 给指定用户发警告私信；锁加在 (targetType,targetID) 对象上。
	HumanWarn(ctx context.Context, userID uint, targetType string, targetID uint, policyHit string) error
	// ResolveTargetAuthorID 实时查询对象作者：视频/评论→作者编号，用户→其本人。
	ResolveTargetAuthorID(ctx context.Context, targetType string, targetID uint) (uint, error)
}

// ResultNotifier 是"给举报人发结果通知"的能力接口（D10）。
// agent.ActionExecutor 用现有私信系统隐式实现它；moderation 不 import agent。
type ResultNotifier interface {
	// NotifyResult 给一个举报人的私信里写入结果文案。
	NotifyResult(ctx context.Context, reporterID, caseID uint, content string) error
}

// 审核台哨兵错误：Handler 据此翻译成 HTTP 状态码。
var (
	// ErrCaseNotFound 工单不存在。
	ErrCaseNotFound = newReviewError("case not found")
	// ErrCaseClosed 工单已终审，不能重复审核。
	ErrCaseClosed = newReviewError("case already reviewed")
	// ErrInvalidReviewAction 审核动作非法。
	ErrInvalidReviewAction = newReviewError("invalid review action")
	// ErrInvalidFinalDecision override 时指定的最终动作非法。
	ErrInvalidFinalDecision = newReviewError("invalid final decision")
	// ErrExecutorUnavailable 处置执行器未配置，无法执行 approve/override 的处置动作。
	ErrExecutorUnavailable = newReviewError("action executor is not available")
)

// reviewError 是包内简单的错误类型，实现 error 接口。
type reviewError struct{ msg string }

// newReviewError 普通函数：造一个审核错误。
func newReviewError(msg string) error { return &reviewError{msg: msg} }

// Error 方法：实现 error 接口，返回错误文本。
func (e *reviewError) Error() string { return e.msg }
