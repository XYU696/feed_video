// package social：关注业务包。本文件是关注处理器，
// 负责关注、取关、粉丝列表、关注列表、关系统计这五个 HTTP 接口。
package social

import (
	// account：空列表时用到 account.Account 类型。
	"feedsystem_video_go/internal/account"
	// apierror：错误归类。
	"feedsystem_video_go/internal/apierror"
	// jwt：从上下文取当前登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"
	// net/http：状态码。
	"net/http"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// SocialHandler 结构体是关注处理器，持有关注服务。
type SocialHandler struct {
	// service 关注业务服务。
	service *SocialService
}

// NewSocialHandler 是构造函数：创建关注处理器。
//
// 参数 service：关注服务；
// 返回值 *SocialHandler：处理器。
func NewSocialHandler(service *SocialService) *SocialHandler {
	// 注入服务并返回指针。
	return &SocialHandler{service: service}
}

// Follow 方法：处理"关注"请求。
//
// 参数 c：Gin 上下文。
func (h *SocialHandler) Follow(c *gin.Context) {
	// req 声明关注请求结构体。
	var req FollowRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 被关注博主编号非法。
	if req.VloggerID <= 0 {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		// 结束。
		return
	}
	// FollowerID 取当前登录账号编号（粉丝只能是登录者本人，不能替别人关注）；err 接收错误。
	FollowerID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// social 组装关注关系（指针）。
	social := &Social{
		// FollowerID 粉丝（当前用户）。
		FollowerID: FollowerID,
		// VloggerID 被关注博主（请求指定）。
		VloggerID: req.VloggerID,
	}
	// 调 Service 关注；err 接收错误。
	if err := h.service.Follow(c.Request.Context(), social); err != nil {
		// 失败：返回归类状态码（关注自己/已关注等也走这）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200 提示。
	c.JSON(http.StatusOK, gin.H{"message": "followed"})
}

// Unfollow 方法：处理"取关"请求。
//
// 参数 c：Gin 上下文。
func (h *SocialHandler) Unfollow(c *gin.Context) {
	// req 声明取关请求。
	var req UnfollowRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 博主编号非法。
	if req.VloggerID <= 0 {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		// 结束。
		return
	}
	// FollowerID 取登录编号；err 接收错误。
	FollowerID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// social 组装关系定位对象。
	social := &Social{
		// FollowerID 当前用户。
		FollowerID: FollowerID,
		// VloggerID 要取关的博主。
		VloggerID: req.VloggerID,
	}
	// 调 Service 取关；err 接收错误。
	if err := h.service.Unfollow(c.Request.Context(), social); err != nil {
		// 失败：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200。
	c.JSON(http.StatusOK, gin.H{"message": "unfollowed"})
}

// GetAllFollowers 方法：处理"粉丝列表"请求。
//
// 细节：请求里指定了 vlogger_id 就查那个人的粉丝（看别人主页）；
// 没指定就默认查当前登录用户自己的粉丝。
//
// 参数 c：Gin 上下文。
func (h *SocialHandler) GetAllFollowers(c *gin.Context) {
	// req 声明请求。
	var req GetAllFollowersRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// vloggerID 默认取请求里指定的博主编号。
	vloggerID := req.VloggerID
	// 没指定（看自己的粉丝）。
	if vloggerID == 0 {
		// accountID 取当前登录编号；err 接收错误。
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			// 未登录又没指定博主：401。
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// 默认查自己的粉丝。
		vloggerID = accountID
	}

	// followers 调 Service 查粉丝列表；err 接收错误。
	followers, err := h.service.GetAllFollowers(c.Request.Context(), vloggerID)
	if err != nil {
		// 失败：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// followers == nil：保险起见转成空指针切片，JSON 输出 []。
	if followers == nil {
		// 赋空切片。
		followers = []*account.Account{}
	}
	// followerCount 同时查粉丝总数（_ 忽略错误，列表已有、计数失败不致命）。
	followerCount, _ := h.service.CountFollowers(c.Request.Context(), vloggerID)
	// 200 返回列表和粉丝总数。
	c.JSON(http.StatusOK, GetAllFollowersResponse{Followers: followers, FollowerCount: followerCount})
}

// GetAllVloggers 方法：处理"关注的博主列表"请求。
//
// 同样：请求指定 follower_id 就查那个人，没指定默认查当前登录用户自己的关注。
//
// 参数 c：Gin 上下文。
func (h *SocialHandler) GetAllVloggers(c *gin.Context) {
	// req 声明请求。
	var req GetAllVloggersRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// followerID 默认取请求指定的粉丝编号。
	followerID := req.FollowerID
	// 没指定（看自己的关注列表）。
	if followerID == 0 {
		// accountID 取登录编号；err 接收错误。
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			// 未登录：401。
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// 默认查自己。
		followerID = accountID
	}

	// vloggers 调 Service 查关注的博主；err 接收错误。
	vloggers, err := h.service.GetAllVloggers(c.Request.Context(), followerID)
	if err != nil {
		// 失败：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// nil → 空切片。
	if vloggers == nil {
		// 赋空切片。
		vloggers = []*account.Account{}
	}
	// vloggerCount 查关注总数。
	vloggerCount, _ := h.service.CountVloggers(c.Request.Context(), followerID)
	// 200 返回列表和关注数。
	c.JSON(http.StatusOK, GetAllVloggersResponse{Vloggers: vloggers, VloggerCount: vloggerCount})
}

// GetCounts 方法：处理"关系统计"请求，返回当前用户的粉丝数和关注数。
//
// 参数 c：Gin 上下文。
func (h *SocialHandler) GetCounts(c *gin.Context) {
	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// followerCount 粉丝数（_ 忽略错误）。
	followerCount, _ := h.service.CountFollowers(c.Request.Context(), accountID)
	// vloggerCount 关注数（_ 忽略错误）。
	vloggerCount, _ := h.service.CountVloggers(c.Request.Context(), accountID)
	// 200 返回两个计数。
	c.JSON(http.StatusOK, SocialCounts{FollowerCount: followerCount, VloggerCount: vloggerCount})
}
