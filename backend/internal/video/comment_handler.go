// package video：视频业务包。本文件是评论处理器，
// 负责发表评论、删除评论、查视频全部评论这三个 HTTP 接口。
package video

import (
	// account：账号服务，发表评论时要用它查当前用户的准确用户名。
	"feedsystem_video_go/internal/account"
	// apierror：错误归类。
	"feedsystem_video_go/internal/apierror"
	// jwt：取登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// CommentHandler 结构体是评论处理器，持有评论服务和账号服务。
type CommentHandler struct {
	// service 评论业务服务。
	service *CommentService
	// accountService 账号服务，拿用户名。
	accountService *account.AccountService
}

// NewCommentHandler 是构造函数：创建评论处理器。
//
// 参数：service 评论服务、accountService 账号服务；
// 返回值 *CommentHandler：处理器。
func NewCommentHandler(service *CommentService, accountService *account.AccountService) *CommentHandler {
	// 注入两个服务并返回指针。
	return &CommentHandler{service: service, accountService: accountService}
}

// PublishComment 方法：处理"发表评论"请求。
//
// 与点赞 Handler 的一个区别：用户名不信任前端传，而是用账号编号反查出准确用户名再写进评论。
//
// 参数 c：Gin 上下文。
func (h *CommentHandler) PublishComment(c *gin.Context) {
	// req 声明评论请求结构体。
	var req PublishCommentRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 绑定错误时结束。
		return
	}
	// 内容为空（binding 之外的手动兜底）。
	if req.Content == "" {
		// 400。
		c.JSON(400, gin.H{"error": "content is required"})
		// 结束。
		return
	}
	// 视频编号非法。
	if req.VideoID <= 0 {
		// 400。
		c.JSON(400, gin.H{"error": "video_id is required"})
		// 结束。
		return
	}
	// authorId 取当前登录账号编号；err 接收错误。
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回归类状态码（401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// user 用账号编号反查账号，拿到准确用户名；err 接收错误。
	user, err := h.accountService.FindByID(c.Request.Context(), authorId)
	if err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// comment 组装评论对象（指针）。
	comment := &Comment{
		// Username 来自反查结果（而不是请求里随便填的名字，防冒充）。
		Username: user.Username,
		// VideoID 评论的视频。
		VideoID: req.VideoID,
		// AuthorID 评论作者编号。
		AuthorID: authorId,
		// Content 评论内容。
		Content: req.Content,
	}
	// 调 Service 发表；err 接收错误。
	if err := h.service.Publish(c.Request.Context(), comment); err != nil {
		// 失败：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200 提示。
	c.JSON(200, gin.H{"message": "comment published successfully"})
}

// DeleteComment 方法：处理"删除评论"请求（归属校验在 Service 里：只能删自己的）。
//
// 参数 c：Gin 上下文。
func (h *CommentHandler) DeleteComment(c *gin.Context) {
	// req 声明删除请求。
	var req DeleteCommentRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 评论编号非法。
	if req.CommentID <= 0 {
		// 400。
		c.JSON(400, gin.H{"error": "comment_id is required"})
		// 结束。
		return
	}
	// 调 Service 删除（它会校验评论是不是当前用户写的）；err 接收错误。
	if err := h.service.Delete(c.Request.Context(), req.CommentID, accountID); err != nil {
		// 失败：返回归类状态码（越权→401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// 成功：200 提示。
	c.JSON(200, gin.H{"message": "comment deleted successfully"})
}

// GetAllComments 方法：处理"查某视频评论列表"请求（公开接口）。
//
// 参数 c：Gin 上下文。
func (h *CommentHandler) GetAllComments(c *gin.Context) {
	// req 声明请求。
	var req GetAllCommentsRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 视频编号缺失。
	if req.VideoID == 0 {
		// 400。
		c.JSON(400, gin.H{"error": "video_id is required"})
		// 结束。
		return
	}
	// comments 调 Service 查评论；err 接收错误。
	comments, err := h.service.GetAll(c.Request.Context(), req.VideoID)
	if err != nil {
		// 失败：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// comments == nil：该视频还没人评论。
	if comments == nil {
		// 转空切片，JSON 输出 [] 而非 null。
		comments = []Comment{}
	}
	// 200 返回评论列表。
	c.JSON(200, comments)
}
