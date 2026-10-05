// package moderation：本文件是审核工单的仓储层，
// 封装对 moderation_cases 表的插入和查询。
package moderation

// import 导入 context：方法第一个参数。
import "context"
// time：MarkReviewed 写审核时刻。
import "time"

// gorm：ORM，用到 gorm.ErrRecordNotFound 判断"工单不存在"。
import "gorm.io/gorm"

// CaseRepository 审核工单仓储，持有数据库连接。
type CaseRepository struct {
	// db 数据库连接。
	db *gorm.DB
}

// NewCaseRepository 构造函数：创建工单仓储。
//
// 参数 db：数据库连接；
// 返回值 *CaseRepository：仓储。
func NewCaseRepository(db *gorm.DB) *CaseRepository {
	// 装配返回。
	return &CaseRepository{db: db}
}

// Create 方法：向 moderation_cases 表插入一张工单。
//
// 参数 c：工单对象；
// 返回值 error：错误。
func (r *CaseRepository) Create(ctx context.Context, c *Case) error {
	// INSERT，返回其错误。
	return r.db.WithContext(ctx).Create(c).Error
}

// UpdateDecision 方法：把 Agent 得到的决策写回工单。
//
// 参数：caseID 工单编号、decision 决策、modelInfo 模型名；
// 返回值 error：错误。
//
// D3 只回写决策本身（decision/confidence/reason/policy_hit）和模型名，
// 处置动作、证据快照等在后面阶段补。
func (r *CaseRepository) UpdateDecision(ctx context.Context, caseID uint, d DecisionView, modelInfo string) error {
	// fields 用 map 显式列出要更新的列（避免 GORM 用零值跳过）。
	fields := map[string]interface{}{
		// decision。
		"decision": d.Decision,
		// confidence。
		"confidence": d.Confidence,
		// reason。
		"reason": d.Reason,
		// policy_hit。
		"policy_hit": d.PolicyHit,
		// model_info。
		"model_info": modelInfo,
	}
	// Model 指定表、Where 定位、Updates 批量更新，返回其错误。
	return r.db.WithContext(ctx).
		// 指定表。
		Model(&Case{}).
		// 定位工单。
		Where("id = ?", caseID).
		// 更新多列。
		Updates(fields).Error
}

// SaveEvidenceSnapshot 方法：把取证结果的 JSON 快照存进工单。
//
// 参数：caseID 工单编号、snapshot 证据 JSON 文本（partial 标志已包含在 JSON 内）；
// 返回值 error：错误。
//
// 快照的意义：模型判了什么、当时看到了哪些证据，事后要能完整复盘；
// 即使对象后来被删除/修改，快照里仍保留取证那一刻的样子。
func (r *CaseRepository) SaveEvidenceSnapshot(ctx context.Context, caseID uint, snapshot string) error {
	// fields 显式列出要更新的列。
	fields := map[string]any{
		// evidence_snapshot。
		"evidence_snapshot": snapshot,
	}
	// Model/Where/Updates，返回其错误。
	return r.db.WithContext(ctx).Model(&Case{}).Where("id = ?", caseID).Updates(fields).Error
}

// MarkAutoExecuted 方法：自动处置完成后关单——
// 工单状态置为 auto_executed，并把实际动作写进 executed_action 留痕。
//
// 参数：caseID 工单编号、action 实际动作标识（safe/remove:video/warn:user:7 等）；
// 返回值 error：更新错误。
func (r *CaseRepository) MarkAutoExecuted(ctx context.Context, caseID uint, action string) error {
	// fields 显式列出要更新的列（status 是默认列，必须靠 map 强制更新）。
	fields := map[string]any{
		// status 自动处置完成。
		"status": CaseStatusAutoExecuted,
		// executed_action 实际动作清单。
		"executed_action": action,
	}
	// Model/Where/Updates，返回其错误。
	return r.db.WithContext(ctx).Model(&Case{}).Where("id = ?", caseID).Updates(fields).Error
}

// MarkInjectionSignals 方法：在工单上记录"疑似 Prompt 注入"的检测结果（D8）。
//
// 参数：caseID 工单编号、signals 命中的可疑特征（逗号分隔）；
// 返回值 error：更新错误。
func (r *CaseRepository) MarkInjectionSignals(ctx context.Context, caseID uint, signals string) error {
	// fields 用 map 强制写入布尔 true（否则 GORM 会把 false 当零值跳过）。
	fields := map[string]any{
		// injection_suspected 置 true。
		"injection_suspected": true,
		// injection_signals 写特征。
		"injection_signals": signals,
	}
	// 更新返回。
	return r.db.WithContext(ctx).Model(&Case{}).Where("id = ?", caseID).Updates(fields).Error
}

