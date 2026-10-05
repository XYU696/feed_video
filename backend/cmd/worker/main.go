// package main：这是【独立 worker 进程】的入口（区别于 cmd/main.go 的 Web 服务进程）。
//
// 部署时可以把它和 Web 服务分开跑：Web 服务负责收 HTTP 请求、发 MQ；
// 本进程专门连 RabbitMQ 消费四类事件（关注/点赞/评论/热度）并落库，
// 从而把后台计算和前台流量隔离开。
//
// 本文件做的事：加载配置 → 连 MySQL/Redis/RabbitMQ（都带重试）
// → 声明交换机/队列拓扑 → 给每个 worker 独立 Channel 并自动重连 → 等退出信号优雅关闭。
package main

import (
	// context：贯穿连接与生命周期。
	"context"
	// agent：构建内容安全 Agent。
	"feedsystem_video_go/internal/agent"
	// account：用户仓储（get_account 工具）。
	"feedsystem_video_go/internal/account"
	// config：读 yaml 配置。
	"feedsystem_video_go/internal/config"
	// db：连接/关闭 MySQL。
	"feedsystem_video_go/internal/db"
	// mqrabbit：RabbitMQ 封装，用到死信交换机常量 DLXExchange。
	mqrabbit "feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// message：警告私信仓储（D6 处置动作）。
	"feedsystem_video_go/internal/message"
	// moderation：举报/工单仓储。
	"feedsystem_video_go/internal/moderation"
	// observability：worker 自己的 pprof 服务。
	"feedsystem_video_go/internal/observability"
	// social：SocialRepository。
	"feedsystem_video_go/internal/social"
	// video：各仓储。
	"feedsystem_video_go/internal/video"
	// worker：四个 Worker 类型。
	"feedsystem_video_go/internal/worker"
	// log：启动/错误日志。
	"log"
	// os：环境变量、退出信号。
	"os"
	// os/signal：监听 Ctrl+C / kill。
	"os/signal"
	// strconv：端口数字转字符串。
	"strconv"
	// syscall：SIGTERM 信号。
	"syscall"
	// time：退避、超时。
	"time"

	// amqp：RabbitMQ 官方客户端。
	amqp "github.com/rabbitmq/amqp091-go"
	// godotenv：读 .env 文件。
	"github.com/joho/godotenv"
	// gorm：MySQL ORM 连接类型。
	"gorm.io/gorm"
)

// 这一组常量定义四条队列的【交换机名 / 队列名 / 绑定键】。
// 本项目里交换机和队列取了同名（简化记忆）。
const (
	// socialExchange 社交事件交换机名。
	socialExchange = "social.events"
	// socialQueue 社交队列名。
	socialQueue = "social.events"
	// socialBindingKey 绑定键 social.*：匹配 social.follow、social.unfollow。
	socialBindingKey = "social.*"

	// likeExchange 点赞事件交换机。
	likeExchange = "like.events"
	// likeQueue 点赞队列。
	likeQueue = "like.events"
	// likeBindingKey like.*：匹配 like.like、like.unlike。
	likeBindingKey = "like.*"

	// commentExchange 评论事件交换机。
	commentExchange = "comment.events"
	// commentQueue 评论队列。
	commentQueue = "comment.events"
	// commentBindingKey comment.*：匹配 comment.publish、comment.delete。
	commentBindingKey = "comment.*"

	// popularityExchange 热度事件交换机（名字更长，避免和点赞混淆）。
	popularityExchange = "video.popularity.events"
	// popularityQueue 热度队列。
	popularityQueue = "video.popularity.events"
	// popularityBindingKey video.popularity.*。
	popularityBindingKey = "video.popularity.*"

	// moderationExchange 内容安全事件交换机名。
	moderationExchange = "moderation.events"
	// moderationQueue 内容安全队列名。
	moderationQueue = "moderation.events"
	// moderationBindingKey report.*：匹配 report.created。
	moderationBindingKey = "report.*"
)

