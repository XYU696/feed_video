// package http：HTTP 装配包。本文件是整个 API 的"总路由配置"：
// 创建所有 Repository / Service / Handler，声明每个 URL 该由谁处理、要经过哪些中间件，
// 并启动 Outbox 轮询器、时间线消费者、SSE 通知等后台任务。
//
// 面试时这文件是讲项目架构的最好入口：从它能看到系统全部模块和依赖关系。
package http

import (
	// context：给后台通知任务用。
	"context"
	// net/http：审核员 403 等状态码。
	"net/http"
	// account：账号模块。
	"feedsystem_video_go/internal/account"
	// agent：人工终审处置器（D7）。
	"feedsystem_video_go/internal/agent"
	// config：传空阈值配置（构造器内兜底）。
	"feedsystem_video_go/internal/config"
	// feed：信息流模块。
	"feedsystem_video_go/internal/feed"
	// message：私信模块。
	"feedsystem_video_go/internal/message"
	// moderation：举报模块、审核台服务。
	"feedsystem_video_go/internal/moderation"
	// jwt：鉴权中间件（JWTAuth 强制登录、SoftJWTAuth 登录可选）。
	"feedsystem_video_go/internal/middleware/jwt"
	// rabbitmq：各类 MQ 发布器。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// ratelimit：限流中间件。
	"feedsystem_video_go/internal/middleware/ratelimit"
	// rediscache：Redis 客户端类型。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// social：关注模块。
	"feedsystem_video_go/internal/social"
	// video：视频/点赞/评论/分片上传模块。
	"feedsystem_video_go/internal/video"
	// worker：后台任务（Outbox 轮询、SSE Hub、通知 worker 等）。
	"feedsystem_video_go/internal/worker"
	// log：初始化失败时打日志。
	"log"
	// time：限流时间窗口。
	"time"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
	// gorm：数据库连接类型。
	"gorm.io/gorm"
)

