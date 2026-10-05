// package feed：信息流业务包。本文件是服务层，是整个项目里缓存设计最复杂的一块：
//  1. GetVideoByIDs：L1 本地缓存 → L2 Redis → L3 MySQL 的三级缓存；
//  2. ListLatest：冷热分离的时间线（Redis ZSET 放热数据，MySQL 放冷数据）；
//  3. 关注流：Cache-Aside + 分布式锁；
//  4. 热度榜：Redis 分钟桶 ZUNIONSTORE 合并 + MySQL 游标兜底。
package feed

import (
	// context：上下文。
	"context"
	// encoding/json：缓存对象的序列化/反序列化。
	"encoding/json"
	// rediscache：项目自己封装的 Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// video：用到 LikeRepository 和 Video 结构体。
	"feedsystem_video_go/internal/video"
	// fmt：拼字符串（如 "+inf"、数字转串）。
	"fmt"
	// log：缓存降级时打日志。
	"log"
	// strconv：字符串和数字互转。
	"strconv"
	// sync：WaitGroup 等并发、Mutex 保护 map。
	"sync"
	// time：超时、游标、时间格式化。
	"time"

	// cache：第三方进程内本地缓存库（类似 Python 的一个 dict，但带过期时间）。
	"github.com/patrickmn/go-cache"
	// redis：go-redis 客户端，用到 redis.Z（ZSET 元素类型）。
	redis "github.com/redis/go-redis/v9"
	// singleflight：同一时刻相同 key 的并发调用只放一个去真查，其余等结果共享。
	"golang.org/x/sync/singleflight"
)

// FeedService 结构体：信息流服务层，持有仓储、点赞仓储和两级缓存。
type FeedService struct {
	// repo 信息流仓储，查 MySQL。
	repo *FeedRepository
	// likeRepo 点赞仓储，批量查"当前用户是否点赞过"。
	likeRepo *video.LikeRepository
	// rediscache L2 分布式缓存客户端（可能为 nil）。
	rediscache *rediscache.Client
	// localcache L1 进程内本地缓存（3 秒过期，极短）。
	localcache *cache.Cache
	// cacheTTL 关注流缓存的过期时间（24 小时）。
	cacheTTL time.Duration
	// requestGroup singleflight 组：用来做"同 key 并发合并"。
	requestGroup singleflight.Group
}

// CachedFeedData 结构体：缓存里公共视频列表的包装格式。
type CachedFeedData struct {
	// PublicVideos 公共视频列表；json tag 指定序列化后的字段名。
	PublicVideos []video.Video `json:"public_videos"`
}

// NewFeedService 是构造函数：创建信息流服务。
//
// 参数：repo 信息流仓储、likeRepo 点赞仓储、rediscache Redis 客户端；
// 返回值 *FeedService：服务指针。
func NewFeedService(repo *FeedRepository, likeRepo *video.LikeRepository, rediscache *rediscache.Client) *FeedService {
	// 装配：cache.New(默认3秒过期, 每5秒清理一次过期项)；关注流缓存 TTL 24 小时。
	return &FeedService{repo: repo, likeRepo: likeRepo, rediscache: rediscache, localcache: cache.New(3*time.Second, 5*time.Second), cacheTTL: 24 * time.Hour}
}

