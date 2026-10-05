// package moderation：本文件定义"动作审计" moderation_audit（D10，agent.md §3.3）。
//
// 为什么还要一张审计表：工单（Case）记录的是"当前状态"，会被反复更新；
// 而审计表是【只追加、永不修改】的流水账——谁（系统/哪位审核员）在什么时候
// 做了什么动作、结果如何、报了什么错。用于：
//   - 复盘：一个视频为什么被删，翻流水即可；
//   - 回滚定位：误杀后找出处置发生的精确节点；
//   - 面试展示"全程可审计"。
package moderation

// import 导入：
import (
	// context：方法第一个参数。
	"context"
	// time：审计时刻。
	"time"

	// gorm：数据库操作。
	"gorm.io/gorm"
)

// 审计事件类型常量：Event 字段的合法取值。
const (
	// AuditEventCaseCreate 工单创建（链路起点）。
	AuditEventCaseCreate = "case.create"
	// AuditEventDecision 模型决策已回写。
	AuditEventDecision = "case.decision"
	// AuditEventAction 处置执行（自动或人工，成功/失败看 Result）。
	AuditEventAction = "action.execute"
	// AuditEventEscalate 转人工（闸门未过/决策失败）。
	AuditEventEscalate = "case.escalate"
	// AuditEventReview 人工终审完成。
	AuditEventReview = "case.review"
)

// 审计结果常量：Result 字段取值。
const (
	// AuditResultSuccess 成功。
	AuditResultSuccess = "success"
	// AuditResultFailed 执行失败（基础设施错误）。
	AuditResultFailed = "failed"
	// AuditResultEscalate 未执行处置、转人工。
	AuditResultEscalate = "escalate"
)

// 操作者标识常量。
const (
	// AuditOperatorSystem 系统自动执行。
	AuditOperatorSystem = "system"
)

// AuditLog 是一条审计流水，对应 moderation_audit 表的一行。
type AuditLog struct {
	// ID 主键。
	ID uint `gorm:"primaryKey" json:"id"`

	// CaseID 关联工单：建索引（常按工单翻全部流水）。
	CaseID uint `gorm:"not null;index:idx_audit_case" json:"case_id"`
	// ReportID 关联举报（便于按举报追溯）。
	ReportID uint `gorm:"not null;index:idx_audit_report" json:"report_id"`

	// Event 事件类型（见上方常量）。
	Event string `gorm:"type:varchar(30);not null" json:"event"`
	// TargetType 被处置对象类型 video/comment/user。
	TargetType string `gorm:"type:varchar(20)" json:"target_type"`
	// TargetID 被处置对象编号。
	TargetID uint `json:"target_id"`

	// Operator 操作者："system" 或 "reviewer:<账号编号>"。
	Operator string `gorm:"type:varchar(40);not null" json:"operator"`
	// Result 结果 success/failed/escalate。
	Result string `gorm:"type:varchar(20);not null" json:"result"`

	// Detail 补充说明：实际执行的动作、终审动作等。
	Detail string `gorm:"type:varchar(500)" json:"detail"`
	// ErrorMsg 失败时的错误信息。
	ErrorMsg string `gorm:"type:varchar(500)" json:"error_msg"`
	// LatencyMS 本次动作耗时（毫秒）；无计时语义时为 0。
	LatencyMS int64 `json:"latency_ms"`

	// CreatedAt 流水时刻，只追加不修改。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// AuditRepository 审计仓储，持有数据库连接。
type AuditRepository struct {
	// db 数据库连接。
	db *gorm.DB
}

// NewAuditRepository 构造函数：创建审计仓储。
//
// 参数 db：数据库连接；
// 返回值 *AuditRepository。
func NewAuditRepository(db *gorm.DB) *AuditRepository {
	// 装配返回。
	return &AuditRepository{db: db}
}

// Create 方法：追加一条审计流水。
//
// 参数：ctx、log 流水；
// 返回值 error：写入错误。
func (r *AuditRepository) Create(ctx context.Context, log *AuditLog) error {
	// INSERT 返回。
	return r.db.WithContext(ctx).Create(log).Error
}

// ListByCase 方法：查某张工单的全部流水（按时间正序）。
//
// 参数：ctx、caseID 工单编号；
// 返回值 []*AuditLog、error。
func (r *AuditRepository) ListByCase(ctx context.Context, caseID uint) ([]*AuditLog, error) {
	// out 接收。
	var out []*AuditLog
	// Find 按工单查、时间正序；返回其错误。
	if err := r.db.WithContext(ctx).
		Where("case_id = ?", caseID).
		Order("created_at asc, id asc").
		Find(&out).Error; err != nil {
		// 返回。
		return nil, err
	}
	// 返回。
	return out, nil
}
