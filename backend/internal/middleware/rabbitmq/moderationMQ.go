// package rabbitmq：本文件封装"工单待人工"事件的发布（D7）。
//
// 背景：ModerationWorker 在独立 worker 进程里跑，审核台 SSEHub 在 Web 进程里，
// 两个进程内存不共享。worker 判定"需要人工"后，只能通过 MQ 通知 Web 进程：
// 发布 moderation.pending 事件 → Web 进程的 ModerationNotificationWorker 消费 →
// 写入 notifications 表并通过 SSEHub 实时推给在线审核员。
package rabbitmq

import (
	// context：控制发布超时。
	"context"
	// errors：未初始化/参数错误。
	"errors"
	// time：事件发生时刻。
	"time"

	// amqp：客户端 Channel 类型。
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// moderationPendingRK "工单待人工"事件的路由键。
	// 注意交换机仍是 moderation.events，但该键不匹配 report.* 绑定，
	// 所以不会被 ModerationWorker 自己再消费一次。
	moderationPendingRK = "moderation.pending"
)

// PendingCaseEvent 是"工单待人工"消息的事件体。
type PendingCaseEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// CaseID 待人工的工单编号（审核员据此打开工单）。
	CaseID uint `json:"case_id"`
	// ReportID 来源举报编号。
	ReportID uint `json:"report_id"`
	// TargetType 被举报对象类型。
	TargetType string `json:"target_type"`
	// TargetID 被举报对象编号。
	TargetID uint `json:"target_id"`
	// Reason 转人工原因（哪条闸门没过/Agent 报错）。
	Reason string `json:"reason"`
	// OccurredAt 事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// ModerationMQ 结构体是"审核通知发布器"，持有专用发布通道。
type ModerationMQ struct {
	// ch 发布用的 Channel。
	ch *amqp.Channel
}

// NewModerationMQ 函数：创建审核通知发布器。
//
// 与 NewReportMQ 不同，这里【不声明任何队列】：
// 交换机 moderation.events 已由 worker 拓扑声明；通知队列由 Web 进程声明。
// 本发布器只需要一个能发消息的通道。
//
// 参数 base：总连接；
// 返回值：*ModerationMQ、error。
func NewModerationMQ(base *RabbitMQ) (*ModerationMQ, error) {
	// 连接为空。
	if base == nil {
		// 返回错误。
		return nil, errors.New("rabbitmq base is nil")
	}
	// ch 开辟新通道；err 接收错误。
	ch, err := base.NewChannel()
	// 失败返回。
	if err != nil {
		// 返回。
		return nil, err
	}
	// 构造返回。
	return &ModerationMQ{ch: ch}, nil
}

// PendingCase 是 ModerationMQ 的方法：发布一条"工单待人工"事件。
//
// 参数：ctx 上下文、evt 事件体；
// 返回值 error：发布错误。
func (m *ModerationMQ) PendingCase(ctx context.Context, evt PendingCaseEvent) error {
	// 发布器或通道不存在。
	if m == nil || m.ch == nil {
		// 返回未初始化错误。
		return errors.New("moderation mq is not initialized")
	}
	// 工单编号缺失：无效事件。
	if evt.CaseID == 0 {
		// 返回参数错误。
		return errors.New("case_id is required")
	}
	// PublishJSON 发到 moderation.events 交换机、路由键 moderation.pending，返回其错误。
	return PublishJSON(ctx, m.ch, reportExchange, moderationPendingRK, evt)
}
