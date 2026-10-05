// package rabbitmq：RabbitMQ 中间件包（本文件封装"热度增量事件"的发布）。
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

// PopularityMQ 结构体是"热度事件发布器"，持有一个专用发布通道。
type PopularityMQ struct {
	// ch 是发布热度事件专用的 Channel。
	ch *amqp.Channel
}

const (
	// popularityExchange：热度交换机名。
	popularityExchange = "video.popularity.events"
	// popularityQueue：队列名（PopularityWorker 消费它，把热度写进 Redis）。
	popularityQueue = "video.popularity.events"
	// popularityBindingKey：绑定键 "video.popularity.*"。
	popularityBindingKey = "video.popularity.*"

	// popularityUpdateRK：热度更新动作的路由键。
	popularityUpdateRK = "video.popularity.update"
)

// PopularityEvent 是热度消息的事件体。
type PopularityEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// VideoID 是热度发生变化的视频编号。
	VideoID uint `json:"video_id"`
	// Change 是热度增量（点赞 +1、取消 -1、评论也会 +1），有符号所以可以是负数。
	Change int64 `json:"change"`
	// OccurredAt 事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewPopularityMQ 函数的作用：创建热度发布器——开通道、声明热度拓扑。
//
// 参数 base：总连接；
// 返回值：*PopularityMQ 发布器、error 错误。
func NewPopularityMQ(base *RabbitMQ) (*PopularityMQ, error) {
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
	// 声明热度交换机+队列+绑定。
	if err := DeclareTopic(ch, popularityExchange, popularityQueue, popularityBindingKey); err != nil {
		// 声明失败：关闭通道并返回错误。
		ch.Close()
		// 返回错误。
		return nil, err
	}
	// 成功：构造发布器返回。
	return &PopularityMQ{ch: ch}, nil
}

// Update 是 PopularityMQ 的方法：发布一条"热度增量"事件。
//
// 参数：p 发布器、ctx 上下文、videoID 视频编号、change 热度变化量（正/负）；
// 返回值 error：发布错误。
func (p *PopularityMQ) Update(ctx context.Context, videoID uint, change int64) error {
	// 发布器或通道不存在。
	if p == nil || p.ch == nil {
		// 返回未初始化错误。
		return errors.New("popularity mq is not initialized")
	}
	// 视频编号为 0，或变化量为 0（没有变化的事件没必要发）。
	if videoID == 0 || change == 0 {
		// 返回参数错误。
		return errors.New("videoID and change are required")
	}
	// id 生成 16 字节随机事件 ID；err 接收错误。
	id, err := newEventID(16)
	if err != nil {
		// 失败：返回错误。
		return err
	}
	// event 组装完整事件体。
	event := PopularityEvent{
		// EventID 填入唯一 ID。
		EventID: id,
		// VideoID 填入视频编号。
		VideoID: videoID,
		// Change 填入增量。
		Change: change,
		// OccurredAt 填入当前 UTC 时刻。
		OccurredAt: time.Now().UTC(),
	}
	// PublishJSON 编码并发布到热度交换机，返回其错误。
	return PublishJSON(ctx, p.ch, popularityExchange, popularityUpdateRK, event)
}