// GetVideoByIDs 方法：按一批视频 ID 批量取视频详情，走三级缓存。
//
// 参数 videoIDs：要取的视频编号；
// 返回值：按入参顺序排好的视频指针切片、错误。
func (f *FeedService) GetVideoByIDs(ctx context.Context, videoIDs []uint) ([]*video.Video, error) {
	// GetVideoByIDs 批量获取视频信息
	// 采用 L1(本地缓存) -> L2(Redis) -> L3(MySQL) 三级架构
	// 没有任何 ID：直接返回空切片（不走任何缓存/DB）。
	if len(videoIDs) == 0 {
		// 空切片保证 JSON 是 []。
		return []*video.Video{}, nil
	}

	// videoMap 收集"已经取到"的视频：key 是视频 ID，value 是视频指针。
	videoMap := make(map[uint]*video.Video)
	//L1:本地缓存
	// missedL1 收集 L1 没命中的 ID。
	var missedL1 []uint
	// 遍历入参 ID；id 是当前视频编号。
	for _, id := range videoIDs {
		// cacheKey 拼该视频的实体缓存键，如 video:entity:123。
		cacheKey := f.rediscache.Key("video:entity:%d", id)
		// 本地缓存对象存在（防御性判断）。
		if f.localcache != nil {
			// 从本地缓存取；v 是存的值，found 表示是否命中。
			if v, found := f.localcache.Get(cacheKey); found {
				// 类型断言：存进去的是 video.Video 值类型；data 是断言后的值，ok 表示类型对不对。
				if data, ok := v.(video.Video); ok {
					// 命中：取地址放进结果 map（每个请求一个新指针，避免共享被改）。
					videoMap[id] = &data
					// continue 跳过下面的"记为未命中"。
					continue
				}
			}
		}
		// 记录未命中的 ID，准备进入下一级缓存
		missedL1 = append(missedL1, id)
	}

	// L1 全部命中：直接按入参顺序组装结果返回。
	if len(missedL1) == 0 {
		// buildOrderedResult 保证顺序和 videoIDs 一致（map 本身无序）。
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	//L2:redis
	// missedL2 收集 L2 也没命中的 ID。
	var missedL2 []uint
	// 有 L1 未命中的才需要查 Redis。
	if len(missedL1) > 0 {
		// cacheKeys 拼所有未命中 ID 对应的缓存键，长度等于 missedL1。
		cacheKeys := make([]string, len(missedL1))
		// 遍历未命中 ID；i 是下标、id 是编号。
		for i, id := range missedL1 {
			// 对应位置放缓存键。
			cacheKeys[i] = f.rediscache.Key("video:entity:%d", id)
		}

		// cacheCtx 带 50ms 超时的上下文；cancel 是释放函数。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// MGet 一次批量取所有键（比循环 Get 少往返）；results 是结果数组、err 错误。
		results, err := f.rediscache.MGet(cacheCtx, cacheKeys...)
		// 立即 cancel 释放资源（即使查询已结束）。
		cancel()

		// Redis 查询成功。
		if err == nil {
			// 逐个处理结果；i 下标、res 是该键的值（未命中的键是 nil）。
			for i, res := range results {
				// id 取出这个结果对应的视频 ID。
				id := missedL1[i]
				// 该键有值。
				if res != nil {
					// MGET 返回的是字符串；str 是断言后的串、ok 类型是否正确。
					if str, ok := res.(string); ok {
						// v 准备接收反序列化后的视频。
						var v video.Video
						// json.Unmarshal 把 JSON 串解析进 v；解析成功才用。
						if err := json.Unmarshal([]byte(str), &v); err == nil {
							// 放进结果 map。
							videoMap[id] = &v
							// 回写更新 L1 本地缓存
							if f.localcache != nil {
								// 用 Redis 的值回填 L1，5 秒过期。
								f.localcache.Set(cacheKeys[i], v, 5*time.Second)
							}
							// 该 ID 处理完，继续下一个。
							continue
						}
					}
				}
				// 走到这里：Redis 没这个键 / 值格式不对，记为 L2 未命中。
				missedL2 = append(missedL2, id)
			}
		} else {
			// 如果 Redis 挂了或者超时了，全部降级到 L3
			missedL2 = missedL1
			// 打日志方便排查。
			log.Printf("L2 Redis MGet 失败，全部降级到 MySQL: %v", err)
		}
	}

	// L2 也全部命中：按序返回。
	if len(missedL2) == 0 {
		// 组装有序结果。
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	//L3:MySQL
	// wg 等待组：等所有并发查库的 goroutine 结束。
	var wg sync.WaitGroup
	// mu 互斥锁：多个 goroutine 同时写 videoMap，必须加锁。
	var mu sync.Mutex
	// 逐个 L2 未命中的 ID 开 goroutine 并发查库。
	for _, id := range missedL2 {
		// 计数 +1，表示有一个任务。
		wg.Add(1)
		// go 开启 goroutine（类似 Python 的 asyncio.create_task，但由系统线程调度）；
		// videoID 通过参数传进去（避免闭包直接捕获循环变量 id 的坑）。
		go func(videoID uint) {
			// defer 保证函数退出时 wg.Done()（计数 -1）。
			defer wg.Done()
			// sfKey singleflight 的合并键：同一视频的并发查库只放行一个。
			sfKey := f.rediscache.Key("sf:entity:%d", videoID)

			// requestGroup.Do：相同 sfKey 的并发调用，只有一个真正执行闭包，其余共享结果。
			// v 返回值（interface{}），err 错误，第三个返回值 shared（是否被共享，用 _ 忽略）。
			v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
				// videoList 查库（只查这一个 ID）；err 错误。
				videoList, err := f.repo.GetByIDs(ctx, []uint{videoID})

				// 查询出错或这个视频不存在。
				if err != nil || len(videoList) == 0 {
					// 返回 nil 和原错误。
					return nil, err
				}

				// safeCopy 拷贝一份视频值，避免后续缓存操作动到共享对象。
				safeCopy := *videoList[0]
				// cachekey 拼该视频缓存键。
				cachekey := f.rediscache.Key("video:entity:%d", safeCopy.ID)
				// json.Marshal 把视频序列化成 JSON；b 是字节数组；成功才回写。
				if b, err := json.Marshal(safeCopy); err == nil {
					//异步回写redis
					// 再开一个 goroutine 回写，不阻塞当前返回；k、b 通过参数传入。
					go func(k string, b []byte) {
						// setCtx 用独立的 background 上下文 + 50ms 超时（请求可能已结束）。
						setCtx, setCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
						// 退出时取消。
						defer setCancel()

						// SetBytes 写 Redis，TTL 1 小时。
						f.rediscache.SetBytes(setCtx, k, b, time.Hour)
					}(cachekey, b)
				}
				// 返回查到的视频指针和错误（nil）。
				return videoList[0], err
			})

			// 查库成功且拿到了视频。
			if err == nil && v != nil {
				// 类型断言回 *video.Video，再拷贝一份值（safeCopy）。
				safeCopy := *(v.(*video.Video))
				// 加锁保护对 map 的写。
				mu.Lock()
				// 放进结果 map（用局部变量 id，即本次循环的视频 ID）。
				videoMap[id] = &safeCopy
				// 解锁。
				mu.Unlock()
				// 回填 L1 本地缓存，5 秒过期。
				f.localcache.Set(f.rediscache.Key("video:entity:%d", safeCopy.ID), safeCopy, 5*time.Second)
			}
		}(id)
	}
	// 阻塞等所有 goroutine 完成。
	wg.Wait()
	// 按入参顺序组装并返回（err 一律返回 nil：单个视频查不到只当作缺失）。
	return buildOrderedResult(videoIDs, videoMap), nil
}

