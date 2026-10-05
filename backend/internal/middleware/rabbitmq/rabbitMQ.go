// package rabbitmq：RabbitMQ 中间件包。
// 本文件是 MQ 部分的"地基"：管理连接、提供创建 Channel 的方法，
// 以及三个所有业务 MQ 都会复用的通用工具——声明 Topic 拓扑(DeclareTopic)、
// 发送 JSON 消息(PublishJSON)、生成事件 ID(newEventID)。
package rabbitmq

import (
	// context：控制发布操作的超时/取消。
	"context"
	// crypto/rand：生成事件 ID 用的随机字节。
	"crypto/rand"
	// encoding/hex：随机字节转十六进制字符串。
	"encoding/hex"
	// encoding/json：把事件结构体编码成 JSON 字节。
	"encoding/json"
	// errors：返回参数错误。
	"errors"
	// config：RabbitMQ 配置类型。
	"feedsystem_video_go/internal/config"
	// log：DLX 声明失败时打日志。
	"log"
	// strconv：端口转字符串。
	"strconv"
	// time：消息时间戳。
	"time"

	// amqp：官方 RabbitMQ 客户端库（amqp091-go 是社区维护的延续版本）。
	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitMQ 结构体只管理一条 TCP 连接。
//
// 重点技术点——Connection 与 Channel 的分工：
//
//	Connection：一条 TCP 长连接（创建成本高）；
//	Channel   ：建立在 Connection 之上的"轻量虚拟通道"（创建成本低），
//	           真正收发命令都走 Channel。
//
// 设计上这里只持有 Connection，各个业务 MQ / Worker 用到时再各自 NewChannel()，
// 互不干扰（Channel 不适合被多个 goroutine 同时使用）。
type RabbitMQ struct {
	// Conn 是底层 AMQP 连接指针，公开字段，本包其他代码和外部都要用它。
	Conn *amqp.Connection
}

// NewRabbitMQ 函数的作用：根据配置拨号，建立并返回 RabbitMQ 连接。
//
// 参数 cfg：MQ 连接配置指针；
// 返回值：*RabbitMQ 连接包装对象、error 拨号失败错误。
func NewRabbitMQ(cfg *config.RabbitMQConfig) (*RabbitMQ, error) {
	// 配置为空时。
	if cfg == nil {
		// 返回错误，没法建立连接。
		return nil, errors.New("rabbitmq config is nil")
	}
	// url 变量拼出 AMQP 连接字符串，格式：amqp://用户名:密码@主机:端口/
	url := "amqp://" + cfg.Username + ":" + cfg.Password + "@" + cfg.Host + ":" + strconv.Itoa(cfg.Port) + "/"
	// conn 变量接收拨号建立的连接；err 接收错误。amqp.Dial(url) 按连接字符串建立 TCP 连接并完成 AMQP 握手。
	conn, err := amqp.Dial(url)
	if err != nil {
		// 拨号失败：返回 nil 和错误（调用方据此决定禁用 MQ 或重试）。
		return nil, err
	}
	// 成功：用连接包装成 RabbitMQ 对象返回。
	return &RabbitMQ{Conn: conn}, nil
}

// Close 是 RabbitMQ 的方法：关闭连接、释放资源。
//
// 参数 r：连接对象指针；
// 返回值 error：关闭错误，成功为 nil。
func (r *RabbitMQ) Close() error {
	// r == nil 允许对空指针调用（方便 defer 里无脑关闭）。
	if r == nil {
		// 没东西可关，返回 nil。
		return nil
	}
	// 连接存在时。
	if r.Conn != nil {
		// r.Conn.Close() 关闭 TCP 连接并返回错误。
		return r.Conn.Close()
	}
	// 连接字段为空：返回 nil。
	return nil
}

// NewChannel 是 RabbitMQ 的方法：在当前连接上打开一个新的 Channel。
//
// 参数 r：连接对象；
// 返回值：*amqp.Channel 新通道、error 打开失败错误。
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error) {
	// 对象或底层连接不存在。
	if r == nil || r.Conn == nil {
		// 返回错误。
		return nil, errors.New("rabbitmq connection is not initialized")
	}
	// r.Conn.Channel() 在 TCP 连接上开辟一个虚拟通道并返回。
	return r.Conn.Channel()
}

