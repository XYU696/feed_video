// package moderation：本文件是举报领域的仓储层，
// 封装对 reports 表的插入和查询。
package moderation

// import 导入 context：每个方法第一个参数都是上下文。
import "context"

// gorm：ORM。
import "gorm.io/gorm"

// ReportRepository 举报仓储，持有数据库连接。
type ReportRepository struct {
	// db 数据库连接（包私有）。
	db *gorm.DB
}

// NewReportRepository 构造函数：创建举报仓储。
//
// 参数 db：数据库连接；
// 返回值 *ReportRepository：仓储。
func NewReportRepository(db *gorm.DB) *ReportRepository {
	// 初始化并返回指针。
	return &ReportRepository{db: db}
}

// Create 方法：向 reports 表插入一条举报。
//
// 参数 report：举报对象；
// 返回值 error：错误。
//
// 注意：如果 (举报人, 对象类型, 对象编号) 已经举报过，会触发联合唯一索引冲突，
// MySQL 报 1062 错误——由上层 Service 识别成"你已经举报过了"。
func (r *ReportRepository) Create(ctx context.Context, report *Report) error {
	// .Create 执行 INSERT，直接返回其 Error。
	return r.db.WithContext(ctx).Create(report).Error
}

// ListByReporter 方法：查某举报人提交过的举报记录（"我的举报"列表）。
//
// 参数：reporterID 举报人编号、limit 最多返回条数；
// 返回值：[]*Report 举报指针切片、error 错误。
func (r *ReportRepository) ListByReporter(ctx context.Context, reporterID uint, limit int) ([]*Report, error) {
	// reports 声明结果切片。
	var reports []*Report
	// 链式查询；err 接收错误。
	if err := r.db.WithContext(ctx).
		// Model 指定操作 reports 表。
		Model(&Report{}).
		// 只查当前举报人的（数据隔离，看不到别人的举报）。
		Where("reporter_id = ?", reporterID).
		// 最新举报排在前面。
		Order("created_at desc").
		// 限条数。
		Limit(limit).
		// 执行并填充。
		Find(&reports).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回列表（可能为 nil，Handler 会转成空切片保证输出 []）。
	return reports, nil
}

// CountByTarget 方法：统计某个对象被【累计举报】过多少次（举报频次，供 Agent 判断）。
//
// 参数：targetType 对象类型、targetID 对象编号；
// 返回值：int64 条数、error 错误。
func (r *ReportRepository) CountByTarget(ctx context.Context, targetType string, targetID uint) (int64, error) {
	// count 接收条数。
	var count int64
	// Model 指定表、Where 定位对象、Count 计数，返回其错误。
	err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Report{}).
		// 定位被举报对象。
		Where("target_type = ? AND target_id = ?", targetType, targetID).
		// 计数。
		Count(&count).Error
	// 返回计数和错误。
	return count, err
}

// UpdateStatusByCase 方法：按工单编号【批量】更新关联举报的状态。
//
// 参数：caseID 工单编号、status 新状态（auto_resolved / human_pending）；
// 返回值 error：更新错误。
//
// 为什么按 case_id 批量改而不是只改当前这条举报？
// 为"多条举报合并成一张工单"预留：同一张工单可能关联多条举报，
// 工单了结时指向它的举报应一次全部更新，WHERE case_id = ? 正好天然支持。
func (r *ReportRepository) UpdateStatusByCase(ctx context.Context, caseID uint, status string) error {
	// Model 指定表、Where 定位工单关联的所有举报、Update 批量改状态，返回其错误。
	return r.db.WithContext(ctx).
		// 指定表。
		Model(&Report{}).
		// 该工单关联的全部举报。
		Where("case_id = ?", caseID).
		// 更新状态。
		Update("status", status).Error
}

// ListReporterIDsByCase 方法：查某张工单关联的【全部举报人编号】（去重）。
//
// 参数：caseID 工单编号；
// 返回值 []uint：举报人编号列表；error。
//
// 用途（D10）：工单了结后给每个举报人发结果通知；多举报合并时人数可能多个。
func (r *ReportRepository) ListReporterIDsByCase(ctx context.Context, caseID uint) ([]uint, error) {
	// ids 接收。
	var ids []uint
	// Distinct 去重、Pluck 只取 reporter_id 一列；返回其错误。
	if err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Report{}).
		// 该工单关联的全部举报。
		Where("case_id = ?", caseID).
		// 举报人去重。
		Distinct().
		// 只取编号列。
		Pluck("reporter_id", &ids).Error; err != nil {
		// 返回。
		return nil, err
	}
	// 返回。
	return ids, nil
}

// AttachCaseID 方法：把生成的审核工单编号回填到举报记录上（reports.case_id）。
//
// 参数：reportID 举报编号、caseID 工单编号；
// 返回值 error：错误。
func (r *ReportRepository) AttachCaseID(ctx context.Context, reportID uint, caseID uint) error {
	// Model(Report) 指定 reports 表；Where 定位该举报；Update 更新单列。
	return r.db.WithContext(ctx).
		// 指定表。
		Model(&Report{}).
		// 定位。
		Where("id = ?", reportID).
		// 更新 case_id。
		Update("case_id", caseID).Error
}