// DecisionView 是"决策回写"所需的最小视图。
//
// 为什么单独定义一个，而不是让 moderation 直接依赖 agent 包？
// 分层上 worker/agent 可以依赖 moderation，但 moderation 不应反向依赖 agent；
// Worker 负责把 agent.Decision 转成这个中立结构再传进来。
type DecisionView struct {
	// Decision 判定。
	Decision string
	// Confidence 置信度。
	Confidence float64
	// Reason 理由。
	Reason string
	// PolicyHit 命中条款。
	PolicyHit string
}

// GetByID 方法：按主键查工单（审核台打开工单时用）。
//
// 参数 id：工单编号；
// 返回值：*Case、error；查不到时返回 gorm.ErrRecordNotFound。
func (r *CaseRepository) GetByID(ctx context.Context, id uint) (*Case, error) {
	// c 接收结果。
	var c Case
	// First 按主键取第一条；err。
	err := r.db.WithContext(ctx).First(&c, id).Error
	// 返回。
	return &c, err
}

// ListCases 方法：审核台工单列表，支持按状态过滤和分页。
//
// 排序：待人工（pending）排最前——审核员要先看到还没处理的；
// 同状态内按创建时间倒序，新的在前。
//
// 参数：status 状态过滤（空串=全部）、offset 跳过条数、limit 每页条数；
// 返回值：[]*Case 工单、int64 满足过滤条件的总数（前端算页数）、error。
func (r *CaseRepository) ListCases(ctx context.Context, status string, offset, limit int) ([]*Case, int64, error) {
	// query 先取出带过滤的链式查询，后面 Count 和 Find 复用同一套条件。
	query := r.db.WithContext(ctx).Model(&Case{})
	// status 非空：加过滤条件。
	if status != "" {
		// Where。
		query = query.Where("status = ?", status)
	}
	// total 满足条件的总数；err。
	var total int64
	// Count 计数。
	if err := query.Count(&total).Error; err != nil {
		// 失败返回。
		return nil, 0, err
	}
	// cases 接收当前页结果。
	var cases []*Case
	// Order 排序（FIELD 函数让 pending 排第一）+ Offset/Limit 分页 + Find。
	// 排序里的 'pending' 是代码内常量，不接受用户传入，无注入风险。
	if err := query.
		// pending 优先，其余按创建时间倒序。
		Order("FIELD(status, 'pending'), created_at desc").
		// 跳过前 offset 条。
		Offset(offset).
		// 取 limit 条。
		Limit(limit).
		// 执行。
		Find(&cases).Error; err != nil {
		// 失败返回。
		return nil, 0, err
	}
	// 返回工单页和总数。
	return cases, total, nil
}

// MarkReviewed 方法：人工终审后回写工单。
//
// 参数：caseID 工单编号、status 终审状态（approved/rejected/overridden）、
// finalDecision 最终动作、reviewerID 审核员编号、action 实际执行动作（可为空）；
// 返回值 error：更新错误。
func (r *CaseRepository) MarkReviewed(ctx context.Context, caseID uint, status, finalDecision string, reviewerID uint, action string) error {
	// fields 显式列出要更新的列。
	fields := map[string]any{
		// status 终审状态。
		"status": status,
		// final_decision 最终动作。
		"final_decision": finalDecision,
		// reviewer_id 审核员。
		"reviewer_id": reviewerID,
		// reviewed_at 审核时刻。
		"reviewed_at": time.Now(),
		// executed_action 实际动作。
		"executed_action": action,
	}
	// Model/Where/Updates，返回其错误。
	return r.db.WithContext(ctx).Model(&Case{}).Where("id = ?", caseID).Updates(fields).Error
}

// GetByReportID 方法：按来源举报编号查工单（用于消费幂等）。
//
// 参数 reportID：举报编号；
// 返回值：*Case 工单、error；查不到时返回 gorm.ErrRecordNotFound。
func (r *CaseRepository) GetByReportID(ctx context.Context, reportID uint) (*Case, error) {
	// c 准备接收工单。
	var c Case
	// 按 report_id 查第一条；err 接收错误。
	err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Case{}).
		// 定位来源举报。
		Where("report_id = ?", reportID).
		// 取一条。
		First(&c).Error
	// 返回工单和错误（不存在时 err 为 gorm.ErrRecordNotFound）。
	return &c, err
}
