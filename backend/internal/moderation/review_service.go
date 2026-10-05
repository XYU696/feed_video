// package moderation：本文件是人工审核台的服务层（D7）。
//
// 负责两件事：
//   - ListCases：给审核员看工单列表（pending 优先 + 状态过滤 + 分页）；
//   - Review：人工终审 approve/reject/override，需要处置时通过
//     ReviewActionExecutor 接口执行（实现在 agent 包，带 Redis 锁）。
//
// 终审完成后：工单写终审状态、关联举报全部关闭。
package moderation

// import 导入：
import (
	// context：方法第一个参数。
	"context"
	// errors：判断"工单不存在"。
	"errors"
	// fmt：拼处置失败的错误信息。
	"fmt"
	// log：审计/通知 best-effort 失败时记录。
	"log"

	// gorm：gorm.ErrRecordNotFound。
	"gorm.io/gorm"
)

// listDefaultPageSize 列表默认每页条数。
const listDefaultPageSize = 20

// listMaxPageSize 列表每页最大条数。
const listMaxPageSize = 100

// validCaseStatuses 是工单状态的白名单，用来校验列表过滤参数。
var validCaseStatuses = map[string]bool{
	// pending。
	CaseStatusPending: true,
	// approved。
	CaseStatusApproved: true,
	// rejected。
	CaseStatusRejected: true,
	// overridden。
	CaseStatusOverridden: true,
	// auto_executed。
	CaseStatusAutoExecuted: true,
}

// ReviewService 审核台服务。
type ReviewService struct {
	// caseRepo 工单仓储。
	caseRepo *CaseRepository
	// reportRepo 举报仓储：终审后关闭关联举报。
	reportRepo *ReportRepository
	// executor 处置执行器；为 nil 时 approve/override 的处置动作无法执行。
	executor ReviewActionExecutor
	// auditRepo 审计仓储（D10）；为 nil 时跳过审计流水。
	auditRepo *AuditRepository
	// notifier 举报人结果通知（D10）；为 nil 时跳过通知。
	notifier ResultNotifier
}

// NewReviewService 构造函数：创建审核台服务。
//
// 参数：caseRepo、reportRepo、executor（可为 nil）、
// auditRepo 审计仓储（可 nil）、notifier 结果通知（可 nil）；
// 返回值 *ReviewService。
func NewReviewService(caseRepo *CaseRepository, reportRepo *ReportRepository, executor ReviewActionExecutor,
	auditRepo *AuditRepository, notifier ResultNotifier) *ReviewService {
	// 装配返回。
	return &ReviewService{
		caseRepo: caseRepo, reportRepo: reportRepo, executor: executor,
		auditRepo: auditRepo, notifier: notifier,
	}
}

// ListCases 方法：查询工单列表。
//
// 参数：ctx、req 过滤/分页请求；
// 返回值：[]*Case 当前页工单、int64 总条数、error。
func (s *ReviewService) ListCases(ctx context.Context, req ListCasesRequest) ([]*Case, int64, error) {
	// status 非空时必须在白名单里，否则是非法参数。
	if req.Status != "" && !validCaseStatuses[req.Status] {
		// 返回。
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidReviewAction, req.Status)
	}
	// page 兜底：<1 当第 1 页。
	page := req.Page
	// 小于 1。
	if page < 1 {
		// 置 1。
		page = 1
	}
	// pageSize 兜底。
	pageSize := req.PageSize
	// 空或负：默认值。
	if pageSize < 1 {
		// 默认。
		pageSize = listDefaultPageSize
	}
	// 超出上限：压到上限。
	if pageSize > listMaxPageSize {
		// 压回。
		pageSize = listMaxPageSize
	}
	// offset 计算：跳过前几页的条数。
	offset := (page - 1) * pageSize
	// 调仓储查询，返回其结果。
	return s.caseRepo.ListCases(ctx, req.Status, offset, pageSize)
}

