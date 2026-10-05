// package video：视频业务包。本文件是点赞处理器，
// 负责点赞、取消点赞、查询是否赞过、我的喜欢列表这四个 HTTP 接口。
package video

import (
	// apierror：错误归类成状态码。
	"feedsystem_video_go/internal/apierror"
	// jwt：从上下文取当前登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// LikeHandler 结构体是点赞处理器，持有点赞服务。
type LikeHandler struct {
	// service 点赞业务服务。
	service *LikeService
}

// NewLikeHandler 是构造函数：创建点赞处理器。
//
// 参数 service：点赞服务；
// 返回值 *LikeHandler：处理器。
func NewLikeHandler(service *LikeService) *LikeHandler {
	// 注入服务并返回指针。
	return &LikeHandler{service: service}
}

// Like 方法：处理"点赞"请求。
//
// 参数 c：Gin 上下文。
func (lh *LikeHandler) Like(c *gin.Context) {
	// req 声明点赞请求（字段 VideoID 来自 entity 定义）。
	var req LikeRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 绑定错误时结束。
		return
	}
	// 视频编号非法（<=0）。
	if req.VideoID <= 0 {
		// 400 提示必填。
		c.JSON(400, gin.H{"error": "video_id is required"})
		// 结束。
		return
	}

	// accountID 取当前登录账号编号（点赞者只能是登录者本人）；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录等：返回归类状态码（401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// like 组装点赞对象（指针），视频编号来自请求、账号编号来自 JWT。
	like := &Like{
		// VideoID 目标视频。
		VideoID: req.VideoID,
		// AccountID 点赞人。
		AccountID: accountID,
	}
	// 调 Service 点赞（内部含 MQ 优先、直写兜底）；err 接收错误。
	if err := lh.service.Like(c.Request.Context(), like); err != nil {
		// 业务失败：这里统一回 500（重复点赞/视频不存在也走这，可细分改进）。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200 提示。
	c.JSON(200, gin.H{"message": "like success"})
}

// Unlike 方法：处理"取消点赞"请求，结构与 Like 对称。
//
// 参数 c：Gin 上下文。
func (lh *LikeHandler) Unlike(c *gin.Context) {
	// req 声明请求。
	var req LikeRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
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

	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// like 组装取消点赞的定位对象。
	like := &Like{
		// VideoID 目标视频。
		VideoID: req.VideoID,
		// AccountID 取消人。
		AccountID: accountID,
	}
	// 调 Service 取消点赞；err 接收错误。
	if err := lh.service.Unlike(c.Request.Context(), like); err != nil {
		// 失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200。
	c.JSON(200, gin.H{"message": "unlike success"})
}

// IsLiked 方法：处理"我是否赞过这个视频"请求。
//
// 参数 c：Gin 上下文。
func (lh *LikeHandler) IsLiked(c *gin.Context) {
	// req 声明请求。
	var req LikeRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
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

	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// isLiked 调 Service 查询；err 接收错误。
	isLiked, err := lh.service.IsLiked(c.Request.Context(), req.VideoID, accountID)
	if err != nil {
		// 失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200 返回 {"is_liked": true/false}。
	c.JSON(200, gin.H{"is_liked": isLiked})
}

// ListMyLikedVideos 方法：处理"我的喜欢"视频列表请求。
//
// 参数 c：Gin 上下文。
func (lh *LikeHandler) ListMyLikedVideos(c *gin.Context) {
	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// videos 调 Service 查点赞过的视频；err 接收错误。
	videos, err := lh.service.ListLikedVideos(c.Request.Context(), accountID)
	if err != nil {
		// 失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// videos == nil：还没赞过任何视频。
	if videos == nil {
		// 转成空切片，让 JSON 输出 [] 而非 null。
		videos = []Video{}
	}
	// 200 返回列表。
	c.JSON(200, videos)
}
