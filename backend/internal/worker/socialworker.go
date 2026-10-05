// package worker：后台工作者包。本文件是 SocialWorker——
// 消费"关注 / 取关"事件，把关注关系写进/移出 socials 表。
//
// 注意定位：主流程里关注是【先同步写库】的（见 social/service.go），
// 所以这个 worker 多数时候处理的是"同步失败后转异步"或独立 worker 进程的场景。
// 关键点：关注时遇到 1062 重复键要当成功处理（关系已存在 = 幂等）。
package worker

import (
	// context：上下文。
	"context"
	// encoding/json：反序列化社交事件。
	"encoding/json"
	// errors：初始化报错、errors.As 提取 MySQL 错误。
	"errors"
	// rabbitmq：SocialEvent 事件结构。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// social：SocialRepository、Social 模型。
	"feedsystem_video_go/internal/social"
	// log：重试日志。
	"log"
	// time：退避等待。
	"time"

	// mysql：用来判断 1062 重复键错误。
	"github.com/go-sql-driver/mysql"
	// amqp：RabbitMQ 客户端。
	amqp "github.com/rabbitmq/amqp091-go"
)

// SocialWorker 结构体：关注工作者。
type SocialWorker struct {
	// ch RabbitMQ 频道。
	ch *amqp.Channel
	// repo 关注仓储：插/删关注关系。
	repo *social.SocialRepository
	// queue 队列名。
	queue string
}

// NewSocialWorker 是构造函数：创建关注工作者。
//
// 参数：ch 频道、repo 关注仓储、queue 队列名；
// 返回值 *SocialWorker。
func NewSocialWorker(ch *amqp.Channel, repo *social.SocialRepository, queue string) *SocialWorker {
	// 装配返回。
	return &SocialWorker{ch: ch, repo: repo, queue: queue}
}

// Run 方法：启动关注消费循环。
//
// 参数 ctx：生命周期控制；
// 返回值 error：初始化失败/通道关闭的错误。
func (w *SocialWorker) Run(ctx context.Context) error {
	// 依赖检查。
	if w == nil || w.ch == nil || w.repo == nil {
		// 返回未初始化错误。
		return errors.New("social worker is not initialized")
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

// handleDelivery 方法：处理单条关注消息，失败指数退避重试 3 次。
//
// 参数：ctx 上下文；d 消息；
// 无返回值。
func (w *SocialWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
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
				log.Printf("social worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
				// Ack。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录。
			log.Printf("social worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
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

// process 方法：解析关注事件并执行关注/取关。
//
// 参数：ctx 上下文；body 消息体；
// 返回值 error：处理错误（坏消息返回 nil 忽略）。
func (w *SocialWorker) process(ctx context.Context, body []byte) error {
	// evt 准备接收社交事件。
	var evt rabbitmq.SocialEvent
	// 反序列化；err 错误。
	if err := json.Unmarshal(body, &evt); err != nil {
		// 解析事件失败，直接丢弃
		// 返回 nil。
		return nil
	}
	// 粉丝或博主 ID 缺失：无效事件。
	if evt.FollowerID == 0 || evt.VloggerID == 0 {
		// 返回 nil。
		return nil
	}

	// switch 按动作分发。
	switch evt.Action {
	// 关注。
	case "follow":
		// repo.Follow 插入关注关系；err 错误。
		err := w.repo.Follow(ctx, &social.Social{
			// FollowerID 粉丝。
			FollowerID: evt.FollowerID,
			// VloggerID 被关注博主。
			VloggerID: evt.VloggerID,
		})
		// 插入成功：完成。
		if err == nil {
			// 返回 nil。
			return nil
		}
		// mysqlErr 准备接收 MySQL 具体错误。
		var mysqlErr *mysql.MySQLError
		// errors.As 把错误链里的 *mysql.MySQLError 提取到 mysqlErr；
		// 若是 1062 唯一键冲突，说明这条关注关系【已经存在】。
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			// 当成功处理：重复关注幂等，不重试、不报错。
			return nil
		}
		// 其他错误才真正返回，触发重试。
		return err
	// 取关。
	case "unfollow":
		// repo.Unfollow 删除关系并返回错误（没关注过时删除 0 行，也不报错）。
		return w.repo.Unfollow(ctx, &social.Social{
			// FollowerID 粉丝。
			FollowerID: evt.FollowerID,
			// VloggerID 博主。
			VloggerID: evt.VloggerID,
		})
	// 未知动作：忽略。
	default:
		// 返回 nil。
		return nil
	}
}