// connectWithRetry 是普通函数：反复尝试执行 fn 直到成功或超过最大次数。
//
// 参数：name 资源名（打日志用）；maxRetries 最大次数；fn 实际的连接动作（返回 error）。
// 超过次数仍失败就 log.Fatalf 直接退出进程（基础设施连不上，worker 没法干活）。
func connectWithRetry(name string, maxRetries int, fn func() error) {
	// i 从 0 数到 maxRetries-1。
	for i := 0; i < maxRetries; i++ {
		// 执行一次连接；err 为 nil 表示成功。
		if err := fn(); err == nil {
			// 成功：直接返回。
			return
		}
		// wait 指数退避：1、2、4... 秒。
		wait := time.Duration(1<<i) * time.Second
		// 封顶 30 秒，避免后面越等越久。
		if wait > 30*time.Second {
			// 压回 30 秒。
			wait = 30 * time.Second
		}
		// 记录第几次失败、多久后重试。
		log.Printf("%s 不可用，%v 后重试 (%d/%d)...", name, wait, i+1, maxRetries)
		// 休眠。
		time.Sleep(wait)
	}
	// 所有重试都失败：打印致命错误并退出进程。
	log.Fatalf("%s: 超过最大重试次数", name)
}

// runWorkerWithRetry 是普通函数：在独立 Channel 上跑一个 worker，Channel 断开后自动重连。
//
// 参数：ctx 生命周期；name worker 名（日志用）；conn RabbitMQ TCP 连接（断线重连靠它）；
// fn 真正的 worker 启动逻辑——接收一个 Channel、返回 error（Run 返回即说明连接断了）。
//
// 为什么每个 worker 一个独立 Channel？AMQP 里同一 Channel 不建议并发使用，
// 且一个 Channel 异常关闭会影响挂在上面的所有消费者，分开更隔离。
func runWorkerWithRetry(ctx context.Context, name string, conn *amqp.Connection, fn func(*amqp.Channel) error) {
	// 外层无限循环负责重连，直到 ctx 取消。
	for {
		// 每轮先看是否该退出（非阻塞）。
		select {
		// ctx 已取消。
		case <-ctx.Done():
			// 返回，goroutine 结束。
			return
		// 没取消。
		default:
		}

		// conn.Channel 在同一 TCP 连接上开一个新 Channel；ch 频道、err 错误。
		ch, err := conn.Channel()
		if err != nil {
			// 开 Channel 失败：等 5 秒再试（不退出，连接可能在恢复）。
			log.Printf("%s: 创建 Channel 失败: %v, 5秒后重试", name, err)
			// 休眠。
			time.Sleep(5 * time.Second)
			// 继续下一轮。
			continue
		}
		// Qos 预取 50 条：本 worker 最多同时有 50 条未确认消息（worker 进程吞吐可设大些）。
		if err := ch.Qos(50, 0, false); err != nil {
			// 失败只记日志，不致命。
			log.Printf("%s: QoS 设置失败: %v", name, err)
		}

		// 启动日志。
		log.Printf("%s started, consuming", name)
		// fn(ch) 跑该 worker 的 Run，通常会一直阻塞；只有 Channel 断/出错才返回 err。
		if err := fn(ch); err != nil {
			// ctx 已取消导致的退出：关掉 Channel 后直接结束（这是正常关停，不当错误重连）。
			if ctx.Err() != nil {
				// Close。
				ch.Close()
				// 返回。
				return
			}
			// 真正的连接错误：记日志，下面睡 5 秒重连。
			log.Printf("%s: %v, 5秒后重连...", name, err)
		}
		// 关闭失效 Channel。
		ch.Close()
		// 等待后回到外层循环开新 Channel。
		time.Sleep(5 * time.Second)
	}
}