// SetRouter 函数的作用：装配整个 HTTP 服务，返回可用的 Gin 引擎。
//
// 参数：db 数据库连接、cache Redis 客户端（可能为 nil）、rmq RabbitMQ 连接（可能为 nil）、
// reviewerIDs 审核员账号编号列表（D7：只有这些账号能访问审核台）；
// 返回值 *gin.Engine：配置好全部路由和中间件的引擎。
//
// 贯穿全文件的容错思想：MQ 为 nil 时每个发布器初始化失败都只是"该功能停用"，不影响整体启动。
func SetRouter(db *gorm.DB, cache *rediscache.Client, rmq *rabbitmq.RabbitMQ, reviewerIDs []uint) *gin.Engine {
	// r 创建默认 Gin 引擎（gin.Default 自带 Logger 日志和 Recovery 崩溃恢复两个中间件）。
	r := gin.Default()
	// r.SetTrustedProxies(nil)：设置"信任的反向代理"为 nil = 不信任任何代理。
	// 作用：防止客户端伪造 X-Forwarded-For 头来伪造 IP（KeyByIP 限流时取的 IP 才可信）；err 接收错误。
	if err := r.SetTrustedProxies(nil); err != nil {
		// 设置失败（参数问题）只记日志。
		log.Printf("SetTrustedProxies failed: %v", err)
	}
	// r.GET("/healthz", 处理函数)：注册健康检查接口（GET 请求）。
	// 用于 Docker/K8s/CI 探测服务是否活着；这里用匿名函数直接内联处理。
	r.GET("/healthz", func(c *gin.Context) {
		// 永远返回 200 和 {"status":"ok"}。
		c.JSON(200, gin.H{"status": "ok"})
	})
	// r.Static("/static", "./.run/uploads")：把磁盘目录 ./.run/uploads 暴露成静态文件服务，
	// 这样上传的视频/封面/头像才能通过 /static/... URL 被浏览器访问。
	r.Static("/static", "./.run/uploads")
	// rate_limit
	// loginLimiter：登录限流——每个 IP 每分钟最多 10 次（KeyByIP 按来源 IP 计数）。
	loginLimiter := ratelimit.Limit(cache, "account_login", 10, time.Minute, ratelimit.KeyByIP)
	// registerLimiter：注册限流——每个 IP 每小时最多 5 次（防批量注册小号）。
	registerLimiter := ratelimit.Limit(cache, "account_register", 5, time.Hour, ratelimit.KeyByIP)

	// likeLimiter：点赞限流——每个账号每分钟最多 30 次。
	likeLimiter := ratelimit.Limit(cache, "like_write", 30, time.Minute, ratelimit.KeyByAccount)
	// commentLimiter：评论限流——每个账号每分钟最多 10 次。
	commentLimiter := ratelimit.Limit(cache, "comment_write", 10, time.Minute, ratelimit.KeyByAccount)
	// socialLimiter：关注限流——每个账号每分钟最多 20 次。
	socialLimiter := ratelimit.Limit(cache, "social_write", 20, time.Minute, ratelimit.KeyByAccount)
	// reportLimiter：举报限流——每个账号每分钟最多 10 次。
	reportLimiter := ratelimit.Limit(cache, "report_create", 10, time.Minute, ratelimit.KeyByAccount)

	// account —— 开始组装账号模块的三层对象：
	// accountRepository 创建账号仓储，传入数据库连接。
	accountRepository := account.NewAccountRepository(db)
	// accountService 创建账号服务，注入仓储和缓存。
	accountService := account.NewAccountService(accountRepository, cache)
	// accountHandler 创建账号处理器，注入服务。
	accountHandler := account.NewAccountHandler(accountService)
	// accountGroup 创建路由组，统一前缀 /account（组内路由都会自动带上这个前缀）。
	accountGroup := r.Group("/account")
	// { } 只是代码块，让组内注册 visually 成组（Go 语法允许单独的花括号块）。
	{
		// 这些是"无需登录"的公开接口，把限流中间件和最终处理函数按顺序挂上：
		// POST /account/register，先过 registerLimiter 再到 CreateAccount。
		accountGroup.POST("/register", registerLimiter, accountHandler.CreateAccount)
		// POST /account/login，先过 loginLimiter。
		accountGroup.POST("/login", loginLimiter, accountHandler.Login)
		// POST /account/changePassword（凭旧密码即可，不强制 token）。
		accountGroup.POST("/changePassword", accountHandler.ChangePassword)
		// POST /account/findByID。
		accountGroup.POST("/findByID", accountHandler.FindByID)
		// POST /account/findByUsername。
		accountGroup.POST("/findByUsername", accountHandler.FindByUsername)
		// POST /account/refresh（用 refresh token 换新 access token）。
		accountGroup.POST("/refresh", accountHandler.Refresh)
	}
	// protectedAccountGroup 在账号组下再开一个【需要登录】的子组（路径前缀仍为 /account）。
	protectedAccountGroup := accountGroup.Group("")
	// protectedAccountGroup.Use(jwt.JWTAuth(accountRepository, cache))：
	// 给子组挂载 JWT 鉴权中间件，组内所有请求必须带有效 token，否则在中间件就被拦下。
	protectedAccountGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /account/logout。
		protectedAccountGroup.POST("/logout", accountHandler.Logout)
		// POST /account/rename。
		protectedAccountGroup.POST("/rename", accountHandler.Rename)
		// POST /account/uploadAvatar。
		protectedAccountGroup.POST("/uploadAvatar", accountHandler.UploadAvatar)
		// POST /account/updateProfile。
		protectedAccountGroup.POST("/updateProfile", accountHandler.UpdateProfile)
	}
	// video —— 组装视频模块：
	// videoRepository 创建视频仓储。
	videoRepository := video.NewVideoRepository(db)
	// popularityMQ 创建热度 MQ 发布器；err 接收错误（rmq 为 nil 时会失败）。
	popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
	if err != nil {
		// 失败只记日志，并把发布器置 nil（热度功能降级为直接写缓存/库）。
		log.Printf("PopularityMQ init failed (mq disabled): %v", err)
		// popularityMQ = nil。
		popularityMQ = nil
	}
	// videoService 创建视频服务，注入仓储、缓存、热度 MQ。
	videoService := video.NewVideoService(videoRepository, cache, popularityMQ)
	// videoHandler 创建视频处理器，注入视频服务和账号服务（查作者信息用）。
	videoHandler := video.NewVideoHandler(videoService, accountService)
	// chunkHandler 创建分片上传处理器（只依赖缓存存上传会话）。
	chunkHandler := video.NewChunkUploadHandler(cache)
	// videoGroup 路由组，前缀 /video。
	videoGroup := r.Group("/video") //不挂鉴权，说明游客也能访问视频模块的公开接口（查视频列表、详情等）。
	{
		// 公开接口：POST /video/listByAuthorID（查某作者的视频列表）。
		videoGroup.POST("/listByAuthorID", videoHandler.ListByAuthorID)
		// 公开接口：POST /video/getDetail（视频详情，公开可读）。
		videoGroup.POST("/getDetail", videoHandler.GetDetail)
	}
	// protectedVideoGroup 需要登录的视频子组。
	protectedVideoGroup := videoGroup.Group("")
	// 挂载 JWT 鉴权。  只给子组挂鉴权，因为视频模块下有登录接口。
	protectedVideoGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /video/uploadVideo：上传整片视频。
		protectedVideoGroup.POST("/uploadVideo", videoHandler.UploadVideo)
		// POST /video/uploadCover：上传封面。
		protectedVideoGroup.POST("/uploadCover", videoHandler.UploadCover)
		// POST /video/publish：填写信息后正式发布。
		protectedVideoGroup.POST("/publish", videoHandler.PublishVideo)
		// 以下四个是"分片上传"接口（大文件切成小块上传）：
		// /chunk/init：初始化上传会话。
		protectedVideoGroup.POST("/chunk/init", chunkHandler.InitChunkUpload)
		// /chunk/upload：上传单个分片。
		protectedVideoGroup.POST("/chunk/upload", chunkHandler.UploadChunk)
		// /chunk/status：查询哪些分片已传（断点续传）。
		protectedVideoGroup.POST("/chunk/status", chunkHandler.ChunkStatus)
		// /chunk/complete：全部分片传完后合并。
		protectedVideoGroup.POST("/chunk/complete", chunkHandler.CompleteChunkUpload)
	}
	// like —— 组装点赞模块：
	// likeMQ 创建点赞事件发布器；err 接收错误。
	likeMQ, err := rabbitmq.NewLikeMQ(rmq)
	if err != nil {
		// 失败置 nil。
		log.Printf("LikeMQ init failed (mq disabled): %v", err)
		// likeMQ = nil。
		likeMQ = nil
	}
	// likeRepository 创建点赞仓储。
	likeRepository := video.NewLikeRepository(db)
	// likeService 创建点赞服务，注入点赞仓储、视频仓储（校验视频存在/取信息）、缓存、点赞 MQ、热度 MQ（点赞同时影响热度）。
	likeService := video.NewLikeService(likeRepository, videoRepository, cache, likeMQ, popularityMQ)
	// likeHandler 创建点赞处理器。
	likeHandler := video.NewLikeHandler(likeService)
	// likeGroup 路由组，前缀 /like。
	likeGroup := r.Group("/like")
	// protectedLikeGroup 点赞组默认就需要登录（点赞必须有身份）。
	protectedLikeGroup := likeGroup.Group("")
	// 挂载 JWT 鉴权。
	protectedLikeGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /like/like：点赞（额外挂 likeLimiter 限流）。
		protectedLikeGroup.POST("/like", likeLimiter, likeHandler.Like)
		// POST /like/unlike：取消点赞。
		protectedLikeGroup.POST("/unlike", likeLimiter, likeHandler.Unlike)
		// POST /like/isLiked：查询是否已赞。
		protectedLikeGroup.POST("/isLiked", likeHandler.IsLiked)
		// POST /like/listMyLikedVideos：我点赞过的视频列表。
		protectedLikeGroup.POST("/listMyLikedVideos", likeHandler.ListMyLikedVideos)
	}
	// comment —— 组装评论模块：
	// commentRepository 创建评论仓储。
	commentRepository := video.NewCommentRepository(db)
	// commentMQ 创建评论事件发布器；err 接收错误。
	commentMQ, err := rabbitmq.NewCommentMQ(rmq)
	if err != nil {
		// 失败置 nil。
		log.Printf("CommentMQ init failed (mq disabled): %v", err)
		// commentMQ = nil。
		commentMQ = nil
	}
	// commentService 创建评论服务，注入评论仓储、视频仓储、缓存、评论 MQ、热度 MQ（发评论也加热度）。
	commentService := video.NewCommentService(commentRepository, videoRepository, cache, commentMQ, popularityMQ)
	// commentHandler 创建评论处理器，注入评论服务和账号服务（补用户名等）。
	commentHandler := video.NewCommentHandler(commentService, accountService)
	// commentGroup 路由组，前缀 /comment。
	commentGroup := r.Group("/comment")
	{
		// 公开接口：POST /comment/listAll（视频评论列表公开可读）。
		commentGroup.POST("/listAll", commentHandler.GetAllComments)
	}
	// protectedCommentGroup 需要登录的评论子组。
	protectedCommentGroup := commentGroup.Group("")
	// 挂载 JWT 鉴权。
	protectedCommentGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /comment/publish：发表评论（限流器防刷屏）。
		protectedCommentGroup.POST("/publish", commentLimiter, commentHandler.PublishComment)
		// POST /comment/delete：删除评论。
		protectedCommentGroup.POST("/delete", commentLimiter, commentHandler.DeleteComment)
	}
	// social —— 组装关注模块：
	// socialMQ 创建关注事件发布器；err 接收错误。
	socialMQ, err := rabbitmq.NewSocialMQ(rmq)
	if err != nil {
		// 失败置 nil。
		log.Printf("SocialMQ init failed (mq disabled): %v", err)
		// socialMQ = nil。
		socialMQ = nil
	}
	// socialRepository 创建关注仓储。
	socialRepository := social.NewSocialRepository(db)
	// socialService 创建关注服务，注入关注仓储、账号仓储、关注 MQ、缓存。
	socialService := social.NewSocialService(socialRepository, accountRepository, socialMQ, cache)
	// socialHandler 创建关注处理器。
	socialHandler := social.NewSocialHandler(socialService)
	// socialGroup 路由组，前缀 /social。
	socialGroup := r.Group("/social")
	// protectedSocialGroup 关注组整体需要登录。
	protectedSocialGroup := socialGroup.Group("")
	// 挂载 JWT 鉴权。
	protectedSocialGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /social/follow：关注（限流）。
		protectedSocialGroup.POST("/follow", socialLimiter, socialHandler.Follow)
		// POST /social/unfollow：取关（限流）。
		protectedSocialGroup.POST("/unfollow", socialLimiter, socialHandler.Unfollow)
		// POST /social/getAllFollowers：粉丝列表。
		protectedSocialGroup.POST("/getAllFollowers", socialHandler.GetAllFollowers)
		// POST /social/getAllVloggers：关注的博主列表。
		protectedSocialGroup.POST("/getAllVloggers", socialHandler.GetAllVloggers)
		// POST /social/getCounts：粉丝数/关注数。
		protectedSocialGroup.POST("/getCounts", socialHandler.GetCounts)
	}

	// moderation —— 举报模块三层对象：
	// reportMQ 创建举报事件发布器；err 接收错误（rmq 为 nil 时会失败）。
	reportMQ, err := rabbitmq.NewReportMQ(rmq)
	if err != nil {
		// 失败置 nil（举报仍可落库，只是不发事件）。
		log.Printf("ReportMQ init failed (mq disabled): %v", err)
		// reportMQ = nil。
		reportMQ = nil
	}
	// reportRepository 举报仓储。
	reportRepository := moderation.NewReportRepository(db)
	// reportService 举报服务，注入仓储、举报 MQ。
	reportService := moderation.NewReportService(reportRepository, reportMQ)
	// reportHandler 举报处理器。
	reportHandler := moderation.NewReportHandler(reportService)
	// reportGroup 路由组，前缀 /report；组内都需要登录。
	reportGroup := r.Group("/report")
	// 挂 JWT 鉴权。
	reportGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /report：提交举报（限流）。
		reportGroup.POST("", reportLimiter, reportHandler.CreateReport)
		// POST /report/listMy：查我提交的举报。
		reportGroup.POST("/listMy", reportHandler.ListMyReports)
	}

	// accountGroup.POST("/getProfile", ...)：把"用户主页聚合接口"直接挂在账号组（公开接口）。
	// 用一个较长的匿名函数内联实现，因为它要同时聚合账号、视频、点赞、关注四类数据。
	accountGroup.POST("/getProfile", func(c *gin.Context) {
		// req 声明用户主页请求结构体。
		var req account.GetProfileRequest
		// 绑定校验 JSON；err 接收错误。
		if err := c.ShouldBindJSON(&req); err != nil {
			// 参数错误：400。
			c.JSON(400, gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// 没传账号编号。
		if req.AccountID == 0 {
			// 400 提示必填。
			c.JSON(400, gin.H{"error": "account_id is required"})
			// 结束。
			return
		}
		// acc 查到账号主体；err 接收错误。
		acc, err := accountService.FindByID(c.Request.Context(), req.AccountID)
		if err != nil {
			// 查询失败：500。
			c.JSON(500, gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// 下面四个统计量并行语义不大、顺序调用即可；错误用 _ 忽略（统计失败时按 0 处理，不影响主页展示）：
		// videoCount：该用户发布的视频总数。
		videoCount, _ := videoRepository.CountByAuthor(c.Request.Context(), req.AccountID)
		// totalLikes：该用户所有视频被点赞的总数。
		totalLikes, _ := videoRepository.TotalLikesByAuthor(c.Request.Context(), req.AccountID)
		// followerCount：粉丝数。
		followerCount, _ := socialRepository.CountFollowers(c.Request.Context(), req.AccountID)
		// vloggerCount：关注的博主数。
		vloggerCount, _ := socialRepository.CountVloggers(c.Request.Context(), req.AccountID)

		// 200 返回聚合好的主页响应（用 GetProfileResponse 一次装齐所有统计）。
		c.JSON(200, account.GetProfileResponse{
			// Account 字段只放公开信息（编号、用户名、头像、简介）。
			Account: account.FindByIDResponse{ID: acc.ID, Username: acc.Username, AvatarURL: acc.AvatarURL, Bio: acc.Bio},
			// 四个统计字段一并填入。
			VideoCount: videoCount, TotalLikes: totalLikes,
			// 续上一行：粉丝数、关注数。
			FollowerCount: followerCount, VloggerCount: vloggerCount,
		})
	})
	// feed —— 组装信息流模块：
	// feedRepository 创建信息流仓储。
	feedRepository := feed.NewFeedRepository(db)
	// feedService 创建信息流服务，注入信息流仓储、点赞仓储（标记 IsLiked）、缓存。
	feedService := feed.NewFeedService(feedRepository, likeRepository, cache)
	// feedHandler 创建信息流处理器。
	feedHandler := feed.NewFeedHandler(feedService)
	// feedGroup 路由组，前缀 /feed。
	feedGroup := r.Group("/feed")
	// feedGroup.Use(jwt.SoftJWTAuth(...))：挂【软鉴权】——
	// 带了 token 就解析出用户（用于标记"我是否赞过"），不带 token 也能看 Feed（游客可浏览）。
	feedGroup.Use(jwt.SoftJWTAuth(accountRepository, cache))
	{
		// POST /feed/listLatest：最新发布流（时间游标分页）。
		feedGroup.POST("/listLatest", feedHandler.ListLatest)
		// POST /feed/listLikesCount：高赞流（点赞数游标分页）。
		feedGroup.POST("/listLikesCount", feedHandler.ListLikesCount)
		// POST /feed/listByPopularity：热门流（热度滑动窗口榜）。
		feedGroup.POST("/listByPopularity", feedHandler.ListByPopularity)
		// POST /feed/listByTag：按标签筛选。
		feedGroup.POST("/listByTag", feedHandler.ListByTag)
	}
	// protectedFeedGroup 在 feed 组里再开一个必须登录的子组。
	protectedFeedGroup := feedGroup.Group("")
	// 强制 JWT 鉴权。
	protectedFeedGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /feed/listByFollowing：关注流（只看关注的人的视频，必须知道你是谁）。
		protectedFeedGroup.POST("/listByFollowing", feedHandler.ListByFollowing)
	}
	// message —— 组装私信模块：
	// messageRepo 创建私信仓储。
	messageRepo := message.NewRepository(db)
	// messageService 创建私信服务。
	messageService := message.NewService(messageRepo)
	// messageHandler 创建私信处理器。
	messageHandler := message.NewHandler(messageService)
	// messageGroup 路由组，前缀 /message。
	messageGroup := r.Group("/message")
	// protectedMessageGroup 私信整体需要登录。
	protectedMessageGroup := messageGroup.Group("")
	// 挂载 JWT 鉴权。
	protectedMessageGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// POST /message/send：发送私信。
		protectedMessageGroup.POST("/send", messageHandler.Send)
		// POST /message/list：查看与某人的消息记录。
		protectedMessageGroup.POST("/list", messageHandler.List)
	}
	//worker —— 启动进程内的后台任务：
	// timelineMQ 创建时间线事件发布器（OutboxPoller 要用它把消息真正发到 MQ）；err 接收错误。
	timelineMQ, err := rabbitmq.NewTimelineMQ(rmq)
	if err != nil {
		// 失败置 nil（Outbox 轮询会在 MQ 不可用时等待）。
		log.Printf("timelineMQ init failed (mq disabled): %v", err)
		// timelineMQ = nil。
		timelineMQ = nil
	}
	// worker.StartOutboxPoller(db, timelineMQ)：启动发件箱轮询器（后台 goroutine），
	// 不断把 outbox_msgs 表里 status=pending 的消息发到 MQ，发成功后标记 done。
	worker.StartOutboxPoller(db, timelineMQ)
	// worker.StartConsumer(...)：启动时间线消费者（后台 goroutine），
	// 监听队列 video.timeline.update.queue，把新视频写进 Redis 全局时间线 ZSET。
	worker.StartConsumer(timelineMQ, "video.timeline.update.queue", cache, rmq)

	// SSE notification —— 准备实时通知（点赞/评论/关注 实时推送给被互动者）：
	// rmq != nil：只有 MQ 可用时才配置通知。
	if rmq != nil {
		// notifCh 开一个临时 channel 用来声明通知队列；err 接收错误。
		if notifCh, err := rmq.NewChannel(); err == nil {
			// 声明"点赞通知"：复用 like.events 交换机，新建 notification.like 队列，只绑定 like.like 路由键
			// （被赞的通知，不要 unlike）。
			if err := rabbitmq.DeclareTopic(notifCh, "like.events", "notification.like", "like.like"); err != nil {
				// 声明失败只记日志。
				log.Printf("notification like topic init failed: %v", err)
			}
			// 声明"评论通知"：comment.events 交换机，notification.comment 队列，绑定 comment.publish。
			if err := rabbitmq.DeclareTopic(notifCh, "comment.events", "notification.comment", "comment.publish"); err != nil {
				// 记日志。
				log.Printf("notification comment topic init failed: %v", err)
			}
			// 声明"关注通知"：social.events 交换机，notification.social 队列，绑定 social.follow。
			if err := rabbitmq.DeclareTopic(notifCh, "social.events", "notification.social", "social.follow"); err != nil {
				// 记日志。
				log.Printf("notification social topic init failed: %v", err)
			}
			// 声明"审核台通知"（D7）：moderation.events 交换机，
			// notification.moderation 队列，只绑定 moderation.pending。
			if err := rabbitmq.DeclareTopic(notifCh, "moderation.events", "notification.moderation", "moderation.pending"); err != nil {
				// 记日志。
				log.Printf("notification moderation topic init failed: %v", err)
			}
			// notifCh.Close()：临时 channel 用完关闭。
			notifCh.Close()
		}
	}
	// sseHub 创建 SSE 推送中心（持有 db，用来查通知该推给谁）。
	sseHub := worker.NewSSEHub(db)
	// notifGroup 通知路由组，前缀 /notification。
	notifGroup := r.Group("/notification")
	// notifGroup.Use(sseHub.SSERequireAuth())：挂 SSE 专用鉴权（验证 query 参数里的 token）。
	notifGroup.Use(sseHub.SSERequireAuth())
	// sseHub.RegisterRoutes(r, notifGroup)：在通知组上注册 SSE 连接接口（浏览器通过 EventSource 连进来）。
	sseHub.RegisterRoutes(r, notifGroup)

	// go func() { ... }()：再开一个后台 goroutine，负责启动三个通知消费者。
	go func() {
		// rmq 可用时。
		if rmq != nil {
			// hub 局部变量指向 sseHub（消费者收到消息后通过它实时推给在线用户）。
			hub := sseHub
			// ctx 用 background 上下文（通知任务跟任何单个 HTTP 请求无关，生命周期等于整个进程）。
			ctx := context.Background()
			// 遍历三个通知队列名；每个队列都单独起一个 goroutine 消费。
			// 重点技术点：[]string{...} 是字符串切片字面量；for range 遍历它，q 是当前队列名。
			for _, q := range []string{"notification.like", "notification.comment", "notification.social"} {
				// 每个队列起独立 goroutine；注意把 q 作为参数传进匿名函数（queue），
				// 避免多个 goroutine 捕获同一个循环变量导致串值（这是 Go 闭包经典坑，显式传参最稳妥）。
				go func(queue string) {
					// for 无限循环：消费者因任何原因退出后，等 5 秒自动重连（保证通知能力自愈）。
					for {
						// ch 每次重连都开新 channel；err 接收错误。
						ch, err := rmq.NewChannel()
						if err != nil {
							// 开通道失败：记日志。
							log.Printf("notification-%s: 创建 Channel 失败: %v, 5秒后重试", queue, err)
							// 睡 5 秒。
							time.Sleep(5 * time.Second)
							// continue 回到循环开头重试。
							continue
						}
						// w 创建该队列的通知 worker（注入 channel、db、队列名、推送中心 hub）。
						w := worker.NewNotificationWorker(ch, db, queue, hub)
						// w.Run(ctx) 开始阻塞消费；返回错误（连接断开等）时进入重连。
						if err := w.Run(ctx); err != nil {
							// 记日志提示将重连。
							log.Printf("notification-%s: %v, 5秒后重连...", queue, err)
						}
						// ch.Close()：关掉旧 channel。
						ch.Close()
						// 睡 5 秒再重连，避免故障时疯狂重连打爆服务。
						time.Sleep(5 * time.Second)
					}
				}(q)
			}
		} else {
			// MQ 不可用：通知功能停用，只提示。
			log.Printf("Notification SSE disabled (MQ not available)")
		}
	}()

	// ===== D7：人工审核台 =====
	// 审核通知消费者：消费 notification.moderation 队列，给审核员落通知 + SSE 推送。
	go func() {
		// MQ 不可用：审核台实时通知停用（审核员仍可手动刷新列表）。
		if rmq == nil {
			// 提示。
			log.Printf("Moderation review notification disabled (MQ not available)")
			// 返回。
			return
		}
		// ctx background：生命周期等于整个进程。
		ctx := context.Background()
		// 无限循环：断连/退出后 5 秒自愈重连，与其他通知消费者一致。
		for {
			// ch 每次新开通道；err。
			ch, err := rmq.NewChannel()
			// 开通道失败。
			if err != nil {
				// 记录。
				log.Printf("notification-moderation: 创建 Channel 失败: %v, 5秒后重试", err)
				// 睡。
				time.Sleep(5 * time.Second)
				// 重试。
				continue
			}
			// mw 创建审核通知 worker：注入通道、队列、db、推送中心、审核员列表。
			mw := worker.NewModerationNotificationWorker(ch, "notification.moderation", db, sseHub, reviewerIDs)
			// Run 阻塞消费；返回错误时记日志、进入重连。
			if err := mw.Run(ctx); err != nil {
				// 记录。
				log.Printf("notification-moderation: %v, 5秒后重连...", err)
			}
			// 关旧通道。
			ch.Close()
			// 睡 5 秒再重连。
			time.Sleep(5 * time.Second)
		}
	}()

	// 审核台三层装配：
	// modCaseRepo / modReportRepo 工单、举报仓储。
	modCaseRepo := moderation.NewCaseRepository(db)
	// 举报仓储。
	modReportRepo := moderation.NewReportRepository(db)
	// modAuditRepo 审计仓储（D10）：终审留痕。
	modAuditRepo := moderation.NewAuditRepository(db)
	// reviewActionExecutor 人工终审处置器：复用 agent.ActionExecutor，
	// 传空 ThresholdConfig 会在构造函数里兜底为 0.85/0.7（人工路径不看置信度，阈值仅占位）。
	reviewActionExecutor := agent.NewActionExecutor(
		// 视频仓储。
		video.NewVideoRepository(db),
		// 评论仓储。
		video.NewCommentRepository(db),
		// 私信仓储（警告）。
		message.NewRepository(db),
		// Redis：锁 + 缓存失效。
		cache,
		// 阈值：零值兜底。
		config.ThresholdConfig{},
	)
	// reviewService 审核服务：executor 以接口身份注入（moderation 不依赖 agent），
	// auditRepo 写终审流水；同一 executor 兼具 ResultNotifier，负责举报人结果通知。
	reviewService := moderation.NewReviewService(
		// caseRepo、reportRepo。
		modCaseRepo, modReportRepo,
		// executor。
		reviewActionExecutor,
		// auditRepo。
		modAuditRepo,
		// notifier：ActionExecutor 已实现 NotifyResult。
		reviewActionExecutor,
	)
	// reviewHandler 审核台 HTTP 处理器。
	reviewHandler := moderation.NewReviewHandler(reviewService)

	// 审核台接口组：JWT 登录 + 审核员身份，两道中间件。
	modGroup := r.Group("/moderation")
	// JWT 强鉴权。
	modGroup.Use(jwt.JWTAuth(accountRepository, cache))
	// 审核员白名单校验。
	modGroup.Use(requireReviewer(reviewerIDs))
	// POST /moderation/list 工单列表。
	modGroup.POST("/list", reviewHandler.List)
	// POST /moderation/review 人工终审。
	modGroup.POST("/review", reviewHandler.Review)

	// GET /moderation/stream：审核员 SSE 实时工单流。
	// EventSource 不能带自定义头，走 query token 鉴权，复用 SSEHub 的 SSEHandler——
	// hub 的频道只按用户编号区分，通过哪条路由订阅都能收到发给该审核员的通知。
	modStreamGroup := r.Group("/moderation")
	// SSE query token 鉴权 + 审核员白名单。
	modStreamGroup.Use(sseHub.SSERequireAuth(), requireReviewer(reviewerIDs))
	// GET stream。
	modStreamGroup.GET("/stream", sseHub.SSEHandler)

	// 全部装配完成，返回 Gin 引擎给 main 函数启动。
	return r
}

// requireReviewer 普通函数：返回"审核员白名单"中间件（D7）。
//
// 审核员身份用最简单的"配置账号编号列表"实现（agent.md §10）；
// 账号 role 字段方案属于二期。
//
// 参数 reviewerIDs：审核员编号列表；
// 返回值 gin.HandlerFunc：非审核员返回 403。
func requireReviewer(reviewerIDs []uint) gin.HandlerFunc {
	// allowed 把列表转成 set，查询 O(1)。
	allowed := make(map[uint]bool, len(reviewerIDs))
	// range 填充。
	for _, id := range reviewerIDs {
		// 写入。
		allowed[id] = true
	}
	// 返回闭包中间件。
	return func(c *gin.Context) {
		// accountID 取当前登录编号；err。
		accountID, err := jwt.GetAccountID(c)
		// 取不到、或不在审核员名单里。
		if err != nil || !allowed[accountID] {
			// 403 Forbidden：登录了但没有权限。
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden: reviewer only"})
			// 结束。
			return
		}
		// 放行。
		c.Next()
	}
}
