// package worker：本文件是 ModerationNotificationWorker（D7）——
// 在 Web 进程里消费 moderation.pending 事件：
// 给每个审核员生成一条 notifications 记录（历史可查），并通过 SSEHub
// 实时推送给在线审核员的审核台页面。
//
// 它和 NotificationWorker 是同一类角色（MQ → notifications 表 → SSE），
// 区别只在接收者不是"被互动的用户"，而是配置里的【审核员账号列表】。
package worker

import (
	// context：贯穿消费过程。
	"context"
	// encoding/json：反序列化事件。
	"encoding/json"
	// errors：初始化检查。
	"errors"
	// rabbitmq：PendingCaseEvent。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// log：重试/异常日志。
	"log"
	// strconv：工单编号转字符串拼文案。
	"strconv"
	// strings：拼通知文案、截断过长原因。
	"strings"
	// time：退避。
	"time"

	// amqp：客户端。
	amqp "github.com/rabbitmq/amqp091-go"
	// gorm：*gorm.DB 字段类型。
	"gorm.io/gorm"
)

// moderationNotifType 是写入 notifications.type 的事件类型标识，
// 前端审核台据此区分"这是一条待审工单通知"。
const moderationNotifType = "moderation.pending"

// moderationNotifContentMax 通知文案最大长度（notifications.content 是 varchar(255)）。
const moderationNotifContentMax = 255

// ModerationNotificationWorker 结构体：审核通知工作者。
type ModerationNotificationWorker struct {
	// ch RabbitMQ 频道。
	ch *amqp.Channel
	// queue 队列名 notification.moderation。
	queue string
	// db 写 notifications 表。
	db *gorm.DB
	// hub 实时推送中心（SSEHub）。
	hub NotificationHub
	// reviewerIDs 审核员账号列表：每人一条通知。
	reviewerIDs []uint
}

// NewModerationNotificationWorker 构造函数：创建审核通知工作者。
//
// 参数：ch、queue、db、hub、reviewerIDs；
// 返回值 *ModerationNotificationWorker。
func NewModerationNotificationWorker(ch *amqp.Channel, queue string, db *gorm.DB, hub NotificationHub, reviewerIDs []uint) *ModerationNotificationWorker {
	// 装配返回。
	return &ModerationNotificationWorker{
		ch: ch, queue: queue, db: db, hub: hub, reviewerIDs: reviewerIDs,
	}
}

// Run 方法：启动消费循环（结构与 NotificationWorker.Run 一致）。
//
// 参数 ctx：生命周期控制；
// 返回值 error：初始化失败/通道关闭。
func (w *ModerationNotificationWorker) Run(ctx context.Context) error {
	// 依赖完整性检查。
	if w == nil || w.ch == nil || w.db == nil {
		// 返回。
		return errors.New("moderation notification worker is not initialized")
	}
	// 队列名必填。
	if w.queue == "" {
		// 返回。
		return errors.New("queue is required")
	}
	// Consume 注册消费者：手动 Ack。
	deliveries, err := w.ch.Consume(w.queue, "", false, false, false, false, nil)
	// 失败返回。
	if err != nil {
		// 返回。
		return err
	}
	// 主循环。
	for {
		// select 等停止或消息。
		select {
		// 关停。
		case <-ctx.Done():
			// 返回。
			return ctx.Err()
		// 来消息。
		case d, ok := <-deliveries:
			// 通道关闭。
			if !ok {
				// 返回触发重连。
				return errors.New("deliveries channel closed")
			}
			// 带重试处理。
			w.handleDelivery(ctx, d)
		}
	}
}

