// package rabbitmq：RabbitMQ 中间件包（本文件封装"举报事件"的发布）。
package rabbitmq

import (
	// context：控制发布超时。
	"context"
	// errors：返回未初始化/参数错误。
	"errors"
	// time：事件发生时刻。
	"time"

	// amqp：客户端 Channel 类型。
	amqp "github.com/rabbitmq/amqp091-go"
)

// ReportMQ 结构体是"举报事件发布器"，持有一个专用发布通道。
type ReportMQ struct {
	// ch 发布举报事件专用的 Channel。
	ch *amqp.Channel
}

const (
	// reportExchange 举报交换机名。
	reportExchange = "moderation.events"
	// reportQueue 队列名（ModerationWorker 消费它）。
	reportQueue = "moderation.events"
	// reportBindingKey 绑定键 "report.*"，举报相关事件都能收到。
	reportBindingKey = "report.*"

	// reportCreatedRK 举报创建动作的路由键。
	reportCreatedRK = "report.created"
)

// ReportEvent 是举报消息的事件体。
type ReportEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// ReportID 举报记录编号（reports 表主键）。
	ReportID uint `json:"report_id"`
	// TargetType 被举报对象类型 video/comment/user。
	TargetType string `json:"target_type"`
	// TargetID 被举报对象编号。
	TargetID uint `json:"target_id"`
	// ReporterID 举报人编号。
	ReporterID uint `json:"reporter_id"`
	// ReasonType 举报分类。
	ReasonType string `json:"reason_type"`
	// Detail 用户补充说明（不可信文本，喂模型时会隔离）。
	Detail string `json:"detail"`
	// OccurredAt 事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewReportMQ 函数：创建举报发布器——开通道、声明举报拓扑。
//
// 参数 base：总连接；
// 返回值：*ReportMQ 发布器、error 错误。
func NewReportMQ(base *RabbitMQ) (*ReportMQ, error) {
	// 连接为空。
	if base == nil {
		// 返回错误。
		return nil, errors.New("rabbitmq base is nil")
	}
	// ch 开辟新通道；err 接收错误。
	ch, err := base.NewChannel()
	if err != nil {
		// 失败返回。
		return nil, err
	}
	// 声明举报交换机+队列+绑定。
	if err := DeclareTopic(ch, reportExchange, reportQueue, reportBindingKey); err != nil {
		// 声明失败：关通道、返回错误。
		ch.Close()
		// 返回。
		return nil, err
	}
	// 构造发布器返回。
	return &ReportMQ{ch: ch}, nil
}

// Created 是 ReportMQ 的方法：发布一条"举报已创建"事件。
//
// 参数：m 发布器、ctx 上下文、evt 已组装好的举报事件；
// 返回值 error：发布错误。
func (m *ReportMQ) Created(ctx context.Context, evt ReportEvent) error {
	// 发布器或通道不存在。
	if m == nil || m.ch == nil {
		// 返回未初始化错误。
		return errors.New("report mq is not initialized")
	}
	// 举报记录编号缺失：无效事件。
	if evt.ReportID == 0 {
		// 返回参数错误。
		return errors.New("report_id is required")
	}
	// PublishJSON 编码并发布到举报交换机、路由键 report.created，返回其错误。
	return PublishJSON(ctx, m.ch, reportExchange, reportCreatedRK, evt)
}