// main 是 worker 进程的主函数。
func main() {
	// 加载 .env（本地开发）
	// godotenv.Load 读当前目录 .env 进环境变量；找不到文件也不致命。
	if err := godotenv.Load(); err != nil {
		// 记录后继续（可能用系统环境变量/默认配置）。
		log.Println(".env not found; continuing")
	}
	// 加载配置
	// configPath 先看环境变量 CONFIG_PATH。
	configPath := os.Getenv("CONFIG_PATH")
	// 没指定：用默认相对路径。
	if configPath == "" {
		// 默认配置文件位置。
		configPath = "configs/config.yaml"
	}
	// 记录配置来源。
	log.Printf("Loading config from %s", configPath)
	// LoadLocalDev 加载配置；cfg 配置对象、usedDefault 是否因文件缺失而用了内置默认、err 错误。
	cfg, usedDefault, err := config.LoadLocalDev(configPath)
	if err != nil {
		// 配置都加载不了：致命退出。
		log.Fatalf("Failed to load config: %v", err)
	}
	// 用了默认配置：提示一声。
	if usedDefault {
		// 记录。
		log.Printf("Config File %s not found, using default local config", configPath)
	} else {
		// 正常读到文件。
		log.Printf("Config loaded from file: %s", configPath)
	}
	// 连接数据库（带重试）
	// sqlDB 声明 GORM 连接。
	var sqlDB *gorm.DB
	// connectWithRetry 尝试 10 次连接 MySQL。
	connectWithRetry("MySQL", 10, func() error {
		// err 本次连接错误。
		var err error
		// db.NewDB 按配置建连接池并赋给外层 sqlDB。
		sqlDB, err = db.NewDB(cfg.Database)
		// 返回错误供重试判断。
		return err
	})
	// defer：进程退出时关闭数据库连接池。
	defer db.CloseDB(sqlDB)

	// 连接 Redis（用于流行度更新）
	// cache 按配置创建 Redis 客户端；err 错误。
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		// 配置有误：Redis 可选，置 nil（热度 worker 会被禁用）。
		log.Printf("Redis config error (popularity worker disabled): %v", err)
		// 置空。
		cache = nil
	} else {
		// pingCtx 300ms 超时探活；cancel 释放。
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		// defer 取消（注意本处 cancel 要到 main 返回才触发）。
		defer cancel()
		// Ping 探活；err 错误。
		if err := cache.Ping(pingCtx); err != nil {
			// Redis 没起来：关掉客户端、置 nil，热度 worker 不启动。
			log.Printf("Redis not available (popularity worker disabled): %v", err)
			// Close 释放。
			_ = cache.Close()
			// 置空。
			cache = nil
		} else {
			// 连通：进程退出时关闭连接。
			defer cache.Close()
			// 记录热度 worker 将启用。
			log.Printf("Redis connected (popularity worker enabled)")
		}
	}
	// 连接 RabbitMQ（带重试）
	// url 拼 AMQP 连接串：amqp://用户名:密码@主机:端口/虚拟主机
	url := "amqp://" + cfg.RabbitMQ.Username + ":" + cfg.RabbitMQ.Password + "@" + cfg.RabbitMQ.Host + ":" + strconv.Itoa(cfg.RabbitMQ.Port) + "/"
	// conn 声明 RabbitMQ 连接。
	var conn *amqp.Connection
	// 尝试 10 次拨号。
	connectWithRetry("RabbitMQ", 10, func() error {
		// err 本次拨号错误。
		var err error
		// amqp.Dial 建立 TCP 连接并赋给外层 conn。
		conn, err = amqp.Dial(url)
		// 返回错误。
		return err
	})
	// defer：退出时关闭连接。
	defer conn.Close()

	// 用临时 Channel 声明拓扑（持久化队列，声明一次即可）
	// topoCh 开一个专门做"声明"的 Channel；err 错误。
	topoCh, err := conn.Channel()
	if err != nil {
		// 开不了频道：致命退出。
		log.Fatalf("Failed to open topology channel: %v", err)
	}
	// 声明社交交换机/队列/绑定；err 错误。
	if err := declareSocialTopology(topoCh); err != nil {
		// 失败：致命。
		log.Fatalf("Failed to declare social topology: %v", err)
	}
	// 声明点赞拓扑。
	if err := declareLikeTopology(topoCh); err != nil {
		// 致命。
		log.Fatalf("Failed to declare like topology: %v", err)
	}
	// 声明评论拓扑。
	if err := declareCommentTopology(topoCh); err != nil {
		// 致命。
		log.Fatalf("Failed to declare comment topology: %v", err)
	}
	// Redis 可用时才声明热度拓扑（否则热度 worker 不跑）。
	if cache != nil {
		// 声明热度交换机/队列。
		if err := declarePopularityTopology(topoCh); err != nil {
			// 致命。
			log.Fatalf("Failed to declare popularity topology: %v", err)
		}
	}
	// 声明内容安全交换机/队列/绑定（不依赖 Redis）。
	if err := declareModerationTopology(topoCh); err != nil {
		// 致命。
		log.Fatalf("Failed to declare moderation topology: %v", err)
	}
	// 拓扑声明完，关掉临时 Channel。
	topoCh.Close()

	// 准备 repo
	// socialRepo 关注仓储。
	socialRepo := social.NewSocialRepository(sqlDB)
	// videoRepo 视频仓储。
	videoRepo := video.NewVideoRepository(sqlDB)
	// likeRepo 点赞仓储。
	likeRepo := video.NewLikeRepository(sqlDB)
	// commentRepo 评论仓储。
	commentRepo := video.NewCommentRepository(sqlDB)
	// caseRepo 审核工单仓储。
	caseRepo := moderation.NewCaseRepository(sqlDB)
	// reportRepo 举报仓储（回填工单编号、统计举报频次）。
	reportRepo := moderation.NewReportRepository(sqlDB)
	// auditRepo 审计仓储（D10）：工单全链路写流水。
	auditRepo := moderation.NewAuditRepository(sqlDB)
	// accountRepo 用户仓储（供 get_account 工具查作者资料）。
	accountRepo := account.NewAccountRepository(sqlDB)
	// messageRepo 私信仓储（D6 warn 动作：平台给作者发警告私信）。
	messageRepo := message.NewRepository(sqlDB)

	// signal.NotifyContext 返回一个【收到系统信号就自动取消】的 ctx；
	// 监听 os.Interrupt（Ctrl+C）和 SIGTERM（容器/系统 kill）。stop 是手动取消函数。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// defer：函数退出时恢复默认信号行为。
	defer stop()

	// pprofServer 启动 worker 的性能分析服务；err 错误。
	pprofServer, err := observability.NewPprofServer(
		// 服务标签 "Worker"。
		"Worker",
		// 是否启用（配置里可关）。
		cfg.ObservabilityConfig.Pprof.Enabled,
		// worker pprof 监听地址。
		cfg.ObservabilityConfig.Pprof.WorkerAddr,
	)
	if err != nil {
		// pprof 起不来只记日志，不影响消费主流程。
		log.Printf("Failed to start worker pprof server: %v", err)
	}
	// 成功创建：退出时关闭。
	if pprofServer != nil {
		// Close。
		defer pprofServer.Close()
	}

	// 构建 4 个只读查询工具（薄封装现有仓储）：
	queryTools := []agent.Tool{
		// get_video 查视频详情。
		agent.NewGetVideoTool(videoRepo),
		// get_video_comments 查视频评论。
		agent.NewGetCommentsTool(commentRepo),
		// get_account 查作者资料。
		agent.NewGetAccountTool(accountRepo),
		// count_target_reports 查举报频次。
		agent.NewCountReportsTool(reportRepo),
	}

	// 构建并发取证器：复用同一批仓储（对象详情/评论/作者/举报频次/作者近期视频）。
	evidenceCollector := agent.NewEvidenceCollector(videoRepo, commentRepo, accountRepo, reportRepo)

	// 构建内容安全 Agent：
	// moderationAgent 按 cfg.Agent 和工具创建；err 错误。
	// 未启用时 NewAgent 返回 (nil,nil)；启用但 Key/配置缺失返回错误。
	moderationAgent, err := agent.NewAgent(cfg.Agent, queryTools...)
	if err != nil {
		// 配置有误：不致命，记日志并按"未启用"处理（只建空工单）。
		log.Printf("ModerationAgent init failed (agent disabled): %v", err)
		// 置 nil。
		moderationAgent = nil
	}

	// 构建处置动作执行器（D6）：删视频/评论、发警告私信。
	// cache 为 nil（Redis 没连上）时执行器仍会构建，但所有自动处置都会因拿不到锁而转人工。
	actionExecutor := agent.NewActionExecutor(videoRepo, commentRepo, messageRepo, cache, cfg.Agent.Thresholds)

	// 每个 Worker 独立 Channel + 自动重连
	// go 开 goroutine 跑社交 worker：闭包里 new 一个 SocialWorker 并 Run。
	go runWorkerWithRetry(ctx, "SocialWorker", conn, func(ch *amqp.Channel) error {
		// 返回 Run 的结果（阻塞消费）。
		return worker.NewSocialWorker(ch, socialRepo, socialQueue).Run(ctx)
	})
	// 点赞 worker。
	go runWorkerWithRetry(ctx, "LikeWorker", conn, func(ch *amqp.Channel) error {
		// LikeWorker 需要点赞仓储和视频仓储。
		return worker.NewLikeWorker(ch, likeRepo, videoRepo, likeQueue).Run(ctx)
	})
	// 评论 worker。
	go runWorkerWithRetry(ctx, "CommentWorker", conn, func(ch *amqp.Channel) error {
		// CommentWorker 需要评论仓储和视频仓储。
		return worker.NewCommentWorker(ch, commentRepo, videoRepo, commentQueue).Run(ctx)
	})
	// Redis 可用时才启动热度 worker。
	if cache != nil {
		// PopularityWorker。
		go runWorkerWithRetry(ctx, "PopularityWorker", conn, func(ch *amqp.Channel) error {
			// 注入缓存和热度队列。
			return worker.NewPopularityWorker(ch, cache, popularityQueue).Run(ctx)
		})
	}
	// rmqWrapper 把裸 amqp 连接包成中间件类型（worker main 全程用裸 conn，
	// 只有 NewModerationMQ 需要 RabbitMQ 包装）。
	rmqWrapper := &mqrabbit.RabbitMQ{Conn: conn}
	// moderationNotifyMQ 审核台通知发布器（D7）：转人工时给 Web 进程发 moderation.pending。
	moderationNotifyMQ, err := mqrabbit.NewModerationMQ(rmqWrapper)
	if err != nil {
		// 不致命：没有发布器时只更新举报状态，审核员仍能从列表看到工单。
		log.Printf("ModerationNotifyMQ init failed (reviewer push disabled): %v", err)
		// 置 nil。
		moderationNotifyMQ = nil
	}

	// 内容安全 worker：消费举报事件、建空工单。
	go runWorkerWithRetry(ctx, "ModerationWorker", conn, func(ch *amqp.Channel) error {
		// 注入工单/举报仓储、Agent、取证器、处置执行器、通知发布器、审计、私信和内容安全队列。
		return worker.NewModerationWorker(ch, caseRepo, reportRepo, moderationAgent, evidenceCollector,
			actionExecutor, moderationNotifyMQ, auditRepo, messageRepo, moderationQueue).Run(ctx)
	})

	// 等待退出信号
	// 阻塞在这里，直到 ctx 被信号取消。
	<-ctx.Done()
	// 记录开始关停。
	log.Printf("Worker shutting down...")
	// 给 2 秒缓冲，让正在处理的消息尽量完成（简单的优雅停机）。
	time.Sleep(2 * time.Second) // 等待正在处理的消息完成
	// 记录已停止（随后 defer 依次关闭连接）。
	log.Printf("Worker stopped")
}

