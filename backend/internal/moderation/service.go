// package moderation：本文件是举报领域的服务层，
// 负责举报参数校验、识别重复举报，并编排举报的落库。
package moderation

import (
	// context：请求上下文。
	"context"
	// errors：定义业务哨兵错误、errors.As 识别 MySQL 错误。
	"errors"
	// rabbitmq：举报事件发布器（落库后发 MQ）。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// log：MQ 发布失败只记日志。
	"log"
	// time：组装事件发生时刻。
	"time"

	// mysql：用其 MySQLError.Number 判断 1062 唯一键冲突。
	"github.com/go-sql-driver/mysql"
)

// 下面是几个"哨兵错误"：Service 用它们表达明确的业务含义，
// Handler/ apierror 据此归类成不同的 HTTP 状态码。
var (
	// ErrInvalidTarget 举报对象类型非法（不是 video/comment/user）。
	ErrInvalidTarget = errors.New("invalid target_type")
	// ErrInvalidReason 举报分类非法（不在允许的分类里）。
	ErrInvalidReason = errors.New("invalid reason_type")
	// ErrTargetIDRequired 对象编号缺失。
	ErrTargetIDRequired = errors.New("target_id is required")
	// ErrAlreadyReported 该用户已经举报过这个对象（唯一索引冲突）。
	ErrAlreadyReported = errors.New("you have already reported this target")
)

// ReportService 举报服务层。
type ReportService struct {
	// repo 举报仓储。
	repo *ReportRepository
	// reportMQ 举报事件发布器（落库后发 MQ），可能为 nil。
	reportMQ *rabbitmq.ReportMQ
}

// NewReportService 构造函数：创建举报服务。
//
// 参数：repo 举报仓储、reportMQ 举报事件发布器；
// 返回值 *ReportService：服务。
func NewReportService(repo *ReportRepository, reportMQ *rabbitmq.ReportMQ) *ReportService {
	// 装配返回。
	return &ReportService{repo: repo, reportMQ: reportMQ}
}

// isDupKey 普通函数：判断一个错误是不是 MySQL 1062 唯一键冲突。
//
// 参数 err：待判断错误；
// 返回值 bool：是 1062 返回 true。
//
// 技术点：errors.As 会沿着错误包装链找到 *mysql.MySQLError；
// 和点赞/关注里识别重复键是同一个写法。
func isDupKey(err error) bool {
	// me 接收 MySQL 驱动错误。
	var me *mysql.MySQLError
	// 错误链里能提取到 MySQLError 且编号为 1062。
	return errors.As(err, &me) && me.Number == 1062
}

// SubmitReport 方法：提交一条举报的完整业务逻辑。
//
// 参数 report：已由 Handler 填好举报人编号的举报对象；
// 返回值 error：校验/落库错误。
func (s *ReportService) SubmitReport(ctx context.Context, report *Report) error {
	// 校验对象类型是否在允许范围内。
	if !validTargetTypes[report.TargetType] {
		// 非法类型。
		return ErrInvalidTarget
	}
	// 对象编号必须有效。
	if report.TargetID == 0 {
		// 缺失编号。
		return ErrTargetIDRequired
	}
	// 校验举报分类。
	if !validReasonTypes[report.ReasonType] {
		// 非法分类。
		return ErrInvalidReason
	}

	// 兜底：状态强制设为 pending，不信任调用方传入的状态。
	report.Status = StatusPending

	// 落库；err 接收错误。
	if err := s.repo.Create(ctx, report); err != nil {
		// 唯一索引冲突 = 已经举报过。
		if isDupKey(err) {
			// 转成业务错误（Handler 会归成 409）。
			return ErrAlreadyReported
		}
		// 其他数据库错误原样返回。
		return err
	}

	// 落库成功后发 MQ（参考 social：同步落库 + 同步发）。
	if s.reportMQ != nil {
		// event 组装举报事件（落库后 report.ID 已回填）。
		event := rabbitmq.ReportEvent{
			// ReportID 举报记录编号。
			ReportID: report.ID,
			// TargetType 对象类型。
			TargetType: report.TargetType,
			// TargetID 对象编号。
			TargetID: report.TargetID,
			// ReporterID 举报人。
			ReporterID: report.ReporterID,
			// ReasonType 举报分类。
			ReasonType: report.ReasonType,
			// Detail 用户补充说明（随事件带给 Worker）。
			Detail: report.Detail,
			// OccurredAt 当前 UTC 时刻。
			OccurredAt: time.Now().UTC(),
		}
		// 发布；失败只记日志——举报已在库里、已经成立，MQ 失败不影响业务。
		if err := s.reportMQ.Created(ctx, event); err != nil {
			// 记录日志。
			log.Printf("report MQ 发布失败: reportID=%d, err=%v", report.ID, err)
		}
	}
	// 成功。
	return nil
}

// ListMyReports 方法：查某举报人自己提交的举报。
//
// 参数：reporterID 举报人编号、limit 条数（非法时兜底）；
// 返回值：[]*Report、error。
func (s *ReportService) ListMyReports(ctx context.Context, reporterID uint, limit int) ([]*Report, error) {
	// 条数兜底：<=0 或太大时给默认值。
	if limit <= 0 || limit > 50 {
		// 默认 10 条。
		limit = 10
	}
	// 直接转仓储查询。
	return s.repo.ListByReporter(ctx, reporterID, limit)
}

// validTargetTypes 记录合法的举报对象类型（map 当集合用）。
var validTargetTypes = map[string]bool{
	// 视频。
	TargetVideo: true,
	// 评论。
	TargetComment: true,
	// 用户。
	TargetUser: true,
}

// validReasonTypes 记录合法的举报分类。
var validReasonTypes = map[string]bool{
	// 色情。
	ReasonPorn: true,
	// 暴力。
	ReasonViolence: true,
	// 诈骗。
	ReasonFraud: true,
	// 侵权。
	ReasonInfringement: true,
	// 垃圾广告。
	ReasonSpam: true,
	// 其他。
	ReasonOther: true,
}