// 查询最新视频 (冷热分离 + 游标分页)
// ListLatest 方法：刷"最新视频流"。热数据在 Redis ZSET，冷数据在 MySQL。
//
// 参数：limit 条数；latestBefore 时间游标（零值=第一页）；viewerAccountID 观看者 ID；
// 返回值：ListLatestResponse（视频列表 + 下次游标 + 是否还有更多）、错误。
func (f *FeedService) ListLatest(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	// Redis 不可用：直接全部查库（降级）。
	if f.rediscache == nil {
		// 走纯 DB 版本。
		return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
	}

	// 获取 ZSET 中最老的一条数据
	// ZRange 取全局时间线 ZSET 中下标 [0,0] 的元素（即分数最小、最老的那一条）；
	// zsetTail 是结果、err 错误。这条的分数就是"冷热分界线"。
	zsetTail, err := f.rediscache.ZRangeWithScores(ctx, f.rediscache.Key("feed:global_timeline"), 0, 0)

	// 取分界线失败：降级查库。
	if err != nil {
		// 纯 DB。
		return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
	}

	// isZsetEmpty 判断 ZSET 是不是空的（没有最老元素）。
	isZsetEmpty := len(zsetTail) == 0

	// ZSET 为空：需要重建。
	if isZsetEmpty {
		//全局静态锁：无视所有用户的不同时间戳游标
		// 固定 key：所有用户、所有游标都抢这同一把锁，只让一个人去重建。
		sfKey := f.rediscache.Key("sf:fallback:global_timeline_rebuild")

		// singleflight 执行重建闭包；v 结果标记、err 错误。
		v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
			// 无视游标，直接去 MySQL 捞最新的 1000 条
			// dbVideos 最新 1000 条视频；err 错误。
			dbVideos, err := f.repo.ListLatest(ctx, 1000, time.Time{})
			if err != nil {
				// 查库失败：返回。
				return nil, err
			}
			// 库里一条视频都没有。
			if len(dbVideos) == 0 {
				// 返回特殊标记，避免下面递归调用时又回来重建导致死循环。
				return "EMPTY_DB", nil // 防无限递归
			}

			// 重建 ZSET
			// bgCtx 独立上下文 + 2s 超时（重建是后台动作）。
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			// 退出时取消。
			defer cancel()
			// zElements 收集 ZSET 元素：分数=发布时间毫秒值，成员=视频 ID 字符串。
			var zElements []redis.Z
			// 遍历 1000 条视频；vid 当前视频。
			for _, vid := range dbVideos {
				// 追加一个 ZSET 元素。
				zElements = append(zElements, redis.Z{
					// Score 用发布时间的毫秒时间戳（这样按分数排序=按时间排序）。
					Score: float64(vid.CreateTime.UnixMilli()),
					// Member 必须是字符串，用视频 ID。
					Member: fmt.Sprintf("%d", vid.ID),
				})
			}
			// ZAdd 一次性把 1000 个元素灌进 ZSET。
			f.rediscache.ZAdd(bgCtx, f.rediscache.Key("feed:global_timeline"), zElements...)
			// 返回成功标记。
			return "SUCCESS", nil
		})

		// 重建出错：返回。
		if err != nil {
			// 空响应 + 错误。
			return ListLatestResponse{}, err
		}
		// 库里本来就没视频。
		if v == "EMPTY_DB" {
			// 返回空列表、HasMore=false。
			return ListLatestResponse{HasMore: false}, nil
		}

		// 让所有被阻塞的请求重新查一遍
		// 递归调用自己一次：这次 ZSET 已有数据，走正常流程。
		return f.ListLatest(ctx, limit, latestBefore, viewerAccountID)
	}

	// watermark 冷热分界线：ZSET 里最老元素的分数（毫秒时间戳）。
	watermark := int64(zsetTail[0].Score)
	// reqTime 本次请求的"时间位置"，默认当前时刻。
	reqTime := time.Now().UnixMilli()
	// 客户端带了游标：以游标时间为准。
	if !latestBefore.IsZero() {
		// reqTime 换成游标毫秒值。
		reqTime = latestBefore.UnixMilli()
	}

	// baseVideos 本页要返回的视频。
	var baseVideos []*video.Video

	// 请求位置已经老于等于分界线：要看的是冷数据。
	if reqTime <= watermark {
		//冷数据降级查库

		// 针对个别用户的防并发（此时可以用时间戳做锁，因为冷尾流量极小）
		// sfKey 按"条数+游标时间"区分，同一页的并发只查一次库。
		sfKey := f.rediscache.Key("sf:cold:listLatest:%d:%d", limit, reqTime)
		// singleflight 查库；v 结果、err 错误。
		v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
			// 直接查 MySQL。
			return f.repo.ListLatest(ctx, limit, latestBefore)
		})
		// 查库失败：返回。
		if err != nil {
			// 空响应 + 错误。
			return ListLatestResponse{}, err
		}
		// 类型断言成视频切片。
		baseVideos = v.([]*video.Video)
		// 不回写 ZSET，防止冷数据污染热点时间线

	} else {
		// 热数据直接查redis
		// maxScore ZSET 分数上界，默认 "+inf"（正无穷，即从最新一条开始）。
		maxScore := "+inf"
		// 带了游标。
		if !latestBefore.IsZero() {
			// 上界设成"游标时间 -1 毫秒"，保证游标那条不会重复出现。
			maxScore = fmt.Sprintf("%d", reqTime-1) // 防重复
		}

		// ZRevRangeByScore 在分数 [-inf, maxScore] 区间内按分数【从大到小】取 limit 个成员；
		// videoIDsStr 是视频 ID 字符串列表、err 错误。
		videoIDsStr, err := f.rediscache.ZRevRangeByScore(ctx, f.rediscache.Key("feed:global_timeline"), maxScore, "-inf", 0, int64(limit))
		if err != nil {
			// 出错返回。
			return ListLatestResponse{}, err
		}

		// videoIDs 把字符串 ID 转成 uint。
		var videoIDs []uint
		// 遍历字符串 ID；idStr 当前串。
		for _, idStr := range videoIDsStr {
			// ParseUint 转数字；失败的跳过（err == nil 才要）。
			if id, err := strconv.ParseUint(idStr, 10, 64); err == nil {
				// 转成 uint 加进切片。
				videoIDs = append(videoIDs, uint(id))
			}
		}

		// 取到了 ID。
		if len(videoIDs) > 0 {
			// GetVideoByIDs 走三级缓存拿视频详情；err 错误。
			baseVideos, err = f.GetVideoByIDs(ctx, videoIDs)
			if err != nil {
				// 失败返回。
				return ListLatestResponse{}, err
			}
		}

		// 刚好击穿了冷热边界
		// Redis 热数据不够一页（说明本页一部分在热、一部分在冷）。
		if len(baseVideos) < limit {
			// remainLimit 计算还差几个。
			remainLimit := limit - len(baseVideos)

			// coldCursor 查冷数据用的时间游标。
			var coldCursor time.Time
			// 热数据有取到：用本页热数据最后一条的时间接着查。
			if len(baseVideos) > 0 {
				// 最后一条的 CreateTime。
				coldCursor = baseVideos[len(baseVideos)-1].CreateTime
			} else {
				// 热数据一条没有：直接用客户端游标。
				coldCursor = latestBefore
			}

			// sfKey 拼接冷数据的合并键。
			sfKey := f.rediscache.Key("sf:stitch:listLatest:%d:%d", remainLimit, coldCursor.UnixMilli())
			// singleflight 查冷数据补齐；v 结果、err 错误。
			v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
				// 查 MySQL 补剩余条数。
				return f.repo.ListLatest(ctx, remainLimit, coldCursor)
			})

			// 补齐查询成功。
			if err == nil {
				// coldVideos 断言成视频切片。
				coldVideos := v.([]*video.Video)
				// 用 ... 把冷数据展开追加到热数据后面（append 切片拼接）。
				baseVideos = append(baseVideos, coldVideos...)
			}
		}
	}

	// nextTime 下一页游标。
	var nextTime int64
	// 本页有数据。
	if len(baseVideos) > 0 {
		// 将本页最后一条视频的时间作为下一次请求的游标
		nextTime = baseVideos[len(baseVideos)-1].CreateTime.UnixMilli()
	}
	// hasMore 恰好取满 limit 条，认为后面可能还有。
	hasMore := len(baseVideos) == limit

	// feedVideos 给每条视频补上"当前用户是否点赞"等展示字段；err 错误。
	feedVideos, err := f.buildFeedVideos(ctx, baseVideos, viewerAccountID)
	if err != nil {
		// 失败返回。
		return ListLatestResponse{}, err
	}

	// 组装响应返回。
	return ListLatestResponse{
		// VideoList 展示用视频列表。
		VideoList: feedVideos,
		// NextTime 下一页游标。
		NextTime: nextTime,
		// HasMore 是否还有更多。
		HasMore: hasMore,
	}, nil
}

