// package worker：后台工作者包。本文件是 LikeWorker——
// 消费点赞/取消点赞事件，把数据真正写进 MySQL。
//
// 它是"MQ 异步"链路的终点：API 收到点赞请求后只发 MQ 就立刻返回，
// 本 worker 在后台慢慢把"点赞记录 + 视频点赞数 + 热度"更新到数据库。
// 全程保证幂等：重复点赞、重复取消都不会造成计数错误。
package worker

import (
	// context：上下文。
	"context"
	// encoding/json：反序列化点赞事件。
	"encoding/json"
	// errors：初始化错误。
	"errors"
	// rabbitmq：LikeEvent 事件结构。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// video：LikeRepository、VideoRepository、Like 模型。
	"feedsystem_video_go/internal/video"
	// log：重试日志。
	"log"
	// time：退避、点赞时间。
	"time"

	// amqp：RabbitMQ 客户端。
	amqp "github.com/rabbitmq/amqp091-go"
)

// LikeWorker 结构体：点赞工作者。
type LikeWorker struct {
	// ch RabbitMQ 频道。
	ch *amqp.Channel
	// likes 点赞仓储：插/删点赞记录。
	likes *video.LikeRepository
	// videos 视频仓储：改点赞数、热度、判断视频是否存在。
	videos *video.VideoRepository
	// queue 队列名。
	queue string
}

// NewLikeWorker 是构造函数：创建点赞工作者。
//
// 参数：ch 频道、likes 点赞仓储、videos 视频仓储、queue 队列名；
// 返回值 *LikeWorker。
func NewLikeWorker(ch *amqp.Channel, likes *video.LikeRepository, videos *video.VideoRepository, queue string) *LikeWorker {
	// 装配返回。
	return &LikeWorker{ch: ch, likes: likes, videos: videos, queue: queue}
}

// Run 方法：启动消费循环。
//
// 参数 ctx：生命周期控制；
// 返回值 error：初始化失败/通道关闭的错误。
func (w *LikeWorker) Run(ctx context.Context) error {
	// 依赖完整性检查。
	if w == nil || w.ch == nil || w.likes == nil || w.videos == nil {
		// 返回未初始化错误。
		return errors.New("like worker is not initialized")
	}
	// 队列名必填。
	if w.queue == "" {
		// 返回。
		return errors.New("queue is required")
	}

	// Consume 注册消费者；deliveries 消息 channel；err 错误。
	deliveries, err := w.ch.Consume(
		// 队列名。
		w.queue,
		// 消费者标签自动生成。
		"",
		// autoAck=false：手动确认。
		false,
		// 非独占。
		false,
		// noLocal=false。
		false,
		// noWait=false。
		false,
		// 额外参数无。
		nil,
	)
	if err != nil {
		// 注册失败返回。
		return err
	}

	// 消费主循环。
	for {
		// select 等停止或来消息。
		select {
		// 关停信号。
		case <-ctx.Done():
			// 返回 ctx 错误。
			return ctx.Err()
		// 来消息；d 消息、ok 通道是否开着。
		case d, ok := <-deliveries:
			// 通道关闭（断线）。
			if !ok {
				// 返回错误让外层重连。
				return errors.New("deliveries channel closed")
			}
			// 带重试处理。
			w.handleDelivery(ctx, d)
		}
	}
}

