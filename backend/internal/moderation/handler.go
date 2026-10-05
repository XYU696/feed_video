// package moderation：本文件是举报领域的处理器（Handler 层），
// 负责"提交举报"和"查我的举报"两个 HTTP 接口。
package moderation

import (
	// errors：errors.Is 判断具体哨兵错误。
	"errors"
	// jwt：从上下文取当前登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"
	// net/http：HTTP 状态码。
	"net/http"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// ReportHandler 举报处理器，持有举报服务。
type ReportHandler struct {
	// service 举报服务。
	service *ReportService
}

// NewReportHandler 构造函数：创建举报处理器。
//
// 参数 service：举报服务；
// 返回值 *ReportHandler：处理器。
func NewReportHandler(service *ReportService) *ReportHandler {
	// 注入返回。
	return &ReportHandler{service: service}
}

// CreateReport 方法：处理"提交举报"请求。
//
// 参数 c：Gin 上下文。
func (h *ReportHandler) CreateReport(c *gin.Context) {
	// req 声明请求体。
	var req CreateReportRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 请求体格式错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// reporterID 从 JWT 取当前登录账号（举报人只能是本人）；err 接收错误。
	reporterID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// report 组装举报对象：举报人来自 JWT，其余来自请求体。
	report := &Report{
		// ReporterID 举报人（不信任前端）。
		ReporterID: reporterID,
		// TargetType 对象类型。
		TargetType: req.TargetType,
		// TargetID 对象编号。
		TargetID: req.TargetID,
		// ReasonType 举报分类。
		ReasonType: req.ReasonType,
		// Detail 补充说明。
		Detail: req.Detail,
		// Status 由 Service 强制设为 pending，这里不用传。
	}

	// 调 Service 提交；err 接收错误。
	if err := h.service.SubmitReport(c.Request.Context(), report); err != nil {
		// 按错误类型回不同状态码。
		c.JSON(h.classifyStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200 返回落库后的举报（含编号、状态、时间）。
	c.JSON(http.StatusOK, report)
}

// ListMyReports 方法：处理"查我提交的举报"请求。
//
// 参数 c：Gin 上下文。
func (h *ReportHandler) ListMyReports(c *gin.Context) {
	// req 声明请求体（只有可选 limit）。
	var req ListMyReportsRequest
	// 绑定失败一般是 JSON 格式问题。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// reporterID 取登录账号；err 接收错误。
	reporterID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// reports 调 Service 查询；err 接收错误。
	reports, err := h.service.ListMyReports(c.Request.Context(), reporterID, req.Limit)
	if err != nil {
		// 查询失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// 保证返回 [] 而不是 null（一条都没有时 GORM 给 nil）。
	if reports == nil {
		// 换成空切片。
		reports = []*Report{}
	}
	// 200 返回。
	c.JSON(http.StatusOK, gin.H{"reports": reports})
}

// classifyStatus 方法：把举报领域的错误翻译成 HTTP 状态码。
//
// 参数 err：Service 返回的错误；
// 返回值 int：状态码。
func (h *ReportHandler) classifyStatus(err error) int {
	// switch 不带表达式：按条件分支。
	switch {
	// 重复举报：409 Conflict（冲突）。
	case errors.Is(err, ErrAlreadyReported):
		// 返回 409。
		return http.StatusConflict
	// 参数类错误（对象类型/分类非法、编号缺失）：400。
	case errors.Is(err, ErrInvalidTarget),
		errors.Is(err, ErrInvalidReason),
		errors.Is(err, ErrTargetIDRequired):
		// 返回 400。
		return http.StatusBadRequest
	// 其余：500。
	default:
		// 返回 500。
		return http.StatusInternalServerError
	}
}
