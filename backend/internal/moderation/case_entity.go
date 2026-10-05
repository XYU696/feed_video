// package moderation：本文件定义"审核工单" moderation_cases。
//
// 业务理解：一条举报（Report）进来后，Agent 会围绕它取证、让模型判定，
// 判定的结果、证据、最终怎么处置，都记录在一张"工单"（Case）里。
// D2 阶段 Agent 还没接，所以先建一张【空工单】：只填来源举报和被举报对象，
// 决策/证据等字段都留空，用来打通 MQ → Worker → 落库这条链路。
package moderation

// import 导入 time：工单创建/审核时间字段要用。
import "time"

// Case 是"一张审核工单"在程序里的样子，对应 MySQL moderation_cases 表的一行。
type Case struct {
	// ID 工单唯一编号（主键）。
	ID uint `gorm:"primaryKey" json:"id"`

	// ReportID 来源举报编号（这条工单是哪条举报触发的）。
	// index 建索引：常按举报反查工单、也用来做幂等（同一条举报只建一张工单）。
	ReportID uint `gorm:"not null;index:idx_case_report" json:"report_id"`

	// TargetType 被处置对象类型（冗余自举报，免得每次再 join reports）。
	TargetType string `gorm:"type:varchar(20);not null" json:"target_type"`
	// TargetID 被处置对象编号。
	TargetID uint `gorm:"not null" json:"target_id"`

	// Decision 模型给出的决策：safe/remove/warn/ban/escalate（取值见下方常量）。
	// D2 空工单阶段留空。
	Decision string `gorm:"type:varchar(20)" json:"decision"`
	// Confidence 模型置信度，0–1。
	Confidence float64 `json:"confidence"`
	// Reason 模型给出的判定理由（可能较长，用 text）。
	Reason string `gorm:"type:text" json:"reason"`
	// PolicyHit 命中的平台政策条款。
	PolicyHit string `gorm:"type:varchar(100)" json:"policy_hit"`

	// EvidenceSnapshot 取证结果快照（详情、评论、历史违规等拼成的 JSON，用 longtext）。
	// D2 阶段还没取证，留空。
	EvidenceSnapshot string `gorm:"type:longtext" json:"evidence_snapshot"`

	// ModelInfo 模型名 + 版本。
	ModelInfo string `gorm:"type:varchar(100)" json:"model_info"`
	// TokenUsage 本次调用 token 用量（prompt/completion/total 的 JSON）。
	TokenUsage string `gorm:"type:varchar(200)" json:"token_usage"`

	// Status 工单当前状态。
	//
	// 取值见下方常量：pending（待人工）/ approved / rejected / overridden / auto_executed。
	// default:pending 新建默认待人工；index 供审核台按状态扫描。
	Status string `gorm:"type:varchar(20);not null;default:pending;index" json:"status"`

	// FinalDecision 人工终审动作。
	FinalDecision string `gorm:"type:varchar(20)" json:"final_decision"`
	// ReviewerID 审核员账号编号；用 *uint 指针：没人审时为 NULL。
	ReviewerID *uint `json:"reviewer_id"`
	// ReviewedAt 审核时间；用 *time.Time 指针：没审核时为 NULL。
	ReviewedAt *time.Time `json:"reviewed_at"`

	// ExecutedAction 实际执行的动作清单（JSON）。
	ExecutedAction string `gorm:"type:varchar(200)" json:"executed_action"`

	// InjectionSuspected 举报说明或被举报内容中疑似包含 Prompt 注入攻击（D8）。
	// 仅为研判标记：命中启发式规则即为 true，不改变处置流程——
	// 真正的防线是"模型没有处置权 + 动作白名单/阈值/锁"。
	InjectionSuspected bool `gorm:"not null;default:false;index" json:"injection_suspected"`
	// InjectionSignals 命中的可疑特征（逗号分隔），供人工研判与 D9 注入拦截率统计。
	InjectionSignals string `gorm:"type:varchar(255)" json:"injection_signals"`

	// CreatedAt 工单创建时间，GORM 自动写。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	// UpdatedAt 工单更新时间，GORM 自动写。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// 决策常量：Decision 的合法取值（模型输出的"决策意图"）。
const (
	// DecisionSafe 判定安全，不关停对象（仅关单）。
	DecisionSafe = "safe"
	// DecisionRemove 删除/下架对象。
	DecisionRemove = "remove"
	// DecisionWarn 给作者发警告。
	DecisionWarn = "warn"
	// DecisionBan 封禁账号（二期）。
	DecisionBan = "ban"
	// DecisionEscalate 有争议/低置信，转人工。
	DecisionEscalate = "escalate"
)

// 工单状态常量：Status 的合法取值。
const (
	// CaseStatusPending 待人工处理（D2 空工单默认就是它）。
	CaseStatusPending = "pending"
	// CaseStatusApproved 人工终审通过。
	CaseStatusApproved = "approved"
	// CaseStatusRejected 人工终审驳回。
	CaseStatusRejected = "rejected"
	// CaseStatusOverridden 人工改判。
	CaseStatusOverridden = "overridden"
	// CaseStatusAutoExecuted Agent 高置信自动处置完成。
	CaseStatusAutoExecuted = "auto_executed"
)