// handleDelivery 方法：处理单条点赞消息，失败指数退避重试 3 次。
//
// 参数：ctx 上下文；d 消息；
// 无返回值。
func (w *LikeWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	// maxRetries 最大重试次数。
	const maxRetries = 3
	// i 从 0 到 3。
	for i := 0; i <= maxRetries; i++ {
		// 非阻塞检查停止信号。
		select {
		// 已取消。
		case <-ctx.Done():
			// Nack 放回队列不丢。
			_ = d.Nack(false, true)
			// 结束。
			return
		// 没取消，继续。
		default:
		}
		// process 处理消息体；err 错误。
		if err := w.process(ctx, d.Body); err != nil {
			// 重试次数耗尽。
			if i >= maxRetries {
				// 记日志并 Ack 丢弃毒消息。
				log.Printf("like worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
				// Ack。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避 1、2、4 秒。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录重试信息。
			log.Printf("like worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
			// 休眠。
			time.Sleep(wait)
			// 重试。
			continue
		}
		// 成功：Ack。
		_ = d.Ack(false)
		// 结束。
		return
	}
}

// process 方法：解析点赞事件并按动作类型分发。
//
// 参数：ctx 上下文；body 消息体；
// 返回值 error：处理错误（格式问题返回 nil 直接跳过）。
func (w *LikeWorker) process(ctx context.Context, body []byte) error {
	// evt 准备接收点赞事件。
	var evt rabbitmq.LikeEvent
	// 反序列化；err 错误。
	if err := json.Unmarshal(body, &evt); err != nil {
		// 解析事件失败，直接丢弃
		// 返回 nil：坏消息不重试，Ack 掉。
		return nil
	}
	// 用户 ID 或视频 ID 缺失：无效事件，忽略。
	if evt.UserID == 0 || evt.VideoID == 0 {
		// 返回 nil。
		return nil
	}

	// switch 按动作字段分发。
	switch evt.Action {
	// 点赞。
	case "like":
		// 执行点赞落库。
		return w.applyLike(ctx, evt.UserID, evt.VideoID)
	// 取消点赞。
	case "unlike":
		// 执行取消点赞落库。
		return w.applyUnlike(ctx, evt.UserID, evt.VideoID)
	// 其他未知动作：忽略。
	default:
		// 返回 nil。
		return nil
	}
}

// applyLike 方法：把一次点赞真正写进数据库（点赞记录 + 点赞数 +1 + 热度 +1）。
//
// 参数：ctx 上下文；userID 点赞用户；videoID 被点赞视频；
// 返回值 error：写库错误。
func (w *LikeWorker) applyLike(ctx context.Context, userID, videoID uint) error {
	// ok 视频是否存在；err 错误。
	ok, err := w.videos.IsExist(ctx, videoID)
	if err != nil {
		// 查询出错：返回以重试。
		return err
	}
	// 视频已被删除：点赞无意义，直接当成功（消息被 Ack 掉）。
	if !ok {
		// 返回 nil。
		return nil
	}

	// LikeIgnoreDuplicate 插入点赞记录；created 表示是否真的新插入了一条
	// （内部遇到重复键 1062 会吞掉错误并返回 false，实现幂等）；err 错误。
	created, err := w.likes.LikeIgnoreDuplicate(ctx, &video.Like{
		// VideoID 视频编号。
		VideoID: videoID,
		// AccountID 点赞用户。
		AccountID: userID,
		// CreatedAt 点赞时间（服务器时间）。
		CreatedAt: time.Now(),
	})
	if err != nil {
		// 插入出错（非重复）：返回重试。
		return err
	}
	// 不是新建的（之前已点赞过）：计数不能再加，直接结束。
	if !created {
		// 返回 nil——重复点赞幂等。
		return nil
	}

	// ChangeLikesCount 把视频点赞数 +1（SQL 里 likes_count = likes_count + 1）；err 错误。
	if err := w.videos.ChangeLikesCount(ctx, videoID, 1); err != nil {
		// 失败返回重试。
		return err
	}
	// ChangePopularity 把热度 +1，并返回其错误。
	return w.videos.ChangePopularity(ctx, videoID, 1)
}

// applyUnlike 方法：把一次取消点赞写进数据库（删记录 + 点赞数 -1 + 热度 -1）。
//
// 参数：ctx 上下文；userID 用户；videoID 视频；
// 返回值 error：写库错误。
func (w *LikeWorker) applyUnlike(ctx context.Context, userID, videoID uint) error {
	// ok 视频是否存在；err 错误。
	ok, err := w.videos.IsExist(ctx, videoID)
	if err != nil {
		// 出错返回。
		return err
	}
	// 视频不存在：无需操作。
	if !ok {
		// 返回 nil。
		return nil
	}

	// DeleteByVideoAndAccount 删除点赞记录；deleted 表示是否真的删到了一条
	// （RowsAffected==0 时为 false）；err 错误。
	deleted, err := w.likes.DeleteByVideoAndAccount(ctx, videoID, userID)
	if err != nil {
		// 删除出错：返回重试。
		return err
	}
	// 本来就没点赞过：计数不能再减，直接结束（幂等）。
	if !deleted {
		// 返回 nil。
		return nil
	}

	// ChangeLikesCount 点赞数 -1（仓储里用 GREATEST(...,0)，不会减成负数）；err 错误。
	if err := w.videos.ChangeLikesCount(ctx, videoID, -1); err != nil {
		// 失败返回。
		return err
	}
	// ChangePopularity 热度 -1，返回错误。
	return w.videos.ChangePopularity(ctx, videoID, -1)
}
