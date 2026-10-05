// package rabbitmq：RabbitMQ 中间件包（本文件封装"视频时间线事件"的发布）。
package rabbitmq

import (
	// context：控制发布超时。
	"context"
	// errors：返回未初始化/参数错误。
	"errors"
	// time：事件发生时刻、参数里的视频发布时间。
	"time"

	// amqp：客户端 Channel 类型。
	amqp "github.com/rabbitmq/amqp091-go"
)

// TimelineMQ 结构体是"时间线事件发布器"，持有一个专用发布通道。
//
// 这个发布器和其他 MQ 有个不同点：它发布的消息不是在接口请求里直接产生的，
// 而是由"发件箱轮询器(OutboxPoller)"从数据库的 outbox_msgs 表里捞出来后，
// 调用 PublishVideo 投递——这是 Outbox 模式链路里的最后一环。
type TimelineMQ struct {
	// ch 是发布时间线事件专用的 Channel。
	ch *amqp.Channel
}

const (
	// timelineExchange：时间线交换机名。
	timelineExchange = "video.timeline.events"
	// timelineQueue：队列名（TimelineConsumer 消费它，把新视频写进 Redis 全局时间线 ZSET）。
	timelineQueue = "video.timeline.update.queue"
	// timelineBindingKey：绑定键 "video.timeline.*"。
	timelineBindingKey = "video.timeline.*"
	// timelinePublishRK：发布动作的路由键。
	timelinePublishRK = "video.timeline.publish"
)

// TimelineEvent 是时间线消息的事件体。
type TimelineEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// VideoID 是新发布的视频编号（消费者要把它写进时间线）。
	VideoID uint `json:"video_id"`
	// CreateTime 是视频的发布时间（毫秒时间戳），会被当作 ZSET 里的分数，
	// 所以视频在时间线上的排序位置就是由它决定的。
	CreateTime int64 `json:"create_time"`
	// OccurredAt 事件被发送的时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewTimelineMQ 函数的作用：创建时间线发布器——开通道、声明拓扑。
//
// 参数 base：总连接；
// 返回值：*TimelineMQ 发布器、error 错误。
func NewTimelineMQ(base *RabbitMQ) (*TimelineMQ, error) {
	// 连接为空。
	if base == nil {
		// 返回错误。
		return nil, errors.New("rabbitmq base is nil")
	}
	// ch 开辟新通道；err 接收错误。
	ch, err := base.NewChannel()
	if err != nil {
		// 失败：返回错误。
		return nil, err
	}
	// 声明时间线交换机+队列+绑定。
	if err := DeclareTopic(ch, timelineExchange, timelineQueue, timelineBindingKey); err != nil {
		// 声明失败：关闭通道并返回错误。
		ch.Close()
		// 返回错误。
		return nil, err
	}
	// 成功：构造发布器返回。
	return &TimelineMQ{ch: ch}, nil
}

// PublishVideo 是 TimelineMQ 的方法：发布一条"新视频时间线"事件。
//
// 参数：t 发布器、ctx 上下文、videoID 视频编号、createTime 视频发布时间（来自 outbox 记录）；
// 返回值 error：生成 ID/发布错误。
func (t *TimelineMQ) PublishVideo(ctx context.Context, videoID uint, createTime time.Time) error {
	// 发布器或通道不存在。
	if t == nil || t.ch == nil {
		// 返回未初始化错误。
		return errors.New("timeline mq is not initialized")
	}
	// 视频编号为 0（无效数据）。
	if videoID == 0 {
		// 返回参数错误。
		return errors.New("videoID are required")
	}
	// id 生成 16 字节随机事件 ID；err 接收错误。
	id, err := newEventID(16)
	if err != nil {
		// 失败：返回错误。
		return err
	}
	// timeline 组装完整事件体。
	timeline := TimelineEvent{
		// EventID 填入唯一 ID。
		EventID: id,
		// VideoID 填入视频编号。
		VideoID: videoID,
		// CreateTime 填入视频发布时间的毫秒时间戳（UnixMilli() 把 time.Time 转毫秒整数）。
		CreateTime: createTime.UnixMilli(),
		// OccurredAt 填入当前时刻。
		OccurredAt: time.Now(),
	}
	// PublishJSON 编码并发布到时间线交换机，返回其错误。
	return PublishJSON(ctx, t.ch, timelineExchange, timelinePublishRK, timeline)
}
