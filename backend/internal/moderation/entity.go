// package moderation：内容安全治理（举报 & 审核工单）领域包。
//
// 这个文件定义"举报记录"。业务理解：用户看到某个视频/评论/用户有问题，
// 提交一条举报；后台 Agent 或审核员根据这条举报去取证、判定、处置。
package moderation

// import 导入 time：举报创建时间字段要用。
import "time"

// Report 是"一条举报记录"在程序里的样子，对应 MySQL 里 reports 表的一行。
type Report struct {
	// ID 举报记录唯一编号（主键）。
	ID uint `gorm:"primaryKey" json:"id"`

	// ReporterID 举报人编号（谁举报的）。
	//
	// 这个值在 Service/Handler 里从 JWT 取，绝不信任前端传值，防止冒充别人举报。
	//
	// 索引设计：
	//   - uniqueIndex:uniq_report_reporter_target：和下面 TargetType、TargetID【同名】，
	//     三列组成联合唯一索引——同一个用户对同一个对象【最多只能举报一次】，防止刷举报。
	//   - index:idx_report_status_created,priority:2：和 Status（priority:1）组成
	//     (status, created_at) 复合索引，供后台"按状态捞出最早的举报"扫描使用。
	ReporterID uint `gorm:"not null;uniqueIndex:uniq_report_reporter_target;index:idx_report_status_created,priority:2" json:"reporter_id"`

	// TargetType 被举报对象的类型。
	//
	// 取值（见本文件下方常量）：video（视频）/ comment（评论）/ user（用户）。
	// type:varchar(20) 指定定长字符串；同样参与上面两个索引：
	// 联合唯一索引靠它区分"举报的是哪类对象"。
	TargetType string `gorm:"type:varchar(20);not null;uniqueIndex:uniq_report_reporter_target" json:"target_type"`

	// TargetID 被举报对象的编号（视频ID / 评论ID / 用户ID，具体含义看 TargetType）。
	// 参与联合唯一索引。
	TargetID uint `gorm:"not null;uniqueIndex:uniq_report_reporter_target" json:"target_id"`

	// ReasonType 举报分类（用户为什么举报）。
	// 取值见本文件下方常量：色情/暴力/诈骗/侵权/垃圾广告/其他。
	ReasonType string `gorm:"type:varchar(50);not null" json:"reason_type"`

	// Detail 举报补充说明：用户自己写的文字。
	//
	// ★ 安全要点：这是【不可信文本】。它是被举报/举报人随便写的，后面取证喂给模型时
	// 必须做 Prompt 注入隔离（用边界块包起来），不能当指令执行。
	Detail string `gorm:"type:varchar(500)" json:"detail"`

	// Status 举报单当前状态。
	//
	// 取值见本文件下方常量：
	//   pending（待处理，初始状态）/ auto_resolved（Agent 自动处理完）/
	//   human_pending（已转人工）/ closed（已关闭）。
	//
	// default:pending 新建时默认待处理；index:idx_report_status_created,priority:1
	// 是 (status, created_at) 复合索引的前导列。
	Status string `gorm:"type:varchar(20);not null;default:pending;index:idx_report_status_created,priority:1" json:"status"`

	// CaseID 关联后续生成的审核工单 moderation_cases 的编号。
	//
	// 用 *uint 指针类型：【没有工单时为 nil】（数据库里存 NULL），有工单时指向其编号。
	// D1 阶段还没建工单，所以一直为空；后面 Agent 阶段才回填。
	CaseID *uint `gorm:"index" json:"case_id"`

	// CreatedAt 举报创建时间，GORM 自动写入（autoCreateTime）。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// 下面这一组常量统一定义"对象类型"，避免在代码里到处写裸字符串（写错一个字母难排查）。
const (
	// TargetVideo 被举报对象是视频。
	TargetVideo = "video"
	// TargetComment 被举报对象是评论。
	TargetComment = "comment"
	// TargetUser 被举报对象是用户。
	TargetUser = "user"
)

// 举报分类常量：ReasonType 的合法取值。
const (
	// ReasonPorn 色情低俗。
	ReasonPorn = "porn"
	// ReasonViolence 暴力血腥。
	ReasonViolence = "violence"
	// ReasonFraud 诈骗。
	ReasonFraud = "fraud"
	// ReasonInfringement 侵权（盗用/版权）。
	ReasonInfringement = "infringement"
	// ReasonSpam 垃圾广告。
	ReasonSpam = "spam"
	// ReasonOther 其他。
	ReasonOther = "other"
)

// 举报状态常量：Status 的合法取值。
const (
	// StatusPending 待处理（初始）。
	StatusPending = "pending"
	// StatusAutoResolved Agent 已自动处理完。
	StatusAutoResolved = "auto_resolved"
	// StatusHumanPending 已转人工，等待审核员。
	StatusHumanPending = "human_pending"
	// StatusClosed 已关闭（终态）。
	StatusClosed = "closed"
)

// CreateReportRequest 是"提交举报"接口接收的请求体。
//
// 举报人身份（我是谁）从 JWT 取，所以请求体里【不需要】也【不允许】传 reporter_id。
type CreateReportRequest struct {
	// TargetType 举报对象类型（video/comment/user）。
	TargetType string `json:"target_type"`
	// TargetID 举报对象编号。
	TargetID uint `json:"target_id"`
	// ReasonType 举报分类。
	ReasonType string `json:"reason_type"`
	// Detail 补充说明（可选）。
	Detail string `json:"detail"`
}

// ListMyReportsRequest 是"查我提交的举报"接口接收的请求体。
// 当前只有一个可选字段，预留给以后做分页。
type ListMyReportsRequest struct {
	// Limit 返回条数，Service/Handler 里会做兜底。
	Limit int `json:"limit"`
}
