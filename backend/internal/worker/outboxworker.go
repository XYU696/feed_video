// package worker：后台工作者包。本文件有两个后台任务：
//  1. StartOutboxPoller：轮询 outbox_msgs 表，把发视频事件可靠地投递到 MQ（Outbox 模式）；
//  2. StartConsumer：消费"时间线"队列，把新视频写进 Redis 全局时间线 ZSET 并只保留最新 1000 条。
package worker

import (
	// context：后台操作的上下文。
	"context"
	// encoding/json：反序列化时间线事件。
	"encoding/json"
	// rabbitmq：TimelineMQ 发布器、RabbitMQ 连接、TimelineEvent。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：项目封装的 Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// video：OutboxMsg 结构体。
	"feedsystem_video_go/internal/video"
	// fmt：数字 ID 转字符串。
	"fmt"
	// log：错误日志。
	"log"
	// time：空轮询休眠、重连退避、超时。
	"time"

	// oredis：go-redis 客户端，用到 oredis.Z（ZSET 元素）。
	oredis "github.com/redis/go-redis/v9"
	// gorm：数据库。
	"gorm.io/gorm"
)

// StartOutboxPoller 是普通函数：启动一个后台 goroutine 持续轮询 Outbox 表。即轮询器
//
// 为什么需要 Outbox（发件箱模式）？发视频时要同时做两件事：写 MySQL（视频记录）
// 和发 MQ（通知时间线）。这俩无法放在同一个事务里，可能"库写成功了、MQ 没发出去"。
// 解法：发视频的事务里顺带往 outbox_msgs 插一条"待发"记录；本函数再异步把它投递到 MQ，
// 投递成功才删除记录。这样即使 MQ 暂时挂了，重启/恢复后也能补发，保证消息不丢。
//
// 参数：db 数据库；tmq 时间线 MQ 发布器。
func StartOutboxPoller(db *gorm.DB, tmq *rabbitmq.TimelineMQ) {
	// 数据库或 MQ 没初始化：不启动。
	if db == nil || tmq == nil {
		// 记录禁用原因。
		log.Printf("Outbox poller disabled: timeline mq is not initialized")
		// 直接返回。
		return
	}

	// go 开启后台 goroutine，永不返回（随进程生命周期）。
	go func() {
		// 无限循环轮询。
		for {
			// messages 本批待投递的 outbox 记录。
			var messages []video.OutboxMsg

			// 查 status=pending 的记录，按创建时间【升序】（先发的先投），每批 100 条；err 错误。
			err := db.Where("status = ?", "pending").Order("create_time ASC").Limit(100).Find(&messages).Error

			// 查询出错，或没有待发消息。
			if err != nil || len(messages) == 0 {
				// 睡 1 秒再查，避免空转把数据库打满。
				time.Sleep(1 * time.Second)
				// 跳过本轮。
				continue
			}

			// 逐条投递；msg 当前记录。
			for _, msg := range messages {
				// PublishVideo 把"新视频"事件发到 MQ；err 错误。
				err := tmq.PublishVideo(context.Background(), msg.VideoID, msg.CreateTime)

				// 投递成功。
				if err == nil {
					// 删除这条 outbox 记录，表示已完成；err 接收删除错误。
					if err := db.Delete(&msg).Error; err != nil {
						// 删除失败只记日志（下轮会重复投递，靠消费端幂等兜底）。
						log.Printf("删除 outbox 消息失败: id=%d, err=%v", msg.ID, err)
					}
				} else {
					// 投递失败：记录保留，下一轮会重试（这就是"至少一次"投递）。
					log.Printf("投递MQ失败: VideoID: %d, err: %v", msg.VideoID, err)
				}
			}
		}
	}()
}