// listLatestFromDB 方法：Redis 不可用时的纯 DB 版最新流，逻辑和正常版的输出保持一致。
//
// 参数同 ListLatest；返回值 ListLatestResponse、错误。
func (f *FeedService) listLatestFromDB(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	// videos 直接查库；err 错误。
	videos, err := f.repo.ListLatest(ctx, limit, latestBefore)
	if err != nil {
		// 失败返回。
		return ListLatestResponse{}, err
	}
	// feedVideos 补展示字段；err 错误。
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		// 失败返回。
		return ListLatestResponse{}, err
	}
	// nextTime 下一页游标，默认 0。
	var nextTime int64
	// 有数据。
	if len(videos) > 0 {
		// 最后一条的发布时间（毫秒）。
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}
	// 组装返回。
	return ListLatestResponse{
		// 视频列表。
		VideoList: feedVideos,
		// 游标。
		NextTime: nextTime,
		// 是否满页。
		HasMore: len(videos) == limit,
	}, nil
}

// 按照点赞数查询视频
// ListLikesCount 方法：点赞榜，纯 DB 游标分页。
//
// 参数：limit 条数；cursor 复合游标（nil=第一页）；viewerAccountID 观看者；
// 返回值 ListLikesCountResponse、错误。
func (f *FeedService) ListLikesCount(ctx context.Context, limit int, cursor *LikesCountCursor, viewerAccountID uint) (ListLikesCountResponse, error) {
	// videos 查点赞榜；err 错误。
	videos, err := f.repo.ListLikesCountWithCursor(ctx, limit, cursor)
	if err != nil {
		// 失败返回。
		return ListLikesCountResponse{}, err
	}
	// hasMore 满页即可能还有。
	hasMore := len(videos) == limit
	// feedVideos 补展示字段；err 错误。
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		// 失败返回。
		return ListLikesCountResponse{}, err
	}
	// resp 先组装基础响应。
	resp := ListLikesCountResponse{
		// 视频列表。
		VideoList: feedVideos,
		// 是否还有。
		HasMore: hasMore,
	}
	// 本页有数据：生成下一页游标。
	if len(videos) > 0 {
		// last 本页最后一条。
		last := videos[len(videos)-1]
		// nextLikesCountBefore 游标点赞数。
		nextLikesCountBefore := last.LikesCount
		// nextIDBefore 游标视频 ID。
		nextIDBefore := last.ID
		// 取地址赋给响应（指针，nil 时表示没有下一页）。
		resp.NextLikesCountBefore = &nextLikesCountBefore
		resp.NextIDBefore = &nextIDBefore
	}
	// 返回。
	return resp, nil
}

