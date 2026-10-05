// package feed：信息流业务包。本文件是 Handler 层，
// 负责接收四种榜单（最新/点赞/关注/热度）+ 标签查询的 HTTP 请求，
// 做参数兜底处理后调用 Service，并保证返回的列表永远是 [] 而不是 null。
package feed

import (
	// apierror：错误归类成状态码。
	"feedsystem_video_go/internal/apierror"
	// jwt：软登录下尝试取账号 ID（游客也能看 Feed）。
	"feedsystem_video_go/internal/middleware/jwt"
	// time：把请求里的秒/毫秒时间戳转成 time.Time。
	"time"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// FeedHandler 结构体：信息流处理器，持有服务。
type FeedHandler struct {
	// service 信息流服务。
	service *FeedService
}

// NewFeedHandler 是构造函数：创建处理器。
//
// 参数 service：信息流服务；
// 返回值 *FeedHandler：处理器。
func NewFeedHandler(service *FeedService) *FeedHandler {
	// 注入并返回。
	return &FeedHandler{service: service}
}

// ListLatest 方法：处理"最新视频流"请求。
//
// 参数 c：Gin 上下文。
func (f *FeedHandler) ListLatest(c *gin.Context) {
	// req 声明请求参数（条数、游标时间）。
	var req ListLatestRequest
	// 解析 JSON；err 错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：归类状态码返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 条数非法（<=0 或 >50）。
	if req.Limit <= 0 || req.Limit > 50 {
		// 兜底成默认 10 条，防止前端乱传拖垮 DB。
		req.Limit = 10
	}
	// latestTime 游标时间，默认零值（表示第一页）。
	var latestTime time.Time
	// 请求带了毫秒时间戳。
	if req.LatestTime > 0 {
		// UnixMilli 把毫秒时间戳转成 time.Time。
		latestTime = time.UnixMilli(req.LatestTime)
	}
	// viewerAccountID 尝试取登录账号；err 错误（这个接口允许游客）。
	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 没登录：置 0，Service 当作游客处理。
		viewerAccountID = 0
	}
	// feedItems 调服务查最新流；err 错误。
	feedItems, err := f.service.ListLatest(c.Request.Context(), req.Limit, latestTime, viewerAccountID)
	if err != nil {
		// 服务端错误：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 把可能为 nil 的视频列表换成空切片。
	feedItems.VideoList = nonNilFeedVideoItems(feedItems.VideoList)
	// 200 返回。
	c.JSON(200, feedItems)
}

// ListLikesCount 方法：处理"点赞榜"请求。
//
// 参数 c：Gin 上下文。
func (f *FeedHandler) ListLikesCount(c *gin.Context) {
	// req 声明请求（条数 + 两个游标字段）。
	var req ListLikesCountRequest
	// 解析 JSON；err 错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 条数兜底。
	if req.Limit <= 0 || req.Limit > 50 {
		// 默认 10。
		req.Limit = 10
	}

	// cursor 复合游标，默认 nil（第一页）。
	var cursor *LikesCountCursor
	// 两个游标字段至少传了一个。
	if req.LikesCountBefore != nil || req.IDBefore != nil {
		// 只传了其中一个：游标必须成对出现，否则没法定位。
		if req.LikesCountBefore == nil || req.IDBefore == nil {
			// 400。
			c.JSON(400, gin.H{"error": "likes_count_before and id_before must be provided together"})
			// 结束。
			return
		}

		// likesCountBefore 解引用拿到游标签名数。
		likesCountBefore := *req.LikesCountBefore
		// idBefore 解引用拿到游标视频 ID。
		idBefore := *req.IDBefore

		// 点赞数不能是负数。
		if likesCountBefore < 0 {
			// 400。
			c.JSON(400, gin.H{"error": "invalid cursor: likes_count_before must be >= 0"})
			// 结束。
			return
		}
		// 视频 ID 为 0 的特殊情况。
		if idBefore == 0 {
			// 只有"点赞数也为 0"才允许（相当于从头开始），否则 0 ID 是非法游标。
			if likesCountBefore != 0 {
				// 400。
				c.JSON(400, gin.H{"error": "invalid cursor: id_before must be > 0"})
				// 结束。
				return
			}
		} else {
			// 两个游标都有效：组装游标对象。
			cursor = &LikesCountCursor{
				// LikesCount 游标签名数。
				LikesCount: likesCountBefore,
				// ID 游标视频 ID。
				ID: idBefore,
			}
		}
	}
	// viewerAccountID 尝试登录态；err 错误。
	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 游客置 0。
		viewerAccountID = 0
	}
	// feedItems 调服务查点赞榜；err 错误。
	feedItems, err := f.service.ListLikesCount(c.Request.Context(), req.Limit, cursor, viewerAccountID)
	if err != nil {
		// 500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 列表 nil → 空切片。
	feedItems.VideoList = nonNilFeedVideoItems(feedItems.VideoList)
	// 200。
	c.JSON(200, feedItems)
}

// ListByFollowing 方法：处理"关注流"请求。
//
// 参数 c：Gin 上下文。
func (f *FeedHandler) ListByFollowing(c *gin.Context) {
	// req 声明请求。
	var req ListByFollowingRequest
	// 解析 JSON；err 错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 条数兜底。
	if req.Limit <= 0 || req.Limit > 50 {
		// 默认 10。
		req.Limit = 10
	}
	// viewerAccountID 取登录态；err 错误。
	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 游客置 0。
		viewerAccountID = 0
	}
	// latestTime 游标，默认零值。
	var latestTime time.Time
	// 带了秒级时间戳（注意关注流用【秒】，最新流用毫秒）。
	if req.LatestTime > 0 {
		// Unix 秒转 time.Time。
		latestTime = time.Unix(req.LatestTime, 0)
	}
	// feedItems 调服务查关注流；err 错误。
	feedItems, err := f.service.ListByFollowing(c.Request.Context(), req.Limit, latestTime, viewerAccountID)
	if err != nil {
		// 500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// nil → 空切片。
	feedItems.VideoList = nonNilFeedVideoItems(feedItems.VideoList)
	// 200。
	c.JSON(200, feedItems)
}

// ListByPopularity 方法：处理"热度榜"请求。
//
// 参数 c：Gin 上下文。
func (f *FeedHandler) ListByPopularity(c *gin.Context) {
	// req 声明请求（条数、快照时刻、偏移、复合游标等）。
	var req ListByPopularityRequest
	// 解析 JSON；err 错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 条数兜底。
	if req.Limit <= 0 || req.Limit > 50 {
		// 默认 10。
		req.Limit = 10
	}
	// viewerAccountID 登录态；err 错误。
	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 游客置 0。
		viewerAccountID = 0
	}

	// latestPopularity DB 兜底游标热度，默认 0。
	var latestPopularity int64
	// latestBefore DB 兜底游标时间，默认零值。
	var latestBefore time.Time
	// latestIDBefore DB 兜底游标 ID，默认 0。
	var latestIDBefore uint

	// 热度不能为负。
	if req.LatestPopularity < 0 {
		// 400。
		c.JSON(400, gin.H{"error": "latest_popularity must be >= 0"})
		// 结束。
		return
	}

	// anyCursor 游标字段是否传了任意一个：时间非零 或 ID 指针非空。
	anyCursor := !req.LatestBefore.IsZero() || req.LatestIDBefore != nil
	// 传了游标。
	if anyCursor {
		// 时间、ID 必须成对且 ID 有效，否则无法定位。
		if req.LatestBefore.IsZero() || req.LatestIDBefore == nil || *req.LatestIDBefore == 0 {
			// 400。
			c.JSON(400, gin.H{"error": "latest_before and latest_id_before must be provided together"})
			// 结束。
			return
		}
		// latestPopularity 取请求游标热度。
		latestPopularity = req.LatestPopularity
		// latestBefore 取请求游标时间。
		latestBefore = req.LatestBefore
		// latestIDBefore 解引用取 ID。
		latestIDBefore = *req.LatestIDBefore
	}
	// resp 调服务查热度榜，按顺序传全部参数；err 错误。
	resp, err := f.service.ListByPopularity(
		// HTTP 请求自带的 ctx。
		c.Request.Context(),
		// 条数。
		req.Limit,
		// 快照时刻（翻页时原样带回，保证同一张榜）。
		req.AsOf,
		// 快照内偏移。
		req.Offset,
		// 观看者。
		viewerAccountID,
		// 下面三个是 MySQL 复合游标。
		latestPopularity,
		latestBefore,
		latestIDBefore,
	)
	if err != nil {
		// 500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// nil → 空切片。
	resp.VideoList = nonNilFeedVideoItems(resp.VideoList)
	// 200。
	c.JSON(200, resp)
}

// nonNilFeedVideoItems 是普通函数：把 nil 的列表换成空切片。
//
// 参数 items：可能为 nil 的展示项切片；
// 返回值：保证非 nil 的切片。
func nonNilFeedVideoItems(items []FeedVideoItem) []FeedVideoItem {
	// 为 nil（一条数据都没有时）。
	if items == nil {
		// 返回空切片：JSON 序列化为 [] 而不是 null。
		return []FeedVideoItem{}
	}
	// 原样返回。
	return items
}

// ListByTag 方法：处理"按标签查视频"请求。
//
// 参数 c：Gin 上下文。
func (h *FeedHandler) ListByTag(c *gin.Context) {
	// req 用【匿名结构体】直接声明请求参数（只此一处用，不必单独命名类型）。
	var req struct {
		// TagName 标签名。
		TagName string `json:"tag_name"`
		// Limit 条数。
		Limit int `json:"limit"`
	}
	// 解析 JSON；err 错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 400。
		c.JSON(400, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 标签名为空。
	if req.TagName == "" {
		// 400。
		c.JSON(400, gin.H{"error": "tag_name is required"})
		// 结束。
		return
	}
	// 条数兜底。
	if req.Limit <= 0 || req.Limit > 50 {
		// 默认 10。
		req.Limit = 10
	}
	// viewerAccountID 取登录态，错误直接用 _ 忽略（查标签允许游客）。
	viewerAccountID, _ := jwt.GetAccountID(c)
	// items 调服务按标签查；err 错误。
	items, err := h.service.ListByTag(c.Request.Context(), req.TagName, req.Limit, viewerAccountID)
	if err != nil {
		// 失败：归类状态码返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 200 返回（这里用自定义的 video_list 字段名，而不是直接返回结构体）。
	c.JSON(200, gin.H{"video_list": nonNilFeedVideoItems(items)})
}
