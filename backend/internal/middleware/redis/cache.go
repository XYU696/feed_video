// package redis：Redis 中间件包（本文件提供最基础的字符串读写/删除/批量取操作）。
package redis

import (
	// context：控制操作超时/取消。
	"context"
	// errors：返回"客户端未初始化"错误。
	"errors"
	// time：设置 TTL。
	"time"
)

// GetBytes 是 Client 的方法：根据 key 取出对应的值（字节切片形式）。
//
// 参数：c 客户端、ctx 上下文、key 键名；
// 返回值：[]byte 取到的值、error（key 不存在时是 redis.Nil，可用 IsMiss 判断；其他是真正的故障）。
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 nil 值和"未初始化"错误。
		return nil, errors.New("redis client not initialized")
	}
	// c.rdb.Get(ctx, key) 发出 GET 命令；.Bytes() 把返回值转成字节切片（连同错误一起返回）。
	return c.rdb.Get(ctx, key).Bytes()
}

// SetBytes 是 Client 的方法：把一个值写入指定 key，并设置过期时间。
//
// 参数：c 客户端、ctx 上下文、key 键名、value 字节切片值、ttl 存活时间（传 0 表示永久）；
// 返回值 error：写入错误，成功为 nil。
func (c *Client) SetBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回"未初始化"错误。
		return errors.New("redis client not initialized")
	}
	// c.rdb.Set(ctx, key, value, ttl) 发出 SET 命令（同时带过期时间）；.Err() 取错误返回。
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del 是 Client 的方法：删除一个或多个 key。
//
// 参数：c 客户端、ctx 上下文、key 要删除的键名（这里一次删一个）；
// 返回值 error：删除错误，成功为 nil。
func (c *Client) Del(ctx context.Context, key string) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回"未初始化"错误。
		return errors.New("redis client not initialized")
	}
	// c.rdb.Del(ctx, key) 发出 DEL 命令；.Err() 取错误返回。
	return c.rdb.Del(ctx, key).Err()
}

// DelByPattern 是 Client 的方法：按"匹配模式"批量删除 key（比如删一批同类缓存）。
//
// 参数：c 客户端、ctx 上下文、pattern 匹配模式（支持 * 等通配符，例如 "video:detail:*"）；
// 返回值 error：扫描/删除过程中的错误。
func (c *Client) DelByPattern(ctx context.Context, pattern string) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 没东西可删，返回 nil。
		return nil
	}
	// iter 变量是一个"扫描迭代器"。
	// c.rdb.Scan(ctx, 0, pattern, 0)：
	//   - 技术点：用 SCAN 而不是 KEYS 命令。KEYS 会一次性列出全部匹配 key，key 特别多时会卡住 Redis；
	//     SCAN 是分批游标式扫描，每次只扫一小部分，不阻塞服务器。
	//   - 参数依次是：上下文、游标(0 表示从头开始)、匹配模式、每次返回数量(0 用默认值)；
	//   - .Iterator() 把扫描结果包装成可以逐个取值的迭代器。
	iter := c.rdb.Scan(ctx, 0, pattern, 0).Iterator()
	// for iter.Next(ctx)：每次调用推进到下一个匹配的 key，没有更多时返回 false 结束循环。
	for iter.Next(ctx) {
		// iter.Val() 取当前这个 key；c.rdb.Del 删除它；下划线丢掉删除命令的错误（单个删失败不中断整体）。
		_ = c.rdb.Del(ctx, iter.Val())
	}
	// iter.Err() 返回扫描过程中是否出错，作为函数结果。
	return iter.Err()
}

// MGet 是 Client 的方法：一次性取出多个 key 的值（批量读取，减少网络往返）。
//
// 参数：c 客户端、cacheCtx 上下文（通常带很短超时）、cacheKeys ...string 可变参数，若干个 key；
// 返回值：
//   - []interface{}：结果列表，顺序和传入的 key 一一对应；某个 key 不存在时该位置是 nil；
//   - error：命令错误。
func (c *Client) MGet(cacheCtx context.Context, cacheKeys ...string) ([]interface{}, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 nil 结果和"未初始化"错误。
		return nil, errors.New("redis client not initialized")
	}
	// c.rdb.MGet(cacheCtx, cacheKeys...) 发出 MGET 命令（cacheKeys... 把 key 切片展开逐个传入）；
	// .Result() 取出结果列表和错误返回。
	return c.rdb.MGet(cacheCtx, cacheKeys...).Result()
}