// Review 方法：执行一次人工终审。
//
// 参数：ctx、reviewerID 审核员编号、req 终审请求；
// 返回值：*Case 终审后的工单、error。
func (s *ReviewService) Review(ctx context.Context, reviewerID uint, req ReviewRequest) (*Case, error) {
	// 工单编号必填。
	if req.CaseID == 0 {
		// 返回。
		return nil, ErrCaseNotFound
	}
	// action 必须在三种合法取值里。
	if req.Action != ReviewApprove && req.Action != ReviewReject && req.Action != ReviewOverride {
		// 返回。
		return nil, ErrInvalidReviewAction
	}

	// c 查出工单；err。
	c, err := s.caseRepo.GetByID(ctx, req.CaseID)
	// 查询出错。
	if err != nil {
		// 不存在：翻译成哨兵，其余错误原样返回。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 返回。
			return nil, ErrCaseNotFound
		}
		// 返回。
		return nil, err
	}
	// 只有 pending 工单能终审，防止两个人同时审、重复处置。
	if c.Status != CaseStatusPending {
		// 返回。
		return nil, ErrCaseClosed
	}

	// caseStatus 终审后的工单状态、finalDecision 最终动作、executedAction 实际动作留痕。
	var caseStatus, finalDecision, executedAction string

	// 按审核动作决定终审结果。
	switch req.Action {
	// 驳回：审核员认为内容没问题——不做任何处置，最终动作 safe。
	case ReviewReject:
		// 工单状态 rejected。
		caseStatus = CaseStatusRejected
		// 最终动作 safe。
		finalDecision = DecisionSafe

	// 同意：按 Agent 给出的决策执行。
	case ReviewApprove:
		// status 记 approved；最终动作 = Agent 决策。
		caseStatus = CaseStatusApproved
		// 最终决策。
		finalDecision = c.Decision
		// action 按 Agent 决策执行；返回实际动作留痕。
		action, err := s.runDecision(ctx, c)
		// 执行失败/决策无法执行：直接返回（工单保持 pending，可重试或改判）。
		if err != nil {
			// 返回。
			return nil, err
		}
		// 记录实际动作。
		executedAction = action

	// 改判：审核员自己指定最终动作。
	case ReviewOverride:
		// 工单状态 overridden。
		caseStatus = CaseStatusOverridden
		// 只允许改判成 safe/remove/warn（ban 二期、escalate 不是终审动作）。
		if req.FinalDecision != DecisionSafe &&
			req.FinalDecision != DecisionRemove &&
			req.FinalDecision != DecisionWarn {
			// 返回。
			return nil, ErrInvalidFinalDecision
		}
		// 最终动作取审核员指定值。
		finalDecision = req.FinalDecision
		// 非 safe：执行审核员指定的动作。
		if finalDecision != DecisionSafe {
			// 复用执行逻辑：临时把工单决策替换成审核员指定的。
			c.Decision = finalDecision
			// 执行。
			action, err := s.runDecision(ctx, c)
			// 失败：返回，工单保持 pending。
			if err != nil {
				// 返回。
				return nil, err
			}
			// 留痕。
			executedAction = action
		}
	}

	// 回写工单终审结果；失败返回（处置可能已执行，但状态没写上——需要人工介入）。
	if err := s.caseRepo.MarkReviewed(ctx, c.ID, caseStatus, finalDecision, reviewerID, executedAction); err != nil {
		// 返回。
		return nil, err
	}
	// 关联举报【批量】关闭（best-effort：工单已终审，举报状态失败只返回给调用方记日志场景，
	// 这里直接返回错误也无妨——调用是同步的，前端可重试，且 MarkReviewed 已完成）。
	if err := s.reportRepo.UpdateStatusByCase(ctx, c.ID, StatusClosed); err != nil {
		// 返回。
		return nil, err
	}

	// D10：终审完成写审计流水 + 给举报人发结果通知（均 best-effort，不影响终审结果）。
	s.afterReview(ctx, c, reviewerID, req.Action, finalDecision, executedAction)

	// 重新取一次工单返回（含刚写入的终审字段）。
	return s.caseRepo.GetByID(ctx, c.ID)
}

// afterReview 方法：终审后的审计留痕与举报人通知（best-effort）。
//
// 参数：ctx、c 工单、reviewerID 审核员、reviewAction approve/reject/override、
// finalDecision 最终决策、executedAction 实际动作（空=无处置）。
func (s *ReviewService) afterReview(ctx context.Context, c *Case, reviewerID uint,
	reviewAction, finalDecision, executedAction string) {
	// auditRepo 配置了才写流水。
	if s.auditRepo != nil {
		// detail 记终审动作/最终决策/实际执行动作，便于复盘。
		detail := fmt.Sprintf("review=%s final=%s executed=%s",
			reviewAction, finalDecision, fallbackText(executedAction, "none"))
		// entry 组装审计流水。
		entry := &AuditLog{
			// CaseID。
			CaseID: c.ID,
			// ReportID 取工单来源举报（其余关联举报也指向本工单，按工单可查全）。
			ReportID: c.ReportID,
			// Event 人工终审。
			Event: AuditEventReview,
			// TargetType/ID。
			TargetType: c.TargetType,
			// TargetID。
			TargetID: c.TargetID,
			// Operator 审核员编号。
			Operator: fmt.Sprintf("reviewer:%d", reviewerID),
			// Result 成功。
			Result: AuditResultSuccess,
			// Detail。
			Detail: detail,
		}
		// 写流水；失败只记日志。
		if err := s.auditRepo.Create(ctx, entry); err != nil {
			// 记录。
			log.Printf("review service: 终审审计写入失败 caseID=%d, err=%v", c.ID, err)
		}
	}

	// notifier 配置了才通知举报人。
	if s.notifier == nil {
		// 跳过。
		return
	}
	// reporters 取该工单关联的全部举报人；err。
	reporters, err := s.reportRepo.ListReporterIDsByCase(ctx, c.ID)
	// 查询失败只记日志（通知不能阻断终审返回）。
	if err != nil {
		// 记录。
		log.Printf("review service: 查询举报人失败 caseID=%d, err=%v", c.ID, err)
		// 返回。
		return
	}
	// content 结果文案：按最终决策给举报人一个明确说法。
	content := reviewResultContent(c, reviewAction, finalDecision, executedAction)
	// 逐个发送（人数通常很少）。
	for _, rid := range reporters {
		// 发送；失败只记日志。
		if err := s.notifier.NotifyResult(ctx, rid, c.ID, content); err != nil {
			// 记录。
			log.Printf("review service: 举报人通知失败 caseID=%d reporter=%d, err=%v", c.ID, rid, err)
		}
	}
}