// DeclareTopic 函数的作用：一次性声明"Topic 交换机 + 持久化队列(带死信配置) + 绑定关系"，
// 是所有业务 MQ 初始化时共用的拓扑声明工具。
//
// 参数：
//   - ch：用来声明的通道；
//   - exchange：交换机名字；
//   - queue：队列名字；
//   - bindingKey：绑定键（队列按它过滤消息，支持 * # 通配符）；
//
// 返回值 error：任何一步声明失败的错误。
//
// 技术点：声明(declare)是"幂等"的——同名拓扑只要参数完全一致，重复声明不会出错，
// 所以生产者和消费者两边都可以放心各自声明一遍。
func DeclareTopic(ch *amqp.Channel, exchange string, queue string, bindingKey string) error {
	// 通道为空。
	if ch == nil {
		// 返回错误（本函数只有一个返回值 error，不能写成 return nil, err）。
		return errors.New("channel is not initialized")
	}
	// 交换机名、队列名、绑定键有任何一个为空。
	if exchange == "" || queue == "" || bindingKey == "" {
		// 返回参数错误。
		return errors.New("exchange/queue/bindingKey is required")
	}

	// ch.ExchangeDeclare(...) 声明交换机。参数依次是：
	if err := ch.ExchangeDeclare(
		// exchange：交换机名字。
		exchange,
		// "topic"：交换机类型。Topic 类型按 routing key 的【模式匹配】路由（* 一个词、# 多个词）。
		"topic",
		// true：durable 持久化——RabbitMQ 重启后交换机还在。
		true,
		// false：autoDelete——【不】自动删除（最后一个消费者断开后也保留）。
		false,
		// false：internal——不是内部交换机（允许生产者直接往里发消息）。
		false,
		// false：noWait——不使用"声明后不等服务器确认"模式（要等确认，声明失败能立刻知道）。
		false,
		// nil：额外参数（没有）。
		nil,
	); err != nil {
		// 声明失败：直接返回错误。
		return err
	}

	// q 变量接收声明好的队列对象（里面带队列名等服务器生成的信息）；err 接收错误。
	// ch.QueueDeclare(...) 参数依次是：
	q, err := ch.QueueDeclare(
		// queue：队列名字。
		queue,
		// true：durable 持久化队列（重启后队列元数据还在）。
		true,
		// false：autoDelete 不自动删除。
		false,
		// false：exclusive 非独占（只归当前连接专用的队列才设 true）。
		false,
		// false：noWait 要等服务器确认。
		false,
		// amqp.Table{...}：队列的额外参数表（类似 map）。
		// "x-dead-letter-exchange": DLXExchange：
		//   给队列配置【死信交换机】——消息被拒绝且不重回/过期/超长时，会被转投到这个交换机。
		amqp.Table{"x-dead-letter-exchange": DLXExchange},
	)
	if err != nil {
		// 队列声明失败：返回错误。
		return err
	}

	// ch.QueueBind(...) 把队列绑定到交换机。参数依次是：
	if err := ch.QueueBind(
		// q.Name：要绑定的队列名（用服务器确认后的真实名字）。
		q.Name,
		// bindingKey：绑定键，消息的 routing key 匹配它时才会路由进这个队列。
		bindingKey,
		// exchange：绑到哪个交换机。
		exchange,
		// false：noWait 要等确认。
		false,
		// nil：额外参数。
		nil,
	); err != nil {
		// 绑定失败：返回错误。
		return err
	}
	// DeclareDLX(ch, queue)：顺带声明死信交换机和对应死信队列；
	if err := DeclareDLX(ch, queue); err != nil {
		// 死信部分失败只记日志，不让它影响主拓扑的使用（DLX 属于增强能力）。
		log.Printf("DLX declare failed for %s: %v", queue, err)
	}
	// 全部成功：返回 nil。
	return nil
}

// PublishJSON 函数的作用：把任意事件对象编码成 JSON，发布到指定交换机。
//
// 参数：ctx 上下文、ch 发布通道、exchange 目标交换机、routingKey 路由键、payload 任意事件对象；
// 返回值 error：编码/发布错误。
func PublishJSON(ctx context.Context, ch *amqp.Channel, exchange string, routingKey string, payload any) error {
	// 通道为空。
	if ch == nil {
		// 返回错误。
		return errors.New("channel is not initialized")
	}
	// 交换机或路由键为空。
	if exchange == "" || routingKey == "" {
		// 返回参数错误。
		return errors.New("exchange and routingKey are required")
	}
	// b 变量接收事件编码后的 JSON 字节；err 接收编码错误。
	b, err := json.Marshal(payload)
	if err != nil {
		// 编码失败：返回错误。
		return err
	}
	// ch.PublishWithContext(ctx, exchange, routingKey, false, false, amqp.Publishing{...})：
	return ch.PublishWithContext(ctx, exchange, routingKey, false, false, amqp.Publishing{
		// ContentType 告诉消费者消息体是 JSON。
		ContentType: "application/json",
		// DeliveryMode: amqp.Persistent：消息持久化——
		// 技术点：消息会被写入磁盘，broker 重启不丢。
		// 注意"消息持久化"要配合"队列持久化"才有完整效果（两者这里都做了）。
		DeliveryMode: amqp.Persistent,
		// Timestamp 记录消息产生时刻（broker/消费者可用于排查、排序）。
		Timestamp: time.Now(),
		// Body 是真正的消息内容（JSON 字节）。
		Body: b,
	})
}

// newEventID 函数的作用：生成 2n 个字符长的随机十六进制事件 ID。
//
// 参数 n：随机字节数（每个字节能编码成 2 个十六进制字符）；
// 返回值：string 事件 ID、error 随机数生成失败错误。
//
// 用途：每条事件带全局唯一 ID，方便链路追踪、日志排查，也可作为将来消费去重的依据。
func newEventID(n int) (string, error) {
	// b 创建长度 n 的字节切片。
	b := make([]byte, n)
	// rand.Read(b) 填充随机数，err 接收错误。
	if _, err := rand.Read(b); err != nil {
		// 失败：返回空串和错误。
		return "", err
	}
	// 成功：hex.EncodeToString 把字节编码成十六进制字符串返回。
	return hex.EncodeToString(b), nil
}
