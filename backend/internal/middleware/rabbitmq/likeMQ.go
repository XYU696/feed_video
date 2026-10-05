// package rabbitmq：RabbitMQ 中间件包（本文件封装"点赞事件"的发布）。
package rabbitmq

import (
	// context：控制发布超时。
	"context"
	// errors：返回未初始化错误。
	"errors"
	// time：事件发生时刻。
	"time"

	// amqp：客户端，这里只用它的 Channel 类型。
	amqp "github.com/rabbitmq/amqp091-go"
)

// LikeMQ 结构体是"点赞事件发布器"，内部只持有一个专用发布通道。
type LikeMQ struct {
	// ch 是这个发布器专用的 Channel（在 NewLikeMQ 时创建并声明好拓扑）。
	ch *amqp.Channel
}

const (
	// likeExchange：点赞交换机名。
	likeExchange = "like.events"
	// likeQueue：落库队列名（LikeWorker 消费它）。
	likeQueue = "like.events"
	// likeBindingKey：队列绑定键 "like.*" —— like.like 和 like.unlike 两种事件都能收到。
	likeBindingKey = "like.*"

	// likeLikeRK：点赞动作的路由键。
	likeLikeRK = "like.like"
	// likeUnlikeRK：取消点赞动作的路由键。
	likeUnlikeRK = "like.unlike"
)

// LikeEvent 是点赞消息的事件体（被编码成 JSON 传输）。
//
// 设计理解：消息里不直接放"对象"，而是用统一的事件结构：
// 带事件 ID、动作类型、发生时间，消费者拿到后知道"谁、对哪个视频、做了什么"。
type LikeEvent struct {
	// EventID 是事件唯一编号（用于追踪/去重）。
	EventID string `json:"event_id"`
	// Action 是动作类型（"like" 或 "unlike"），消费者靠它分流。
	Action string `json:"action"`
	// UserID 是操作的用户编号（点赞的人）。
	UserID uint `json:"user_id"`
	// VideoID 是被操作的视频编号。
	VideoID uint `json:"video_id"`
	// OccurredAt 是事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewLikeMQ 函数的作用：创建点赞发布器——在连接上开通道、声明点赞拓扑。
//
// 参数 base：总连接对象；
// 返回值：*LikeMQ 发布器、error 开通道/声明失败错误。
func NewLikeMQ(base *RabbitMQ) (*LikeMQ, error) {
	// 连接为空。
	if base == nil {
		// 返回错误。
		return nil, errors.New("rabbitmq base is nil")
	}
	// ch 在连接上开辟一个新通道；err 接收错误。
	ch, err := base.NewChannel()
	if err != nil {
		// 开通道失败：返回错误。
		return nil, err
	}
	// DeclareTopic(ch, 交换机, 队列, 绑定键) 声明点赞的交换机+队列+绑定关系。
	if err := DeclareTopic(ch, likeExchange, likeQueue, likeBindingKey); err != nil {
		// 声明失败：先关掉这个通道避免泄漏，再返回错误。
		ch.Close()
		// 返回声明错误。
		return nil, err
	}
	// 成功：用通道构造发布器返回。
	return &LikeMQ{ch: ch}, nil
}

// Like 是 LikeMQ 的方法：发布一条"点赞"事件。
//
// 参数：l 发布器、ctx 上下文、userID 点赞用户编号、videoID 视频编号；
// 返回值 error：发布错误。
func (l *LikeMQ) Like(ctx context.Context, userID, videoID uint) error {
	// 转调内部 publish：动作 "like"、路由键 likeLikeRK，带上用户和视频。
	return l.publish(ctx, "like", likeLikeRK, userID, videoID)
}

// Unlike 是 LikeMQ 的方法：发布一条"取消点赞"事件。
//
// 参数：l 发布器、ctx 上下文、userID 用户编号、videoID 视频编号；
// 返回值 error：发布错误。
func (l *LikeMQ) Unlike(ctx context.Context, userID, videoID uint) error {
	// 转调内部 publish：动作 "unlike"、路由键 likeUnlikeRK。
	return l.publish(ctx, "unlike", likeUnlikeRK, userID, videoID)
}

// publish 是 LikeMQ 的内部方法：组装点赞事件并发布（Like 和 Unlike 共用它，避免重复代码）。
//
// 参数：l 发布器、ctx 上下文、action 动作名、routingKey 路由键、userID 用户编号、videoID 视频编号；
// 返回值 error：生成 ID/发布错误。
func (l *LikeMQ) publish(ctx context.Context, action, routingKey string, userID, videoID uint) error {
	// 发布器或通道不存在。
	if l == nil || l.ch == nil {
		// 返回未初始化错误。
		return errors.New("like mq is not initialized")
	}
	// 用户编号或视频编号为 0（无效数据）。
	if userID == 0 || videoID == 0 {
		// 返回参数错误。
		return errors.New("userID and videoID are required")
	}
	// id 变量生成 16 字节随机事件 ID（32 个十六进制字符）；err 接收错误。
	id, err := newEventID(16)
	if err != nil {
		// 失败：返回错误。
		return err
	}
	// event 变量组装完整事件体。
	event := LikeEvent{
		// EventID 填入唯一事件 ID。
		EventID: id,
		// Action 填入动作名（like/unlike）。
		Action: action,
		// UserID 填入操作用户。
		UserID: userID,
		// VideoID 填入视频。
		VideoID: videoID,
		// OccurredAt 填入当前时刻。
		OccurredAt: time.Now(),
	}
	// PublishJSON 把事件编码成 JSON，发到点赞交换机、带上本次路由键，返回其错误。
	return PublishJSON(ctx, l.ch, likeExchange, routingKey, event)
}