// declareSocialTopology 是普通函数：声明社交交换机、队列（带死信）、并把队列绑到交换机。
//
// 参数 ch：用于声明的 Channel；
// 返回值 error：声明失败。
func declareSocialTopology(ch *amqp.Channel) error {
	// ExchangeDeclare 声明交换机。
	if err := ch.ExchangeDeclare(
		// 交换机名。
		socialExchange,
		// 类型 topic：按 routing key 模式匹配。
		"topic",
		// durable=true：RabbitMQ 重启后交换机还在。
		true,
		// autoDelete=false：不自动删除。
		false,
		// internal=false。
		false,
		// noWait=false：等服务器确认。
		false,
		// args 无。
		nil,
	); err != nil {
		// 失败返回。
		return err
	}

	// QueueDeclare 声明队列；q 队列信息（含服务器生成的名字）、err 错误。
	q, err := ch.QueueDeclare(
		// 队列名。
		socialQueue,
		// durable=true：队列持久化。
		true,
		// autoDelete=false。
		false,
		// exclusive=false：别的连接也能用。
		false,
		// noWait=false。
		false,
		// 关键参数：给队列绑定死信交换机——消息被 reject/过期后会转投到 DLXExchange。
		amqp.Table{"x-dead-letter-exchange": mqrabbit.DLXExchange},
	)
	if err != nil {
		// 失败返回。
		return err
	}

	// QueueBind 把队列按绑定键绑到交换机。
	if err := ch.QueueBind(
		// q.Name 队列名。
		q.Name,
		// 绑定键 social.*。
		socialBindingKey,
		// 交换机名。
		socialExchange,
		// noWait=false。
		false,
		// args 无。
		nil,
	); err != nil {
		// 失败返回。
		return err
	}
	// 全部成功。
	return nil
}