// 按照关注列表查询视频
// ListByFollowing 方法：关注流，Cache-Aside + 分布式锁防击穿。
//
// 参数：limit 条数；latestBefore 时间游标；viewerAccountID 观看者；
// 返回值 ListByFollowingResponse、错误。
func (f *FeedService) ListByFollowing(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListByFollowingResponse, error) {
	// doListByFollowingFromDB 闭包：真正查库并组装响应的逻辑，多处复用。
	doListByFollowingFromDB := func() (ListByFollowingResponse, error) {
		// videos 查关注流；err 错误。
		videos, err := f.repo.ListByFollowing(ctx, limit, viewerAccountID, latestBefore)
		if err != nil {
			// 失败返回空响应。
			return ListByFollowingResponse{}, err
		}
		// nextTime 下一页游标（这里用【秒】）。
		// 用最后一条的【秒】当下次游标
		var nextTime int64
		// 有数据。
		if len(videos) > 0 {
			// 最后一条发布时间的秒值。
			nextTime = videos[len(videos)-1].CreateTime.Unix()
		} else {
			// 没数据给 0。
			nextTime = 0
		}
		// hasMore 满页判定。
		hasMore := len(videos) == limit
		// feedVideos 补展示字段；err 错误。
		feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID) // 补 is_liked 等
		if err != nil {
			// 失败返回。
			return ListByFollowingResponse{}, err
		}
		// resp 组装。
		resp := ListByFollowingResponse{
			// 视频列表。
			VideoList: feedVideos,
			// 游标。
			NextTime: nextTime,
			// 是否还有。
			HasMore: hasMore,
		}
		// 返回。
		return resp, nil
	}
	// cacheKey 关注流缓存键，下面赋值。
	var cacheKey string
	// 登录用户且 Redis 可用才走缓存。
	if viewerAccountID != 0 && f.rediscache != nil {
		// before 游标秒值，默认 0（第一页）。
		before := int64(0)
		// 带了游标。
		if !latestBefore.IsZero() {
			// 转秒。
			before = latestBefore.Unix()
		}
		// cacheKey 把条数、用户、游标都拼进键里：不同人/不同页互不影响。
		cacheKey = f.rediscache.Key("feed:listByFollowing:limit=%d:accountID=%d:before=%d", limit, viewerAccountID, before)
		// cacheCtx 50ms 超时；cancel 释放。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// 注意：这里 defer 到整个 ListByFollowing 返回时才 cancel。
		defer cancel()

		// GetBytes 读缓存；b 是字节、err 错误。
		b, err := f.rediscache.GetBytes(cacheCtx, cacheKey)
		// 命中且读取成功。
		if err == nil {
			// cached 准备接收。
			var cached ListByFollowingResponse
			// 反序列化成功。
			if err := json.Unmarshal(b, &cached); err == nil {
				// 直接返回缓存。
				return cached, nil
			}
		} else if rediscache.IsMiss(err) { // 缓存未命中
			// lockKey 锁键，在缓存键前加 lock: 前缀。
			lockKey := "lock:" + cacheKey
			// 缓存未命中，尝试加锁
			// Lock 用 SetNX 抢锁 500ms；token 是解锁凭证、locked 是否抢到、_ 忽略错误。
			token, locked, _ := f.rediscache.Lock(cacheCtx, lockKey, 500*time.Millisecond)
			// 抢到锁的人负责查库回填。
			if locked {
				// defer 函数返回时释放锁（用 background 上下文、带 token 防误删别人的锁）。
				defer func() { _ = f.rediscache.Unlock(context.Background(), lockKey, token) }()
				// double-check：拿锁后再读一次缓存，可能前面的人已经回填了。
				if b, err := f.rediscache.GetBytes(cacheCtx, cacheKey); err == nil {
					// cached 接收。
					var cached ListByFollowingResponse
					// 反序列化成功就直接返回。
					if err := json.Unmarshal(b, &cached); err == nil {
						// 返回缓存。
						return cached, nil
					}
				} else { // 缓存仍未命中，从数据库中查询
					// resp 查库；err 错误。
					resp, err := doListByFollowingFromDB()
					if err != nil {
						// 失败返回。
						return ListByFollowingResponse{}, err
					}
					// 序列化响应；成功才写缓存。
					if b, err := json.Marshal(resp); err == nil {
						// SetBytes 回填，TTL 24 小时（错误忽略）。
						_ = f.rediscache.SetBytes(cacheCtx, cacheKey, b, f.cacheTTL)
					}
					// 返回查库结果。
					return resp, nil
				}
			} else {
				// 没抢到锁（别人正在回填）：自旋最多等 5 次。
				for i := 0; i < 5; i++ {
					// 每次睡 20ms，给持锁者回填的时间。
					time.Sleep(20 * time.Millisecond)
					// 再读缓存。
					if b, err := f.rediscache.GetBytes(cacheCtx, cacheKey); err == nil {
						// cached 接收。
						var cached ListByFollowingResponse
						// 反序列化成功就返回别人回填的结果。
						if err := json.Unmarshal(b, &cached); err == nil {
							// 返回。
							return cached, nil
						}
					}
				}
			}
		}
	}

	// 兜底：没抢到锁且等待超时、或没开缓存——直接查库；resp 结果、err 错误。
	resp, err := doListByFollowingFromDB()
	if err != nil {
		// 失败返回。
		return ListByFollowingResponse{}, err
	}
	// cacheKey 非空（说明走过缓存流程）：顺手回填一次。
	if cacheKey != "" {
		// 序列化；成功才写。
		if b, err := json.Marshal(resp); err == nil {
			// cacheCtx 50ms 超时。
			cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			// 退出取消。
			defer cancel()
			// 回填缓存，错误忽略。
			_ = f.rediscache.SetBytes(cacheCtx, cacheKey, b, f.cacheTTL)
		}
	}
	// 返回。
	return resp, nil
}

