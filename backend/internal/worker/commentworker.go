// package worker：后台工作者包。本文件是 CommentWorker——
// 消费"发表评论 / 删除评论"事件，把评论真正写进 MySQL 或从 MySQL 删除。
//
// 结构和 LikeWorker 几乎一致：Run 收消息 → handleDelivery 带重试 → process 分发。
// 区别：评论没有维护 comments_count 之类的计数，发表评论只让视频热度 +1；
// 删除评论时要先把评论查出来（事件里只带了评论 ID），再按整条记录删除。
package worker

import (
	// context：上下文。
	"context"
	// encoding/json：反序列化评论事件。
	"encoding/json"
	// errors：初始化报错。
	"errors"
	// rabbitmq：CommentEvent 事件结构。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// video：CommentRepository、VideoRepository、Comment 模型。
	"feedsystem_video_go/internal/video"
	// log：重试日志。
	"log"
	// strings：TrimSpace 清洗用户名和正文。
	"strings"
	// time：退避等待。
	"time"

	// amqp：RabbitMQ 客户端。
	amqp "github.com/rabbitmq/amqp091-go"
)

// CommentWorker 结构体：评论工作者。
type CommentWorker struct {
	// ch RabbitMQ 频道。
	ch *amqp.Channel
	// comments 评论仓储：插评论、查评论、删评论。
	comments *video.CommentRepository
	// videos 视频仓储：判断视频存在、改热度。
	videos *video.VideoRepository
	// queue 队列名。
	queue string
}

// NewCommentWorker 是构造函数：创建评论工作者。
//
// 参数：ch 频道、comments 评论仓储、videos 视频仓储、queue 队列名；
// 返回值 *CommentWorker。
func NewCommentWorker(ch *amqp.Channel, comments *video.CommentRepository, videos *video.VideoRepository, queue string) *CommentWorker {
	// 装配返回。
	return &CommentWorker{ch: ch, comments: comments, videos: videos, queue: queue}
}

// Run 方法：启动评论消费循环。
//
// 参数 ctx：生命周期控制；
// 返回值 error：初始化失败/通道关闭的错误。
func (w *CommentWorker) Run(ctx context.Context) error {
	// 依赖完整性检查。
	if w == nil || w.ch == nil || w.comments == nil || w.videos == nil {
		// 返回未初始化错误。
		return errors.New("comment worker is not initialized")
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
		// 消费者标签自动。
		"",
		// 手动确认。
		false,
		// 非独占。
		false,
		// noLocal。
		false,
		// noWait。
		false,
		// 无额外参数。
		nil,
	)
	if err != nil {
		// 注册失败返回。
		return err
	}

	// 消费主循环。
	for {
		// select 等停止或消息。
		select {
		// 关停信号。
		case <-ctx.Done():
			// 返回 ctx 错误。
			return ctx.Err()
		// 来消息；d 消息、ok 通道状态。
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

// handleDelivery 方法：处理单条评论消息，失败指数退避重试 3 次。
//
// 参数：ctx 上下文；d 消息；
// 无返回值。
func (w *CommentWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
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
		// 没取消。
		default:
		}
		// process 处理；err 错误。
		if err := w.process(ctx, d.Body); err != nil {
			// 重试耗尽。
			if i >= maxRetries {
				// 记日志并 Ack 丢弃毒消息。
				log.Printf("comment worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
				// Ack。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录重试信息。
			log.Printf("comment worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
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

// process 方法：解析评论事件并按动作分发。
//
// 参数：ctx 上下文；body 消息体；
// 返回值 error：处理错误（坏消息返回 nil 直接跳过）。
func (w *CommentWorker) process(ctx context.Context, body []byte) error {
	// evt 准备接收评论事件。
	var evt rabbitmq.CommentEvent
	// 反序列化；err 错误。
	if err := json.Unmarshal(body, &evt); err != nil {
		// 坏消息：返回 nil 直接 Ack 丢弃。
		return nil
	}
	// switch 按动作分发。
	switch evt.Action {
	// 发表评论。
	case "publish":
		// 传 &evt 指针执行落库。
		return w.applyPublish(ctx, &evt)
	// 删除评论。
	case "delete":
		// 传 &evt 执行删除。
		return w.applyDelete(ctx, &evt)
	// 未知动作：忽略。
	default:
		// 返回 nil。
		return nil
	}
}

// applyPublish 方法：把一条评论真正写进数据库，并让视频热度 +1。
//
// 参数：ctx 上下文；evt 评论事件指针；
// 返回值 error：写库错误。
func (w *CommentWorker) applyPublish(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	// 事件为空、视频/作者 ID 缺失、正文去空格后为空：无效事件，直接忽略。
	if evt == nil || evt.VideoID == 0 || evt.AuthorID == 0 || strings.TrimSpace(evt.Content) == "" {
		// 返回 nil。
		return nil
	}

	// ok 视频是否存在；err 错误。
	ok, err := w.videos.IsExist(ctx, evt.VideoID)
	if err != nil {
		// 查询出错：返回重试。
		return err
	}
	// 视频已删除：评论无意义，直接结束。
	if !ok {
		// 返回 nil。
		return nil
	}

	// c 组装要入库的评论对象：
	c := &video.Comment{
		// Username 评论者用户名（事件里带来，去空格）。
		Username: strings.TrimSpace(evt.Username),
		// VideoID 所属视频。
		VideoID: evt.VideoID,
		// AuthorID 评论作者编号。
		AuthorID: evt.AuthorID,
		// Content 正文（去空格）。
		Content: strings.TrimSpace(evt.Content),
	}
	// CreateComment 插入评论；err 错误。
	if err := w.comments.CreateComment(ctx, c); err != nil {
		// 插入失败：返回重试。
		return err
	}
	// ChangePopularity 让该视频热度 +1，返回其错误。
	return w.videos.ChangePopularity(ctx, evt.VideoID, 1)
}

// applyDelete 方法：删除一条评论。先按 ID 把评论查出来，再整条删除。
//
// 参数：ctx 上下文；evt 评论事件指针（只用到 CommentID）；
// 返回值 error：查询/删除错误。
func (w *CommentWorker) applyDelete(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	// 事件为空或没带评论 ID：无效，忽略。
	if evt == nil || evt.CommentID == 0 {
		// 返回 nil。
		return nil
	}
	// GetByID 按 ID 查评论；c 评论指针、err 错误。
	c, err := w.comments.GetByID(ctx, evt.CommentID)
	if err != nil {
		// 查询出错：返回重试。
		return err
	}
	// c == nil：评论已不存在（仓储约定查不到返回 nil,nil），无需再删。
	if c == nil {
		// 返回 nil——重复删除幂等。
		return nil
	}
	// DeleteComment 删除这条评论，返回错误。
	return w.comments.DeleteComment(ctx, c)
}
