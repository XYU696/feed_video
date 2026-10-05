// package redis：Redis 中间件包（本文件专门封装 ZSET 有序集合的操作）。
//
// 先通俗理解 ZSET：它是 Redis 的一种数据类型，类似一个"自动排序的集合"，
// 每个元素(member)都带一个分数(score)，Redis 始终按分数帮你排好序。
// 本项目用它做两件大事：全局 Feed 时间线（分数=发布时间）、热榜（分数=热度）。
package redis

import (
	// context：控制超时/取消。
	"context"
	// errors：返回"未初始化"错误。
	"errors"
	// time：设置过期时间。
	"time"

	// redis：官方客户端包，这里用它的 Z、ZRangeBy、ZStore 等类型。
	redis "github.com/redis/go-redis/v9"
)

// ZincrBy 是 Client 的方法：给 ZSET 里某个元素的分数【增减一个增量】（元素不存在就先创建）。
//
// 用途：热度变化。点赞 +1、取消 -1，直接累加进"按分钟分桶"的热榜 ZSET。
//
// 参数：c 客户端、ctx 上下文、key 集合键名、member 元素（这里是视频 ID 字符串）、score 增量（正数加、负数减）；
// 返回值 error：命令错误。
func (c *Client) ZincrBy(ctx context.Context, key string, member string, score float64) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// Redis 不可用时静默返回 nil（热度更新属于可降级操作）。
		return nil
	}
	// c.rdb.ZIncrBy(ctx, key, score, member) 发出 ZINCRBY 命令；.Err() 取错误返回。
	return c.rdb.ZIncrBy(ctx, key, score, member).Err()
}

// ZAdd 是 Client 的方法：向 ZSET 里添加一个或多个元素（带分数）；同元素已存在则更新分数。
//
// 用途：往全局时间线里写入新视频；空时间线重建时批量写入。
//
// 参数：c 客户端、ctx 上下文、key 集合键名、members ...redis.Z 可变参数，每个 redis.Z 含一个元素和它的分数；
// 返回值 error：命令错误。
func (c *Client) ZAdd(ctx context.Context, key string, members ...redis.Z) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 静默返回 nil。
		return nil
	}
	// c.rdb.ZAdd(ctx, key, members...) 发出 ZADD 命令（members... 展开批量写入）；.Err() 取错误。
	return c.rdb.ZAdd(ctx, key, members...).Err()
}

// ZRemRangeByRank 是 Client 的方法：按"排名位置区间"删除元素。
//
// 用途：裁剪全局时间线，只保留最新的 1000 条。
//
// 参数：c 客户端、ctx 上下文、key 集合键名、start 起始排名、stop 结束排名；
// 返回值 error：命令错误。
func (c *Client) ZRemRangeByRank(ctx context.Context, key string, start int64, stop int64) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 静默返回 nil。
		return nil
	}
	// c.rdb.ZRemRangeByRank(ctx, key, start, stop) 发出删除命令；.Err() 取错误。
	return c.rdb.ZRemRangeByRank(ctx, key, start, stop).Err()
}

// ZRangeWithScores 是 Client 的方法：按"排名位置区间"取出元素，连同分数一起返回。
//
// 用途：取全局时间线里"最老的一条"（排名第 0），用来确定冷热分界线。
// 注意 ZRANGE 默认按分数【从小到大】取（升序）。
//
// 参数：c 客户端、ctx 上下文、key 键名、start 起始位置、stop 结束位置（-1 表示到最后）；
// 返回值：[]redis.Z 元素+分数列表、error 命令错误。
func (c *Client) ZRangeWithScores(ctx context.Context, key string, start int64, stop int64) ([]redis.Z, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 nil 列表和"未初始化"错误。
		return nil, errors.New("redis client not initialized")
	}
	// c.rdb.ZRangeWithScores(...) 发出命令；.Result() 取 []redis.Z 结果和错误返回。
	return c.rdb.ZRangeWithScores(ctx, key, start, stop).Result()
}

