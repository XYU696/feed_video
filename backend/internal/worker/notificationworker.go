// package worker：后台工作者包。本文件是 NotificationWorker——
// 专门消费"点赞 / 评论 / 关注"三类事件，把它们转成通知记录存进 notifications 表，
// 再通过 NotificationHub（实际是 SSEHub）实时推送给接收者。
//
// 设计要点：按 RabbitMQ 的 routing key 区分事件类型；自己给自己点赞/评论不发通知；
// 处理失败指数退避重试 3 次，仍失败就 ACK 丢弃（避免毒消息无限占队）。
package worker

import (
	// context：上下文。
	"context"
	// encoding/json：反序列化 MQ 消息体。
	"encoding/json"
	// errors：初始化检查报错。
	"errors"
	// rabbitmq：用到各事件结构体 LikeEvent/CommentEvent/SocialEvent。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// log：失败重试日志。
	"log"
	// time：退避等待、通知时间。
	"time"

	// amqp：RabbitMQ 官方客户端，用到 Channel、Delivery。
	amqp "github.com/rabbitmq/amqp091-go"
	// gorm：操作数据库。
	"gorm.io/gorm"
)

// Notification 结构体：通知表模型（一张表存所有类型的通知）。
type Notification struct {
	// ID 主键。
	ID uint `gorm:"primaryKey" json:"id"`
	// RecipientID 接收者编号：建索引（按收件人查列表/计数很频繁）、非空。
	RecipientID uint `gorm:"index;not null" json:"recipient_id"`
	// SenderID 发起者编号、非空。
	SenderID uint `gorm:"not null" json:"sender_id"`
	// Type 通知类型（like/comment/follow），定长 50 字符串、非空。
	Type string `gorm:"type:varchar(50);not null" json:"type"`
	// TargetID 关联目标编号（如被点赞的视频 ID）。
	TargetID uint `json:"target_id"`
	// Content 通知文案，最长 255。
	Content string `gorm:"type:varchar(255)" json:"content"`
	// IsRead 是否已读，默认 false。
	IsRead bool `gorm:"default:false" json:"is_read"`
	// CreatedAt 创建时间，GORM 自动写入。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// NotificationWorker 结构体：通知工作者。
type NotificationWorker struct {
	// ch RabbitMQ 频道：从队列收消息。
	ch *amqp.Channel
	// db 数据库：反查视频作者、写通知记录。
	db *gorm.DB
	// queue 要消费的队列名。
	queue string
	// hub 推送中心：通知入库后实时推给在线用户；面向接口编程（便于替换/测试）。
	hub NotificationHub
}

// NotificationHub 是接口：只要求一个 Push 方法。
// Go 的接口是【隐式实现】——SSEHub 只要有 Push 方法，就自动算实现了本接口，
// 不需要写 "implements"（和 Python 的鸭子类型一个味道：能 Push 就能当 Hub）。
type NotificationHub interface {
	// Push 给指定用户推一条通知。
	Push(userID uint, n *Notification)
}

// NewNotificationWorker 是构造函数：创建通知工作者。
//
// 参数：ch 频道、db 数据库、queue 队列名、hub 推送中心；
// 返回值 *NotificationWorker。
func NewNotificationWorker(ch *amqp.Channel, db *gorm.DB, queue string, hub NotificationHub) *NotificationWorker {
	// 装配返回。
	return &NotificationWorker{ch: ch, db: db, queue: queue, hub: hub}
}

// Run 方法：启动消费循环，直到 ctx 被取消或通道断开。
//
// 参数 ctx：控制 worker 生命周期；
// 返回值 error：初始化/通道关闭时的错误。
func (w *NotificationWorker) Run(ctx context.Context) error {
	// 防御性检查：worker 本身、频道、数据库缺一不可。
	if w == nil || w.ch == nil || w.db == nil {
		// 返回未初始化错误。
		return errors.New("notification worker is not initialized")
	}
	// 队列名不能为空。
	if w.queue == "" {
		// 返回。
		return errors.New("queue is required")
	}
	// AutoMigrate 确保 notifications 表存在；err 错误。
	if err := w.db.WithContext(ctx).AutoMigrate(&Notification{}); err != nil {
		// 建表失败直接返回（没法消费）。
		return err
	}
	// Consume 注册消费者，返回一个 Go channel（deliveries），消息会源源不断送进来；err 错误。
	// 参数依次：队列名、消费者标签(""自动生成)、autoAck=false、exclusive=false、noLocal=false、noWait=false、args=nil。
	deliveries, err := w.ch.Consume(w.queue, "", false, false, false, false, nil)
	if err != nil {
		// 注册失败返回。
		return err
	}
	// 无限循环等消息。
	for {
		// select 等两件事：
		select {
		// ① 外部要求停止（关停服务）。
		case <-ctx.Done():
			// 返回 ctx 的错误（Canceled/DeadlineExceeded）。
			return ctx.Err()
		// ② 来了一条消息；d 是消息、ok 表示通道是否还开着。
		case d, ok := <-deliveries:
			// 通道被关闭（如 RabbitMQ 断线）。
			if !ok {
				// 返回错误，交由外层重连逻辑处理。
				return errors.New("deliveries channel closed")
			}
			// 交给带重试的处理函数。
			w.handleDelivery(ctx, d)
		}
	}
}

// handleDelivery 方法：处理单条消息，失败按指数退避重试，最多 3 次。
//
// 参数：ctx 上下文；d 一条 RabbitMQ 消息；
// 无返回值（最终要么 Ack 成功、要么 Ack 丢弃）。
func (w *NotificationWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	// maxRetries 最大重试次数，常量。
	const maxRetries = 3
	// i 从 0 数到 3（含首次共处理 4 次）。
	for i := 0; i <= maxRetries; i++ {
		// 每次尝试前先看是否该停止。
		select {
		// ctx 已取消。
		case <-ctx.Done():
			// Nack(requeue=true)：不确认、把消息放回队列，等下次启动再处理（不丢消息）。
			_ = d.Nack(false, true)
			// 结束。
			return
		// default：没取消，立即继续（这是"非阻塞检查 ctx"的惯用法）。
		default:
		}
		// process 真正处理消息；err 错误。
		if err := w.process(ctx, d); err != nil {
			// 已达最后一次。
			if i >= maxRetries {
				// 记录：重试 3 次仍失败，丢弃。
				log.Printf("notification worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
				// Ack 确认并丢弃——避免这条"毒消息"被无限重试堵死整个队列。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避：1<<i 即 1、2、4、8... 秒（位运算左移）。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录第几次失败、多久后重试。
			log.Printf("notification worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
			// 睡这么久再试，给故障恢复的时间。
			time.Sleep(wait)
			// 进入下一次循环。
			continue
		}
		// 处理成功：Ack 确认，消息从队列移除。
		_ = d.Ack(false)
		// 结束。
		return
	}
}

// process 方法：解析一条消息并生成对应通知（核心业务在这）。
//
// 参数：ctx 上下文；d 消息；
// 返回值 error：处理错误（消息格式问题返回 nil 表示直接跳过）。
func (w *NotificationWorker) process(ctx context.Context, d amqp.Delivery) error {
	// body 消息体字节。
	body := d.Body
	// 空消息：无事可做，算成功。
	if len(body) == 0 {
		// 返回 nil。
		return nil
	}
	// routingKey 取消息的路由键，用来判断是哪类事件。
	routingKey := d.RoutingKey

	// notif 待生成的通知，默认 nil。
	var notif *Notification

	// switch 不带表达式：相当于 if-else if 链，按条件分支。
	switch {
	// 点赞事件。
	case routingKey == "like.like":
		// evt 准备接收点赞事件。
		var evt rabbitmq.LikeEvent
		// 反序列化；失败返回 nil（当作跳过，不重试）。
		if err := json.Unmarshal(body, &evt); err != nil {
			// 返回 nil 直接丢弃坏消息。
			return nil
		}
		// 关键字段缺失：忽略。
		if evt.UserID == 0 || evt.VideoID == 0 {
			// 返回 nil。
			return nil
		}
		// authorID 接收视频作者编号。
		var authorID uint
		// 反查 videos 表拿作者；用匿名结构体只声明要用的两列，
		// Table 指定 videos 表，Where 按视频 id，Select 只取 author_id，Scan 写进 authorID；err 错误。
		if err := w.db.WithContext(ctx).Model(&struct {
			// ID 视频编号。
			ID uint
			// AuthorID 作者编号。
			AuthorID uint
		}{}).Table("videos").Where("id = ?", evt.VideoID).Select("author_id").Scan(&authorID).Error; err != nil {
			// 查询失败：返回错误触发重试。
			return err
		}
		// 视频不存在（作者0），或点赞的人就是作者本人——不发通知。
		if authorID == 0 || authorID == evt.UserID {
			// 返回 nil 跳过。
			return nil
		}
		// 组装点赞通知。
		notif = &Notification{RecipientID: authorID, SenderID: evt.UserID, Type: "like", TargetID: evt.VideoID, Content: "点赞了你的视频"}

	// 评论事件（结构与点赞分支几乎一样）。
	case routingKey == "comment.publish":
		// evt 评论事件。
		var evt rabbitmq.CommentEvent
		// 反序列化；坏消息跳过。
		if err := json.Unmarshal(body, &evt); err != nil {
			// 返回 nil。
			return nil
		}
		// 关键字段缺失。
		if evt.AuthorID == 0 || evt.VideoID == 0 {
			// 跳过。
			return nil
		}
		// authorID 视频作者。
		var authorID uint
		// 同样反查视频作者；err 错误。
		if err := w.db.WithContext(ctx).Model(&struct {
			// ID 视频编号。
			ID uint
			// AuthorID 作者编号。
			AuthorID uint
		}{}).Table("videos").Where("id = ?", evt.VideoID).Select("author_id").Scan(&authorID).Error; err != nil {
			// 失败返回以重试。
			return err
		}
		// 视频不存在，或评论者就是作者本人——不打扰。
		if authorID == 0 || authorID == evt.AuthorID {
			// 跳过。
			return nil
		}
		// 组装评论通知。
		notif = &Notification{RecipientID: authorID, SenderID: evt.AuthorID, Type: "comment", TargetID: evt.VideoID, Content: "评论了你的视频"}

	// 关注事件。
	case routingKey == "social.follow":
		// evt 关注事件。
		var evt rabbitmq.SocialEvent
		// 反序列化；坏消息跳过。
		if err := json.Unmarshal(body, &evt); err != nil {
			// 返回 nil。
			return nil
		}
		// 关键字段缺失。
		if evt.FollowerID == 0 || evt.VloggerID == 0 {
			// 跳过。
			return nil
		}
		// 组装关注通知：接收者=被关注博主，发起者=粉丝，TargetID 存粉丝编号。
		notif = &Notification{RecipientID: evt.VloggerID, SenderID: evt.FollowerID, Type: "follow", TargetID: evt.FollowerID, Content: "关注了你"}
	}

	// 三个分支都没匹配上（notif 仍为 nil）。
	if notif == nil {
		// 无事发生，算成功 ACK。
		return nil
	}
	// 通知写库；err 错误。
	if err := w.db.WithContext(ctx).Create(notif).Error; err != nil {
		// 写库失败：返回以重试。
		return err
	}
	// hub 已配置。
	if w.hub != nil {
		// 实时推送给接收者（若 TA 不在线，Push 内部会静默跳过，通知已在库里）。
		w.hub.Push(notif.RecipientID, notif)
	}
	// 完成：返回 nil。
	return nil
}