// ListByPopularity 方法：热度榜。优先 Redis（60 个分钟桶合并），失败再走 MySQL 复合游标。
//
// 参数较多：limit 条数；reqAsOf 客户端指定的快照时刻（秒）；offset 快照内偏移；
// viewerAccountID 观看者；latestPopularity/latestBefore/latestIDBefore 是 MySQL 兜底用的复合游标；
// 返回值 ListByPopularityResponse、错误。
func (f *FeedService) ListByPopularity(ctx context.Context, limit int, reqAsOf int64, offset int, viewerAccountID uint, latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error) {
	// Redis 热榜（稳定分页：as_of + offset）
	// Redis 可用才走强逻辑。
	if f.rediscache != nil {
		// asOf 榜单快照时刻，默认当前 UTC 时间并截断到【分钟】（同一分钟内大家共享一张榜）。
		asOf := time.Now().UTC().Truncate(time.Minute)
		// 客户端带了快照时刻：用它（保证翻页时始终是同一张榜）。
		if reqAsOf > 0 {
			// 转时间并截断到分钟。
			asOf = time.Unix(reqAsOf, 0).UTC().Truncate(time.Minute)
		}

		// win 要合并的分钟桶数量：60 个（最近一小时）。
		const win = 60
		// keys 收集 60 个分钟桶的 key，预分配容量。
		keys := make([]string, 0, win)
		// i 从 0（当前分钟）往前数到 59 分钟前。
		for i := 0; i < win; i++ {
			// 每个桶 key 形如 hot:video:1m:202609271430；Format 用 Go 时间模板 200601021504。
			keys = append(keys, f.rediscache.Key("hot:video:1m:%s", asOf.Add(-time.Duration(i)*time.Minute).Format("200601021504")))
		}

		// dest 合并结果的目标 key（快照 key）：同一个 as_of 页内复用。
		//	等下把 60 个桶加总后的结果,存到哪个 key
		dest := f.rediscache.Key("hot:video:merge:1m:%s", asOf.Format("200601021504"))
		// opCtx 80ms 超时（要合并 60 个 key，比单操作略宽）；cancel 释放。
		//	context.Context(上下文):用来在一次调用链里传递"超时、取消信号"。
		//	把 opCtx 传给 Redis 命令,就等于告诉它:"这个操作最多只准跑 80 毫秒,超时就自动中止,别一直挂着。"
		opCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		// 退出取消。
		defer cancel()

		// Exists 看合并结果是否已经存在；exists 存在数、_ 忽略错误。
		exists, _ := f.rediscache.Exists(opCtx, dest)
		// 还没合并过。
		if !exists {
			// ZUnionStore 把 60 个分钟桶按 SUM 求和到 dest（某视频最近 60 分钟的总热度分）。
			_ = f.rediscache.ZUnionStore(opCtx, dest, keys, "SUM")
			// Expire 给合并结果 2 分钟过期——给用户翻页留时间。
			_ = f.rediscache.Expire(opCtx, dest, 2*time.Minute) // 给翻页留时间
		}

		// start 本页起始偏移。
		start := int64(offset)
		// stop 本页结束下标（含），所以 -1。
		stop := start + int64(limit) - 1
		// ZRevRange 取合并榜 [start, stop] 区间（分数从高到低）；members 成员、err 错误。
		members, err := f.rediscache.ZRevRange(opCtx, dest, start, stop)
		// 查询成功但没成员。
		if err == nil && len(members) == 0 {
			// offset>0：说明已经翻过整个榜了，正常返回空 + HasMore=false。
			if offset > 0 {
				// 提前返回"翻到底了"。
				return ListByPopularityResponse{
					// 空列表。
					VideoList: []FeedVideoItem{},
					// 回传快照时刻。
					AsOf: asOf.Unix(),
					// 偏移保持不变。
					NextOffset: offset,
					// 没有更多。
					HasMore: false,
				}, nil
			}
		}
		// 查询成功且这一页有成员：正常出榜。
		if err == nil && len(members) > 0 {
			// ids 把成员（视频 ID 字符串）转成数字，预分配容量。
			ids := make([]uint, 0, len(members))
			// 遍历成员；m 当前 ID 字符串。
			for _, m := range members {
				// ParseUint 转换。
				u, err := strconv.ParseUint(m, 10, 64)
				// 转换成功且是有效 ID。
				if err == nil && u > 0 {
					// 加进 ids。
					ids = append(ids, uint(u))
				}
			}

			// videos 批量查视频详情；err 错误。
			videos, err := f.repo.GetByIDs(ctx, ids)
			// 查库成功才用 Redis 榜的结果。
			if err == nil {
				// byID 建"ID → 视频"映射，方便按榜单顺序重排。
				byID := make(map[uint]*video.Video, len(videos))
				// 遍历查到的视频。
				for _, v := range videos {
					// 录入映射。
					byID[v.ID] = v
				}
				// ordered 按榜单 ids 顺序重排（GetByIDs 的 IN 不保证顺序）。
				ordered := make([]*video.Video, 0, len(ids))
				// 按榜单顺序取。
				for _, id := range ids {
					// 该 ID 查到了视频。
					if v := byID[id]; v != nil {
						// 按序追加。
						ordered = append(ordered, v)
					}
				}
				// items 补展示字段；err 错误。
				items, err := f.buildFeedVideos(ctx, ordered, viewerAccountID)
				if err != nil {
					// 失败返回。
					return ListByPopularityResponse{}, err
				}
				// resp 组装热榜响应。
				resp := ListByPopularityResponse{
					// 视频列表。
					VideoList: items,
					// 回传快照时刻，客户端翻页要原样带回。
					AsOf: asOf.Unix(),
					// 下一页偏移 = 当前偏移 + 本页条数。
					NextOffset: offset + len(items),
					// 满页才可能有下一页。
					HasMore: len(items) == limit,
				}
				// 有数据：同时生成 MySQL 风格的复合游标一并返回。
				if len(ordered) > 0 {
					// last 本页最后一条。
					last := ordered[len(ordered)-1]
					// nextPopularity 游标热度。
					nextPopularity := last.Popularity
					// nextBefore 游标发布时间。
					nextBefore := last.CreateTime
					// nextID 游标视频 ID。
					nextID := last.ID
					// 取地址赋给响应。
					resp.NextLatestPopularity = &nextPopularity
					resp.NextLatestBefore = &nextBefore
					resp.NextLatestIDBefore = &nextID
				}
				// 返回热榜结果。
				return resp, nil
			}
		}
	}

	// videos MySQL 兜底：用复合游标查热度榜；err 错误。
	videos, err := f.repo.ListByPopularity(ctx, limit, latestPopularity, latestBefore, latestIDBefore)
	if err != nil {
		// 失败返回。
		return ListByPopularityResponse{}, err
	}
	// items 补展示字段；err 错误。
	items, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		// 失败返回。
		return ListByPopularityResponse{}, err
	}
	// resp 组装 DB 兜底响应（AsOf/Offset 给 0，表示这不是快照分页）。
	resp := ListByPopularityResponse{
		// 列表。
		VideoList: items,
		// 无快照时刻。
		AsOf: 0,
		// 无偏移。
		NextOffset: 0,
		// 满页判定。
		HasMore: len(items) == limit,
	}
	// 有数据：生成下一页复合游标。
	if len(videos) > 0 {
		// last 最后一条。
		last := videos[len(videos)-1]
		// nextPopularity 游标热度。
		nextPopularity := last.Popularity
		// nextBefore 游标时间。
		nextBefore := last.CreateTime
		// nextID 游标 ID。
		nextID := last.ID
		// 取地址。
		resp.NextLatestPopularity = &nextPopularity
		resp.NextLatestBefore = &nextBefore
		resp.NextLatestIDBefore = &nextID
	}
	// 返回。
	return resp, nil
}

