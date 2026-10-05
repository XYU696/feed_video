// package rabbitmq：RabbitMQ 中间件包（本文件封装"关注事件"的发布）。
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

// SocialMQ 结构体是"关注事件发布器"，持有一个专用发布通道。
type SocialMQ struct {
	// ch 是发布关注事件专用的 Channel。
	ch *amqp.Channel
}

const (
	// socialExchange：关注交换机名。
	socialExchange = "social.events"
	// socialQueue：落库队列名（SocialWorker 消费它）。
	socialQueue = "social.events"
	// socialBindingKey：绑定键 "social.*"，follow/unfollow 都能收到。
	socialBindingKey = "social.*"

	// socialFollowRK：关注动作的路由键。
	socialFollowRK = "social.follow"
	// socialUnfollowRK：取关动作的路由键。
	socialUnfollowRK = "social.unfollow"
)

// SocialEvent 是关注消息的事件体。
type SocialEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// Action 动作名（"follow" / "unfollow"），消费者靠它分流。
	Action string `json:"action"`
	// FollowerID 是粉丝（主动关注的人）的用户编号。
	FollowerID uint `json:"follower_id"`
	// VloggerID 是被关注者的用户编号。
	VloggerID uint `json:"vlogger_id"`
	// OccurredAt 事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewSocialMQ 函数的作用：创建关注发布器——开通道、声明关注拓扑。
//
// 参数 base：总连接；
// 返回值：*SocialMQ 发布器、error 错误。
func NewSocialMQ(base *RabbitMQ) (*SocialMQ, error) {
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
	// 声明关注交换机+队列+绑定。
	if err := DeclareTopic(ch, socialExchange, socialQueue, socialBindingKey); err != nil {
		// 声明失败：关闭通道并返回错误。
		ch.Close()
		// 返回错误。
		return nil, err
	}
	// 成功：构造发布器返回。
	return &SocialMQ{ch: ch}, nil
}

// Follow 是 SocialMQ 的方法：发布一条"关注"事件。
//
// 参数：s 发布器、ctx 上下文、followerID 粉丝编号、vloggerID 被关注者编号；
// 返回值 error：发布错误。
func (s *SocialMQ) Follow(ctx context.Context, followerID, vloggerID uint) error {
	// 转调内部 publish：动作 "follow"、路由键 socialFollowRK，带上粉丝和被关注者。
	return s.publish(ctx, "follow", socialFollowRK, followerID, vloggerID)
}

// UnFollow 是 SocialMQ 的方法：发布一条"取关"事件。
//
// 参数：s 发布器、ctx 上下文、followerID 粉丝编号、vloggerID 被关注者编号；
// 返回值 error：发布错误。
func (s *SocialMQ) UnFollow(ctx context.Context, followerID, vloggerID uint) error {
	// 转调内部 publish：动作 "unfollow"、路由键 socialUnfollowRK。
	return s.publish(ctx, "unfollow", socialUnfollowRK, followerID, vloggerID)
}

// publish 是 SocialMQ 的内部方法：组装关注事件并发布（Follow 和 UnFollow 共用）。
//
// 参数：s 发布器、ctx 上下文、action 动作名、routingKey 路由键、followerID 粉丝编号、vloggerID 被关注者编号；
// 返回值 error：错误。
func (s *SocialMQ) publish(ctx context.Context, action, routingKey string, followerID, vloggerID uint) error {
	// 发布器或通道不存在。
	if s == nil || s.ch == nil {
		// 返回未初始化错误。
		return errors.New("social mq is not initialized")
	}
	// 粉丝编号或被关注者编号为 0（无效数据）。
	if followerID == 0 || vloggerID == 0 {
		// 返回参数错误。
		return errors.New("followerID and vloggerID are required")
	}
	// id 生成 16 字节随机事件 ID；err 接收错误。
	id, err := newEventID(16)
	if err != nil {
		// 失败：返回错误。
		return err
	}
	// event 组装完整事件体。
	evt := SocialEvent{
		// EventID 填入唯一 ID。
		EventID: id,
		// Action 填入动作名。
		Action: action,
		// FollowerID 填入粉丝。
		FollowerID: followerID,
		// VloggerID 填入被关注者。
		VloggerID: vloggerID,
		// OccurredAt 填入当前 UTC 时刻。
		OccurredAt: time.Now().UTC(),
	}
	// PublishJSON 编码事件并发布到关注交换机，返回其错误。
	return PublishJSON(ctx, s.ch, socialExchange, routingKey, evt)
}