// declarePopularityTopology 是普通函数：声明热度交换机/队列/绑定，结构同上。
//
// 参数 ch：声明用 Channel；
// 返回值 error：声明错误。
func declarePopularityTopology(ch *amqp.Channel) error {
	// ExchangeDeclare 热度 topic 交换机（持久化）。
	if err := ch.ExchangeDeclare(
		// 名字。
		popularityExchange,
		// topic。
		"topic",
		// durable。
		true,
		// autoDelete。
		false,
		// internal。
		false,
		// noWait。
		false,
		// args。
		nil,
	); err != nil {
		// 返回。
		return err
	}

	// QueueDeclare 热度队列（带死信参数）；q、err。
	q, err := ch.QueueDeclare(
		// 队列名。
		popularityQueue,
		// durable。
		true,
		// autoDelete。
		false,
		// exclusive。
		false,
		// noWait。
		false,
		// 死信交换机参数。
		amqp.Table{"x-dead-letter-exchange": mqrabbit.DLXExchange},
	)
	if err != nil {
		// 返回。
		return err
	}

	// QueueBind 直接返回其结果：队列按 video.popularity.* 绑到热度交换机。
	return ch.QueueBind(
		// 队列名。
		q.Name,
		// 绑定键。
		popularityBindingKey,
		// 交换机。
		popularityExchange,
		// noWait。
		false,
		// args。
		nil,
	)
}