// buildFeedVideos 方法：把数据库视频模型转成给前端的展示结构，并批量补上"我是否点赞过"。
//
// 参数：videos 视频列表；viewerAccountID 观看者；
// 返回值：[]FeedVideoItem 展示项、错误。
func (f *FeedService) buildFeedVideos(ctx context.Context, videos []*video.Video, viewerAccountID uint) ([]FeedVideoItem, error) {
	// feedVideos 结果切片，预分配容量。
	feedVideos := make([]FeedVideoItem, 0, len(videos))
	// videoIDs 收集所有视频 ID，长度等于视频数，用于批量查点赞。
	videoIDs := make([]uint, len(videos))
	// 遍历视频；i 下标、v 当前视频。
	for i, v := range videos {
		// 对应位置放视频 ID。
		videoIDs[i] = v.ID
	}
	// likedMap 一次批量查出"当前用户对这些视频分别有没有点赞"；err 错误。
	// 批量查而不是循环查，避免 N+1 次数据库往返。
	likedMap, err := f.likeRepo.BatchGetLiked(ctx, videoIDs, viewerAccountID)
	if err != nil {
		// 失败返回 nil。
		return nil, err
	}
	// 再次遍历视频逐个组装展示项；video 当前视频。
	for _, video := range videos {
		// append 一个展示项。
		feedVideos = append(feedVideos, FeedVideoItem{
			// ID 视频编号。
			ID: video.ID,
			// Author 作者信息（编号 + 用户名）。
			Author: FeedAuthor{ID: video.AuthorID, Username: video.Username},
			// Title 标题。
			Title: video.Title,
			// Description 简介。
			Description: video.Description,
			// PlayURL 播放地址。
			PlayURL: video.PlayURL,
			// CoverURL 封面地址。
			CoverURL: video.CoverURL,
			// CreateTime 发布时间转秒。
			CreateTime: video.CreateTime.Unix(),
			// LikesCount 点赞数。
			LikesCount: video.LikesCount,
			// IsLiked 当前观看者是否点过赞：从 map 取，没有这个 key 时返回零值 false。
			IsLiked: likedMap[video.ID],
		})
	}
	// 返回展示列表。
	return feedVideos, nil
}