// reviewResultContent 普通函数：生成给举报人的结果文案。
//
// 参数：c 工单、reviewAction 终审动作、finalDecision 最终决策、executedAction 实际动作；
// 返回值 string：文案。
func reviewResultContent(c *Case, reviewAction, finalDecision, executedAction string) string {
	// 最终决策 safe（reject 或 override→safe）：不予处置。
	if finalDecision == DecisionSafe {
		// 文案。
		return fmt.Sprintf("您举报的 %s #%d（工单 #%d）经人工复核，未发现违规，不予处置。",
			c.TargetType, c.TargetID, c.ID)
	}
	// 有处置动作：告知已处理及具体动作。
	return fmt.Sprintf("您举报的 %s #%d（工单 #%d）经人工审核已处理：%s。",
		c.TargetType, c.TargetID, c.ID, fallbackText(executedAction, finalDecision))
}

// fallbackText 普通函数：文本为空时返回兜底值。
func fallbackText(s, fallback string) string {
	// 空。
	if s == "" {
		// 兜底。
		return fallback
	}
	// 原值。
	return s
}

// runDecision 方法：执行工单上记录的决策（approve 时是 Agent 决策，
// override 时已被替换成审核员指定决策）。
//
// 参数：ctx、c 工单（TargetType/TargetID/Decision/PolicyHit）；
// 返回值：string 实际动作标识（remove:video / remove:comment / warn:user:<id> / safe）、
// error（执行器缺失、决策不可执行、执行故障）。
func (s *ReviewService) runDecision(ctx context.Context, c *Case) (string, error) {
	// executor 没配置：无法处置。
	if s.executor == nil {
		// 返回。
		return "", ErrExecutorUnavailable
	}
	// 按决策分发。
	switch c.Decision {
	// safe：没有动作要执行。
	case DecisionSafe:
		// 返回 safe。
		return DecisionSafe, nil
	// remove：按对象类型删视频或评论。
	case DecisionRemove:
		// 视频。
		if c.TargetType == TargetVideo {
			// HumanRemoveVideo 执行（内部带锁）；返回其错误。
			if err := s.executor.HumanRemoveVideo(ctx, c.TargetID); err != nil {
				// 包装返回，说明是处置失败。
				return "", fmt.Errorf("处置执行失败: %w", err)
			}
			// 动作标识。
			return "remove:video", nil
		}
		// 评论。
		if c.TargetType == TargetComment {
			// HumanRemoveComment。
			if err := s.executor.HumanRemoveComment(ctx, c.TargetID); err != nil {
				// 返回。
				return "", fmt.Errorf("处置执行失败: %w", err)
			}
			// 动作标识。
			return "remove:comment", nil
		}
		// remove 与对象类型不匹配（如 user）。
		return "", ErrInvalidFinalDecision
	// warn：解析作者并发警告私信。
	case DecisionWarn:
		// authorID 实时查对象作者；err。
		authorID, err := s.executor.ResolveTargetAuthorID(ctx, c.TargetType, c.TargetID)
		// 查询失败。
		if err != nil {
			// 返回。
			return "", fmt.Errorf("处置执行失败: %w", err)
		}
		// HumanWarn 发警告（锁加在对象上）。
		if err := s.executor.HumanWarn(ctx, authorID, c.TargetType, c.TargetID, c.PolicyHit); err != nil {
			// 返回。
			return "", fmt.Errorf("处置执行失败: %w", err)
		}
		// 动作标识。
		return fmt.Sprintf("warn:user:%d", authorID), nil
	// escalate/空/ban：没有可自动执行的动作——
	// approve 一个"转人工"建议没有意义，请审核员用 override 明确指定。
	default:
		// 返回。
		return "", fmt.Errorf("该工单建议为 %q，无法 approve，请使用 override 指定最终动作", c.Decision)
	}
}