// Expire 是 Client 的方法：给一个 key 设置过期时间（刷新 TTL）。
//
// 参数：c 客户端、ctx 上下文、key 键名、ttl 存活时长；
// 返回值 error：命令错误。
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 静默返回 nil。
		return nil
	}
	// c.rdb.Expire(ctx, key, ttl) 发出 EXPIRE 命令；.Err() 取错误。
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// ZUnionStore 是 Client 的方法：把多个 ZSET 按某种聚合方式合并，结果存入一个新的 key。
//
// 用途（核心）：生成热榜快照——把最近 60 个"按分钟分桶"的 ZSET 用 SUM（分数相加）合并，
// 得到每个视频最近 60 分钟的总热度。
//
// 参数：c 客户端、ctx 上下文、dst 合并结果存放的目标 key、keys 待合并的多个 key、aggregate 聚合方式（"SUM"/"MIN"/"MAX"）；
// 返回值 error：命令错误。
func (c *Client) ZUnionStore(ctx context.Context, dst string, keys []string, aggregate string) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 静默返回 nil。
		return nil
	}
	// c.rdb.ZUnionStore(ctx, dst, &redis.ZStore{...}) 发出 ZUNIONSTORE 命令：
	return c.rdb.ZUnionStore(ctx, dst, &redis.ZStore{
		// Keys 是要合并的源 key 列表。
		Keys: keys,
		// Aggregate 是聚合方式（SUM 表示同元素分数相加）。
		Aggregate: aggregate,
		// .Err() 取错误返回。
	}).Err()
}

// Exists 是 Client 的方法：判断一个 key 是否存在。
//
// 参数：c 客户端、ctx 上下文、key 键名；
// 返回值：bool true 表示存在、error 命令错误。
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 false（当作不存在）、nil。
		return false, nil
	}
	// n 变量接收"存在的 key 数量"（EXISTS 可同时查多个 key），err 接收错误。
	n, err := c.rdb.Exists(ctx, key).Result()
	// n > 0 表示该 key 存在，连同错误一起返回。
	return n > 0, err
}

// ZRevRange 是 Client 的方法：按"排名位置区间"取出元素，但是按分数【从大到小】（倒序）取。
//
// 用途：在热榜快照上按 offset 取一页视频 ID（第 1 名、第 2 名...）。
//
// 参数：c 客户端、ctx 上下文、key 键名、start 起始位置、stop 结束位置；
// 返回值：[]string 元素列表（不带分数）、error 命令错误。
func (c *Client) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 nil 列表、nil。
		return nil, nil
	}
	// c.rdb.ZRevRange(...) 发出 ZREVRANGE 命令；.Result() 取元素列表和错误返回。
	return c.rdb.ZRevRange(ctx, key, start, stop).Result()
}

// ZRevRangeByScore 是 Client 的方法：按"分数区间"倒序取出元素（还可跳过前 offset 个、最多取 count 个）。
//
// 用途：从全局时间线里取"分数不超过 max 的最新 count 条"——最新流的热数据查询就靠它。
//
// 参数：c 客户端、ctx 上下文、key 键名、max 分数上限（字符串，比如 "+inf" 正无穷）、min 分数下限、offset 跳过几个、count 最多取几个（0 表示不限制）；
// 返回值：[]string 元素列表、error 命令错误。
func (c *Client) ZRevRangeByScore(ctx context.Context, key string, max, min string, offset, count int64) ([]string, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回 nil 列表、nil。
		return nil, nil
	}
	// c.rdb.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{...}) 发出命令，用 ZRangeBy 结构体描述查询条件：
	return c.rdb.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{
		// Max 分数上限。
		Max: max,
		// Min 分数下限。
		Min: min,
		// Offset 跳过前几个元素（一般为 0）。
		Offset: offset,
		// Count 最多返回多少条（就是每页条数）。
		Count: count,
		// .Result() 取元素列表和错误返回。
	}).Result()
}