// buildOrderedResult 是普通函数（没有接收者）：按给定 ID 顺序从 map 里取出视频。
//
// 参数：orderedIDs 期望的顺序；dataMap ID → 视频 的映射；   用orderedIDs去dataMap取视频，按orderedIDs顺序返回视频切片，跳过没查到的 ID。
// 返回值：排好序的视频指针切片。
func buildOrderedResult(orderedIDs []uint, dataMap map[uint]*video.Video) []*video.Video {
	// res 结果切片，预分配容量。
	res := make([]*video.Video, 0, len(orderedIDs))
	// 按期望顺序遍历；id 当前编号。
	for _, id := range orderedIDs {
		// 从 map 取；v 视频、exits 是否存在（注意原代码拼写就是 exits）。
		if v, exits := dataMap[id]; exits && v != nil {
			// 存在且非空才追加，从而保持顺序且跳过没查到的 ID。
			res = append(res, v)
		}
	}
	// 返回有序结果。
	return res
}

// ListByTag 方法：按标签查视频的服务入口。
//
// 参数：tagName 标签名；limit 条数；viewerAccountID 观看者；
// 返回值：展示项列表、错误。
func (f *FeedService) ListByTag(ctx context.Context, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error) {
	// videos 调仓储按标签查；err 错误。
	videos, err := f.repo.ListByTag(ctx, tagName, limit)
	if err != nil {
		// 失败返回 nil。
		return nil, err
	}
	// 补展示字段后返回。
	return f.buildFeedVideos(ctx, videos, viewerAccountID)
}
