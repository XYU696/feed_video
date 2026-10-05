// package rabbitmq：RabbitMQ 中间件包（本文件负责死信交换机 DLX 的声明，
// 以及从消息头读取"已被死信次数"的工具函数）。
package rabbitmq

// log：声明成功时打印日志。
import "log"

// amqp：RabbitMQ 客户端，用它的 Table 类型读消息头。
import amqp "github.com/rabbitmq/amqp091-go"

const (
	// DLXExchange 常量：死信交换机的统一名字。
	// 所有业务队列声明时都把死信指向它（x-dead-letter-exchange）。
	DLXExchange = "dlx.events"
	// MaxRetryCount 常量：最大重试次数（3 次）。
	MaxRetryCount = 3
)

// DeclareDLX 函数的作用：声明死信交换机 + 一个以业务队列名命名的死信队列，并绑定。
//
// 参数：ch 通道、queueName 对应的业务队列名（用来给死信队列起名，区分不同来源的死信）；
// 返回值 error：声明过程中的错误。
//
// 业务理解：正常处理失败、无法再用的消息应该有个"善后去处"，
// 统一进死信队列后可以人工排查原因或后续补偿处理，而不是凭空消失。
func DeclareDLX(ch *amqp.Channel, queueName string) error {
	// 通道为空时直接返回（不视为错误，避免 nil 调用崩溃）。
	if ch == nil {
		// 返回 nil。
		return nil
	}
	// ch.ExchangeDeclare(...) 声明死信交换机：
	if err := ch.ExchangeDeclare(
		// DLXExchange：交换机名 "dlx.events"。
		DLXExchange,
		// "topic"：也用 Topic 类型，方便按 routing key 区分死信来源。
		"topic",
		// true：持久化。
		true,
		// false：不自动删除。
		false,
		// false：非内部。
		false,
		// false：要等确认。
		false,
		// nil：无额外参数。
		nil,
	); err != nil {
		// 失败：返回错误。
		return err
	}
	// dlxQueue 变量给死信队列起名：在业务队列名后加 ".dlx"（比如 like.events.dlx），一眼能看出死信来源。
	dlxQueue := queueName + ".dlx"
	// ch.QueueDeclare(...) 声明死信队列（注意最后一个参数是 nil：死信队列自己不再配死信交换机，避免循环）。
	_, err := ch.QueueDeclare(
		// dlxQueue：队列名。
		dlxQueue,
		// true：持久化。
		true,
		// false：不自动删除。
		false,
		// false：非独占。
		false,
		// false：要等确认。
		false,
		// nil：无额外参数。
		nil,
	)
	if err != nil {
		// 声明失败：返回错误。
		return err
	}
	// ch.QueueBind(dlxQueue, "#", DLXExchange, false, nil)：
	//   "#" 是 Topic 绑定键，匹配【任意多个单词】——也就是不管死信的 routing key 是什么，全部收进这个死信队列。
	if err := ch.QueueBind(dlxQueue, "#", DLXExchange, false, nil); err != nil {
		// 绑定失败：返回错误。
		return err
	}
	// 打印日志：死信交换机和队列已准备好，方便确认拓扑生效。
	log.Printf("DLX ready: exchange=%s queue=%s", DLXExchange, dlxQueue)
	// 成功：返回 nil。
	return nil
}

// GetRetryCount 函数的作用：从一条消息的 x-death 头里，读出它已经被死信过多少次。
//
// 技术点——x-death 是什么：消息被投入死信交换机时，broker 会自动在消息头里
// 追加/更新一个名为 x-death 的数组，记录"它在哪个队列死过、死了几次、原因、时间"。
// 做"延迟重试队列"时，靠它判断重试次数，超过上限就不再重试。
//
// 参数 d：一条投递过来的消息；
// 返回值 int：已死信次数（读不到时返回 0）。
func GetRetryCount(d amqp.Delivery) int {
	// deaths 变量尝试取消息头里的 x-death，并做类型断言：
	//   d.Headers["x-death"] 取出头字段；.([]interface{}) 断言成"任意类型的切片"；ok 表示是否存在且类型正确。
	deaths, ok := d.Headers["x-death"].([]interface{})
	// !ok 头不存在，或 len(deaths) == 0 数组为空。
	if !ok || len(deaths) == 0 {
		// 没有死信记录：返回 0。
		return 0
	}
	// death 变量取数组第一个元素，断言成 amqp.Table（最新一次死信记录排在最前）。
	death, ok := deaths[0].(amqp.Table)
	if !ok {
		// 类型不符：返回 0。
		return 0
	}
	// count 变量从记录表里取 "count" 字段（这次一共死了多少条），断言成 int64。
	count, ok := death["count"].(int64)
	if !ok {
		// 取不到：返回 0。
		return 0
	}
	// int(count) 把 int64 转成 int 返回。
	return int(count)
}