// handleDelivery 方法：失败指数退避重试 3 次，耗尽则 Ack 丢弃。
//
// 参数：ctx、d。
func (w *ModerationNotificationWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
	// maxRetries。
	const maxRetries = 3
	// i 遍历。
	for i := 0; i <= maxRetries; i++ {
		// 非阻塞检查停止信号。
		select {
		// 已取消：Nack 放回。
		case <-ctx.Done():
			// Nack。
			_ = d.Nack(false, true)
			// 结束。
			return
		// 默认继续。
		default:
		}
		// process 处理。
		if err := w.process(ctx, d.Body); err != nil {
			// 重试耗尽：记日志、Ack 丢弃。
			if i >= maxRetries {
				// 日志。
				log.Printf("moderation notification worker: 重试 %d 次后失败, 丢弃: %v", maxRetries, err)
				// Ack。
				_ = d.Ack(false)
				// 结束。
				return
			}
			// wait 指数退避。
			wait := time.Duration(1<<uint(i)) * time.Second
			// 记录。
			log.Printf("moderation notification worker: 处理失败, %v 后重试 (%d/%d): %v", wait, i+1, maxRetries, err)
			// 休眠。
			time.Sleep(wait)
			// 重试。
			continue
		}
		// 成功 Ack。
		_ = d.Ack(false)
		// 结束。
		return
	}
}

// process 方法：给审核员落通知 + SSE 推送。
//
// 参数：ctx、body 事件体；
// 返回值 error：坏消息返回 nil 跳过；DB 故障返回错误触发重试。
func (w *ModerationNotificationWorker) process(ctx context.Context, body []byte) error {
	// evt 接收事件。
	var evt rabbitmq.PendingCaseEvent
	// 反序列化失败：坏消息，直接跳过。
	if err := json.Unmarshal(body, &evt); err != nil {
		// 返回 nil。
		return nil
	}
	// 工单编号缺失：无效事件，跳过。
	if evt.CaseID == 0 {
		// 返回 nil。
		return nil
	}
	// 没有配置审核员：通知无处可发，记日志后跳过（不重试——重试也没人可发）。
	if len(w.reviewerIDs) == 0 {
		// 日志。
		log.Printf("moderation notification worker: 未配置审核员, caseID=%d 无法推送", evt.CaseID)
		// 返回 nil。
		return nil
	}

	// content 通知文案：说明哪个工单、为什么转人工。
	content := "有新的待审工单 #" + strconv.FormatUint(uint64(evt.CaseID), 10)
	// 转人工原因非空：拼进文案。
	if strings.TrimSpace(evt.Reason) != "" {
		// 拼接原因。
		content += "：" + strings.TrimSpace(evt.Reason)
	}
	// 截断到列长度内，避免写入报错。
	if len(content) > moderationNotifContentMax {
		// 截断（按字节，文案是 ASCII/UTF-8 混合，可能截断多字节字符，
		// 通知场景可接受；最多末尾出现半个汉字）。
		content = content[:moderationNotifContentMax]
	}

	// notifs 给每个审核员构造一行通知。
	notifs := make([]*Notification, 0, len(w.reviewerIDs))
	// range 审核员；rid 当前审核员。
	for _, rid := range w.reviewerIDs {
		// 审核员编号为 0 的无效配置跳过。
		if rid == 0 {
			// 跳过。
			continue
		}
		// notif 组装。
		notifs = append(notifs, &Notification{
			// RecipientID 审核员。
			RecipientID: rid,
			// SenderID 0 平台官方。
			SenderID: 0,
			// Type moderation.pending。
			Type: moderationNotifType,
			// TargetID 存工单编号，前端点击可跳转。
			TargetID: evt.CaseID,
			// Content 文案。
			Content: content,
		})
	}
	// 全部跳过（没有有效审核员）：结束。
	if len(notifs) == 0 {
		// 返回 nil。
		return nil
	}

	// Create 批量插入通知；失败返回重试。
	if err := w.db.WithContext(ctx).Create(&notifs).Error; err != nil {
		// 返回。
		return err
	}
	// hub 配置了：逐条实时推送给在线审核员（不在线的已在库里，跳过即可）。
	if w.hub != nil {
		// range 刚插入的通知。
		for _, n := range notifs {
			// Push。
			w.hub.Push(n.RecipientID, n)
		}
	}
	// 成功。
	return nil
}
