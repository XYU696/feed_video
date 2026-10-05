// package video：视频业务包。本文件只有一个函数，
// 是点赞/评论服务在"热度 MQ 发不出去"时调用的降级方案：直接更新 Redis 热度。
package video

import (
	// context：超时控制。
	"context"
	// strconv：视频编号转字符串。
	"strconv"
	// time：取当前分钟、格式化时间。
	"time"

	// rediscache：Redis 客户端类型。
	rediscache "feedsystem_video_go/internal/middleware/redis"
)

// 更新视频流行度缓存
//
// UpdatePopularityCache 函数：直接在 Redis 上完成两件事——
//  1. 让该视频的详情/实体缓存失效（热度变了，旧缓存不能再用）；
//  2. 给"当前这一分钟的热榜桶"里该视频的分数加上 change。
//
// 这样即使没有 MQ、没有 worker，热门榜单依然能实时累计。
//
// 参数：ctx 请求上下文、cache Redis 客户端、id 视频编号、change 热度增量（+1/-1）；
// 无返回值（缓存操作失败被刻意忽略，属于尽力而为）。
func UpdatePopularityCache(ctx context.Context, cache *rediscache.Client, id uint, change int64) {
	// 无需操作的情况：没配缓存、编号无效、增量为 0。
	if cache == nil || id == 0 || change == 0 {
		// 直接返回。
		return
	}

	// 删除视频【详情缓存】，用 background 上下文（此操作不依附具体请求生命周期）。
	_ = cache.Del(context.Background(), cache.Key("video:detail:id=%d", id))
	// 删除视频【实体缓存】（另一种可能的缓存键，一并清掉，防止读到旧热度）。
	_ = cache.Del(context.Background(), cache.Key("video:entity:%d", id))

	// now：当前 UTC 时间截断到分钟（同一分钟内的所有点赞/评论落进同一个桶）。
	now := time.Now().UTC().Truncate(time.Minute)
	// windowKey 拼这一分钟的热榜桶键 hot:video:1m:202609271530。
	windowKey := cache.Key("hot:video:1m:%s", now.Format("200601021504"))
	// member 用视频编号字符串当 ZSET 成员。
	member := strconv.FormatUint(uint64(id), 10)

	// opCtx 给 Redis 操作设 50ms 超时；cancel 取消函数。
	opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	// defer 释放上下文。
	defer cancel()

	// ZincrBy：分钟桶里该视频分数 += change（原子操作，并发点赞也不会丢）。
	_ = cache.ZincrBy(opCtx, windowKey, member, float64(change))
	// Expire：给分钟桶设 2 小时过期，老桶自动清理。
	_ = cache.Expire(opCtx, windowKey, 2*time.Hour)
}
