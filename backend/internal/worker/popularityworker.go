// package worker：后台工作者包。本文件是 PopularityWorker——
// 消费"热度变化"事件，把变化更新到 Redis 的热度结构里。
//
// 它是视频热度【实时缓存】链路的消费者：点赞/评论服务在直接写 Redis 失败时，
// 会改发"热度事件"到 MQ；本 worker 收到后调用 video.UpdatePopularityCache，
// 删掉旧详情缓存、并给当前分钟桶 ZINCRBY。逻辑很薄，主要价值在削峰和兜底。
package worker

import (
	// context：上下文。
	"context"
	// encoding/json：反序列化热度事件。
	"encoding/json"
	// errors：初始化报错。
	"errors"
	// rabbitmq：PopularityEvent 事件结构。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// video：UpdatePopularityCache 函数。
	"feedsystem_video_go/internal/video"
	// log：重试日志。
	"log"
	// time：退避等待。
	"time"

	// amqp：RabbitMQ 客户端。
	amqp "github.com/rabbitmq/amqp091-go"
)

// PopularityWorker 结构体：热度工作者。
type PopularityWorker struct {
	// ch RabbitMQ 频道。
	ch *amqp.Channel
	// cache Redis 客户端：更新分钟桶、删详情缓存。
	cache *rediscache.Client
	// queue 队列名。
	queue string
}

// NewPopularityWorker 是构造函数：创建热度工作者。
//
// 参数：ch 频道、cache Redis 客户端、queue 队列名；
// 返回值 *PopularityWorker。
func NewPopularityWorker(ch *amqp.Channel, cache *rediscache.Client, queue string) *PopularityWorker {
	// 装配返回。
	return &PopularityWorker{ch: ch, cache: cache, queue: queue}
}

// Run 方法：启动热度消费循环。
//
// 参数 ctx：生命周期控制；
// 返回值 error：初始化失败/通道关闭的错误。
func (w *PopularityWorker) Run(ctx context.Context) error {
	// 依赖检查。
	if w == nil || w.ch == nil || w.cache == nil {
		// 返回未初始化错误。
		return errors.New("popularity worker is not initialized")
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
		// 标签自动。
		"",
		// 手动确认。
		false,
		// 非独占。
		false,
		// noLocal。
		false,
		// noWait。
		false,
		// 无参数。
		nil,
	)
	if err != nil {
		// 失败返回。
		return err
	}

	// 主循环。
	for {
		// select 等停止或消息。
		select {
		// 关停。
		case <-ctx.Done():
			// 返回 ctx 错误。
			return ctx.Err()
		// 来消息；d 消息、ok 通道状态。
		case d, ok := <-deliveries:
			// 断线。
			if !ok {
				// 返回错误让外层重连。
				return errors.New("deliveries channel closed")
			}
			// 带重试处理。
			w.handleDelivery(ctx, d)
		}
	}
}

// handleDelivery 方法：处理单条热度消息，失败指数退避重试 3 次。
//
// 参数：ctx 上下文；d 消息；
// 无返回值。
func (w *PopularityWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	// maxRetries 最大重试次数。
	const maxRetries = 3
	// i 从 0 到 3。
	for i := 0; i <= maxRetries; i++ {
		// 非阻塞检查停止信号。
		select {
		// 已取消。
		case <-ctx.Done():
			// Nack 放回队列。
			_ = d.Nack(false, true)
			// 结束。
			return
		// 没取消。
		default:
		}
		// process 处理；err 错误。
		if err := w.process(ctx, d.Body); err != nil {
			// 重试耗尽。
			if i >= maxRetries {
				// 记日志、Ack 丢弃毒消息。
				log.Printf("popularity worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
				// Ack。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录。
			log.Printf("popularity worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
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

// process 方法：解析热度事件并更新 Redis 热度缓存。
//
// 参数：ctx 上下文；body 消息体；
// 返回值 error：始终返回 nil（缓存更新失败在底层被忽略，见 UpdatePopularityCache）。
func (w *PopularityWorker) process(ctx context.Context, body []byte) error {
	// evt 准备接收热度事件。
	var evt rabbitmq.PopularityEvent
	// 反序列化；err 错误。
	if err := json.Unmarshal(body, &evt); err != nil {
		// 坏消息：直接忽略。
		return nil
	}
	// 视频 ID 缺失，或变化量为 0（没有意义）：忽略。
	if evt.VideoID == 0 || evt.Change == 0 {
		// 返回 nil。
		return nil
	}
	// UpdatePopularityCache 更新 Redis：删该视频的详情/实体缓存，
	// 并给当前分钟桶 hot:video:1m:{分钟} 的该视频 ZINCRBY 加上 evt.Change。
	video.UpdatePopularityCache(ctx, w.cache, evt.VideoID, evt.Change)
	// 始终返回 nil：缓存操作内部已吞掉错误，不需要 MQ 重试。
	return nil
}