// StartConsumer 是普通函数：启动"时间线"消费者，带断线无限重连。
//
// 消费到新视频事件后做两件事：
//  1. ZADD 把视频加进全局时间线 ZSET（分数=发布时间）；
//  2. ZREMRANGEBYRANK 删掉排名 1000 名开外的，只留最新 1000 条（冷热分离的"热"部分）。
//
// 参数：tmq 时间线 MQ（本函数仅用于非空判断）；queueName 队列名；
// redisClient Redis 客户端；rmq RabbitMQ 连接（用来开 Channel）。
func StartConsumer(tmq *rabbitmq.TimelineMQ, queueName string, redisClient *rediscache.Client, rmq *rabbitmq.RabbitMQ) {
	// MQ 或其连接没就绪：不启动。
	if tmq == nil || rmq == nil || rmq.Conn == nil {
		// 记录原因。
		log.Printf("Timeline consumer disabled: rabbitmq is not initialized")
		// 返回。
		return
	}
	// Redis 没就绪：不启动（没地方写时间线）。
	if redisClient == nil {
		// 记录。
		log.Printf("Timeline consumer disabled: redis is not initialized")
		// 返回。
		return
	}

	// 后台 goroutine。
	go func() {
		// 外层循环负责"断线重连"。
		for {
			// 每次重连创建独立的 Channel，不与发布者共用
			// NewChannel 在现有 TCP 连接上开一个新频道；ch 频道、err 错误。
			ch, err := rmq.NewChannel()
			if err != nil {
				// 开频道失败：记日志，5 秒后再试。
				log.Printf("Timeline consumer: 创建 Channel 失败: %v, 5秒后重试", err)
				// 休眠。
				time.Sleep(5 * time.Second)
				// 继续外层循环重连。
				continue
			}

			// Qos 限制本消费者【最多同时】收 10 条未确认消息（流控，防止一次性灌爆）；
			// 第二个参数 0 表示不限字节，false 表示对整个 Channel 生效。
			if err := ch.Qos(10, 0, false); err != nil {
				// 设置失败只记日志，不致命（继续消费）。
				log.Printf("Timeline consumer: QoS 设置失败: %v", err)
			}

			// Consume 注册消费者；msgs 是消息 Go channel；err 错误。
			msgs, err := ch.Consume(queueName, "", false, false, false, false, nil)
			if err != nil {
				// 注册失败：关掉刚开的频道，5 秒后重连。
				log.Printf("Timeline consumer: 注册消费失败: %v, 5秒后重试", err)
				// Close 释放频道。
				ch.Close()
				// 休眠。
				time.Sleep(5 * time.Second)
				// 重连。
				continue
			}

			// 启动成功日志。
			log.Printf("Timeline consumer 已启动, queue=%s", queueName)

			// for-range 持续从 msgs 取消息；msg 当前消息。channel 关闭时循环自动退出。
			for msg := range msgs {
				// event 准备接收时间线事件。
				var event rabbitmq.TimelineEvent
				// 反序列化消息体；err 错误。
				if err := json.Unmarshal(msg.Body, &event); err != nil {
					// 坏消息没法处理：记日志后直接 Ack 丢弃（不重投）。
					log.Printf("Timeline consumer: 反序列化失败: %v", err)
					// Ack。
					msg.Ack(false)
					// 跳过本条。
					continue
				}

				// ctx 500ms 超时上下文（写 Redis 用）；cancel 释放。
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				// timelineKey 全局时间线的 Redis 键。
				timelineKey := redisClient.Key("feed:global_timeline")
				// ZAdd 把新视频加进 ZSET；err 错误。
				// 键是 timelineKey，分数是 event.CreateTime，成员是视频 ID 字符串。
				// 根据分数排序，分数越大越靠前（最新的视频在前面）。
				err = redisClient.ZAdd(ctx, timelineKey, oredis.Z{
					// Score 分数=事件里的发布时间（排序依据）。
					Score: float64(event.CreateTime),
					// Member 成员=视频 ID 字符串。
					Member: fmt.Sprintf("%d", event.VideoID),
				})

				// 写 ZSET 失败。
				if err != nil {
					// 记日志。
					log.Printf("Timeline consumer: 写入Zset失败: %v", err)
					// Nack(requeue=true)：放回队列稍后重试。
					msg.Nack(false, true)
					// 释放 ctx。
					cancel()
					// 跳过本条。
					continue
				}

				// ZRemRangeByRank 按排名删除：0 到 -1001。
				// 负下标 -1001 表示"倒数第 1001 名"，即保留分数最高的最新 1000 条、删掉更老的；err 错误。
				if err := redisClient.ZRemRangeByRank(ctx, timelineKey, 0, -1001); err != nil {
					// 裁剪失败只记日志（下条消息会再裁），不影响本条 ACK。
					log.Printf("Timeline consumer: ZRem失败: %v", err)
				}

				// 处理成功：Ack 确认。
				msg.Ack(false)
				// 释放 ctx。
				cancel()
			}

			// msgs channel 关闭说明 AMQP Channel 断开，关闭并重连
			// Close 关掉失效频道。
			ch.Close()
			// 记日志：准备重连。
			log.Printf("Timeline consumer: Channel 断开, 5秒后重连...")
			// 等 5 秒，回到外层循环开新 Channel。
			time.Sleep(5 * time.Second)
		}
	}()
}
