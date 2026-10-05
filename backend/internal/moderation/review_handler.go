// package moderation：本文件是人工审核台的处理器（D7）。
//
// 两个接口：
//   - List：POST /moderation/list，工单列表（审核员）；
//   - Review：POST /moderation/review，人工终审。
//
// 审核员身份（是不是配置里的审核员账号）由路由层的中间件保证，
// 本处理器只负责取登录编号、绑参数、调服务、翻译错误码。
package moderation

// import 导入：
import (
	// errors：errors.Is 匹配哨兵错误。
	"errors"
	// io：空请求体报 io.EOF，列表接口允许空 body。
	"io"
	// jwt：从上下文取审核员账号编号。
	"feedsystem_video_go/internal/middleware/jwt"
	// net/http：HTTP 状态码。
	"net/http"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// ReviewHandler 审核台处理器。
type ReviewHandler struct {
	// service 审核服务。
	service *ReviewService
}

// NewReviewHandler 构造函数：创建审核台处理器。
//
// 参数 service：审核服务；
// 返回值 *ReviewHandler。
func NewReviewHandler(service *ReviewService) *ReviewHandler {
	// 装配返回。
	return &ReviewHandler{service: service}
}

// List 方法：处理"工单列表"请求。
//
// 参数 c：Gin 上下文。
func (h *ReviewHandler) List(c *gin.Context) {
	// req 接收过滤/分页参数。
	var req ListCasesRequest
	// 空请求体允许（查全部、默认分页）；EOF 不当错误。
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		// 其他 JSON 解析错误：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// cases 当前页、total 总数、err。
	cases, total, err := h.service.ListCases(c.Request.Context(), req)
	// 查询失败：参数类 400，其余 500。
	if err != nil {
		// 返回对应状态码。
		c.JSON(h.reviewStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// nil 转空切片，保证前端收到 []。
	if cases == nil {
		// 空切片。
		cases = []*Case{}
	}
	// 200：当前页 + 总数（前端据此算总页数）。
	c.JSON(http.StatusOK, gin.H{"cases": cases, "total": total})
}

// Review 方法：处理"人工终审"请求。
//
// 参数 c：Gin 上下文。
func (h *ReviewHandler) Review(c *gin.Context) {
	// req 接收终审参数。
	var req ReviewRequest
	// 绑定 JSON。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// reviewerID 取当前登录的审核员编号；err。
	reviewerID, err := jwt.GetAccountID(c)
	// 取不到（中间件配置异常）。
	if err != nil {
		// 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// updated 终审后的工单、err。
	updated, err := h.service.Review(c.Request.Context(), reviewerID, req)
	// 失败：翻译状态码。
	if err != nil {
		// 返回。
		c.JSON(h.reviewStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 200 返回终审后的完整工单。
	c.JSON(http.StatusOK, updated)
}

// reviewStatus 方法：把审核台错误翻译成 HTTP 状态码。
//
// 参数 err：服务返回的错误；
// 返回值 int：状态码。
func (h *ReviewHandler) reviewStatus(err error) int {
	// switch 按错误类型。
	switch {
	// 工单不存在：404。
	case errors.Is(err, ErrCaseNotFound):
		// 返回。
		return http.StatusNotFound
	// 工单已终审：409 Conflict（防止重复审核）。
	case errors.Is(err, ErrCaseClosed):
		// 返回。
		return http.StatusConflict
	// 参数类错误（动作/最终决策非法、过滤状态非法）：400。
	case errors.Is(err, ErrInvalidReviewAction),
		errors.Is(err, ErrInvalidFinalDecision):
		// 返回。
		return http.StatusBadRequest
	// 执行器未配置：503 Service Unavailable（服务端能力缺失，不是请求的错）。
	case errors.Is(err, ErrExecutorUnavailable):
		// 返回。
		return http.StatusServiceUnavailable
	// 其余：500。
	default:
		// 返回。
		return http.StatusInternalServerError
	}
}