// declareLikeTopology 是普通函数：声明点赞交换机/队列/绑定。
//
// 参数 ch：声明用 Channel；
// 返回值 error。
func declareLikeTopology(ch *amqp.Channel) error {
	// ExchangeDeclare 点赞 topic 交换机。
	if err := ch.ExchangeDeclare(
		// 名字。
		likeExchange,
		// topic。
		"topic",
		// durable。
		true,
		// autoDelete。
		false,
		// internal。
		false,
		// noWait。
		false,
		// args。
		nil,
	); err != nil {
		// 返回。
		return err
	}

	// QueueDeclare 点赞队列（死信参数）；q、err。
	q, err := ch.QueueDeclare(
		// 队列名。
		likeQueue,
		// durable。
		true,
		// autoDelete。
		false,
		// exclusive。
		false,
		// noWait。
		false,
		// 死信交换机。
		amqp.Table{"x-dead-letter-exchange": mqrabbit.DLXExchange},
	)
	if err != nil {
		// 返回。
		return err
	}

	// QueueBind 返回绑定结果：like.* → 点赞交换机。
	return ch.QueueBind(
		// 队列。
		q.Name,
		// 绑定键。
		likeBindingKey,
		// 交换机。
		likeExchange,
		// noWait。
		false,
		// args。
		nil,
	)
}

// declareCommentTopology 是普通函数：声明评论交换机/队列/绑定。
//
// 参数 ch：声明用 Channel；
// 返回值 error。
func declareCommentTopology(ch *amqp.Channel) error {
	// ExchangeDeclare 评论 topic 交换机。
	if err := ch.ExchangeDeclare(
		// 名字。
		commentExchange,
		// topic。
		"topic",
		// durable。
		true,
		// autoDelete。
		false,
		// internal。
		false,
		// noWait。
		false,
		// args。
		nil,
	); err != nil {
		// 返回。
		return err
	}

	// QueueDeclare 评论队列（死信参数）；q、err。
	q, err := ch.QueueDeclare(
		// 队列名。
		commentQueue,
		// durable。
		true,
		// autoDelete。
		false,
		// exclusive。
		false,
		// noWait。
		false,
		// 死信交换机。
		amqp.Table{"x-dead-letter-exchange": mqrabbit.DLXExchange},
	)
	if err != nil {
		// 返回。
		return err
	}

	// QueueBind 返回绑定结果：comment.* → 评论交换机。
	return ch.QueueBind(
		// 队列。
		q.Name,
		// 绑定键。
		commentBindingKey,
		// 交换机。
		commentExchange,
		// noWait。
		false,
		// args。
		nil,
	)
}

// declareModerationTopology 是普通函数：声明内容安全交换机/队列（带死信）/绑定。
//
// 参数 ch：声明用 Channel；
// 返回值 error：声明错误。
func declareModerationTopology(ch *amqp.Channel) error {
	// ExchangeDeclare 内容安全 topic 交换机（持久化）。
	if err := ch.ExchangeDeclare(
		// 名字。
		moderationExchange,
		// topic。
		"topic",
		// durable。
		true,
		// autoDelete。
		false,
		// internal。
		false,
		// noWait。
		false,
		// args。
		nil,
	); err != nil {
		// 返回。
		return err
	}

	// QueueDeclare 内容安全队列（带死信参数）；q、err。
	q, err := ch.QueueDeclare(
		// 队列名。
		moderationQueue,
		// durable。
		true,
		// autoDelete。
		false,
		// exclusive。
		false,
		// noWait。
		false,
		// 死信交换机。
		amqp.Table{"x-dead-letter-exchange": mqrabbit.DLXExchange},
	)
	if err != nil {
		// 返回。
		return err
	}

	// QueueBind 返回绑定结果：report.* → 内容安全交换机。
	return ch.QueueBind(
		// 队列。
		q.Name,
		// 绑定键。
		moderationBindingKey,
		// 交换机。
		moderationExchange,
		// noWait。
		false,
		// args。
		nil,
	)
}
