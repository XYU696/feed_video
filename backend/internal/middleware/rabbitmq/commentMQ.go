// package rabbitmq：RabbitMQ 中间件包（本文件封装"评论事件"的发布）。
package rabbitmq

import (
	// context：控制发布超时。
	"context"
	// errors：返回未初始化错误。
	"errors"
	// time：事件发生时刻。
	"time"

	// amqp：客户端 Channel 类型。
	amqp "github.com/rabbitmq/amqp091-go"
)

// CommentMQ 结构体是"评论事件发布器"，持有一个专用发布通道。
type CommentMQ struct {
	// ch 是发布评论事件专用的 Channel。
	ch *amqp.Channel
}

const (
	// commentExchange：评论交换机名。
	commentExchange = "comment.events"
	// commentQueue：落库队列名（CommentWorker 消费）。
	commentQueue = "comment.events"
	// commentBindingKey：绑定键 "comment.*"，publish/delete 都能收到。
	commentBindingKey = "comment.*"

	// commentPublishRK：发表评论的路由键。
	commentPublishRK = "comment.publish"
	// commentDeleteRK：删除评论的路由键。
	commentDeleteRK = "comment.delete"
)

// CommentEvent 是评论消息的事件体。
//
// 设计细节：因为"发表"和"删除"两种动作携带的信息不同（删除只需要评论 ID），
// 其他字段都加了 omitempty——没用上的字段在 JSON 里就不输出，消息体更短。
type CommentEvent struct {
	// EventID 事件唯一编号。
	EventID string `json:"event_id"`
	// Action 动作名（"publish" / "delete"）。
	Action string `json:"action"`
	// CommentID 评论编号（删除时主要用它）；omitempty 为 0 时不输出。
	CommentID uint `json:"comment_id,omitempty"`
	// Username 评论者用户名（发表时带）。
	Username string `json:"username,omitempty"`
	// VideoID 评论所属视频编号。
	VideoID uint `json:"video_id,omitempty"`
	// AuthorID 评论者用户编号（用于通知、@提及）。
	AuthorID uint `json:"author_id,omitempty"`
	// Content 评论正文。
	Content string `json:"content,omitempty"`
	// OccurredAt 事件发生时刻。
	OccurredAt time.Time `json:"occurred_at"`
}

// NewCommentMQ 函数的作用：创建评论发布器——开通道、声明评论拓扑。
//
// 参数 base：总连接；
// 返回值：*CommentMQ 发布器、error 错误。
func NewCommentMQ(base *RabbitMQ) (*CommentMQ, error) {
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
	// 声明评论交换机+队列+绑定。
	if err := DeclareTopic(ch, commentExchange, commentQueue, commentBindingKey); err != nil {
		// 声明失败：关闭通道并返回错误。
		ch.Close()
		// 返回错误。
		return nil, err
	}
	// 成功：构造发布器返回。
	return &CommentMQ{ch: ch}, nil
}

// Publish 是 CommentMQ 的方法：发布一条"发表评论"事件。
//
// 参数：c 发布器、ctx 上下文、username 评论者用户名、videoID 视频编号、authorID 评论者编号、content 评论内容；
// 返回值 error：发布错误。
func (c *CommentMQ) Publish(ctx context.Context, username string, videoID, authorID uint, content string) error {
	// 转调内部 publish：动作 "publish"、路由键 commentPublishRK，事件体直接组装好传入。
	return c.publish(ctx, "publish", commentPublishRK, CommentEvent{
		// Username 带上评论者用户名。
		Username: username,
		// VideoID 带上视频编号。
		VideoID: videoID,
		// AuthorID 带上评论者编号。
		AuthorID: authorID,
		// Content 带上评论正文。
		Content: content,
	})
}

// Delete 是 CommentMQ 的方法：发布一条"删除评论"事件。
//
// 参数：c 发布器、ctx 上下文、commentID 要删除的评论编号；
// 返回值 error：发布错误。
func (c *CommentMQ) Delete(ctx context.Context, commentID uint) error {
	// 转调内部 publish：动作 "delete"、路由键 commentDeleteRK，事件只带评论 ID。
	return c.publish(ctx, "delete", commentDeleteRK, CommentEvent{
		// CommentID 指明删除哪条评论。
		CommentID: commentID,
	})
}

// publish 是 CommentMQ 的内部方法：给事件补全 ID/动作/时间，然后发布（Publish 和 Delete 共用）。
//
// 参数：c 发布器、ctx 上下文、action 动作名、routingKey 路由键、evt 已部分填好的事件体；
// 返回值 error：错误。
func (c *CommentMQ) publish(ctx context.Context, action, routingKey string, evt CommentEvent) error {
	// 发布器或通道不存在。
	if c == nil || c.ch == nil {
		// 返回未初始化错误。
		return errors.New("comment mq is not initialized")
	}
	// id 生成 16 字节随机事件 ID；err 接收错误。
	id, err := newEventID(16)
	if err != nil {
		// 失败：返回错误。
		return err
	}
	// evt.EventID = id：给传进来的事件补上唯一 ID（这里直接修改原事件结构体）。
	evt.EventID = id
	// evt.Action = action：补上动作名。
	evt.Action = action
	// evt.OccurredAt = time.Now().UTC()：补上事件时刻；UTC() 转成世界标准时间（避免时区歧义）。
	evt.OccurredAt = time.Now().UTC()
	// PublishJSON 发布到评论交换机，返回其错误。
	return PublishJSON(ctx, c.ch, commentExchange, routingKey, evt)
}
