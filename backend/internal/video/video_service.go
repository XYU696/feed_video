// package video：视频业务包。本文件是视频服务层，
// 负责发布视频（事务 + Outbox + 标签）、删除（鉴权）、以及最有技术含量的
// 视频详情读取：Redis 缓存 + 分布式锁防击穿 + 未获锁等待回填。
package video

import (
	// context：超时、取消。
	"context"
	// encoding/json：缓存值 JSON 序列化/反序列化。
	"encoding/json"
	// errors：返回普通错误。
	"errors"
	// strconv：视频编号转字符串。
	"strconv"
	// strings：去空白。
	"strings"
	// time：缓存 TTL、时间格式化。
	"time"

	// apierror：用它的 ErrUnauthorized 表示"不是你的视频不能删"。
	"feedsystem_video_go/internal/apierror"
	// rabbitmq：热度 MQ 类型。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端、IsMiss。
	rediscache "feedsystem_video_go/internal/middleware/redis"

	// gorm：事务、视频模型。
	"gorm.io/gorm"
)

// VideoService 结构体是视频服务层。
type VideoService struct {
	// repo 视频仓储，数据库操作走它。
	repo *VideoRepository
	// cache Redis 客户端，可能为 nil。
	cache *rediscache.Client
	// cacheTTL 详情缓存的有效期（构造时设为 5 分钟）。
	cacheTTL time.Duration
	// popularityMQ 热度事件发布器，可能为 nil。
	popularityMQ *rabbitmq.PopularityMQ
}

// NewVideoService 是构造函数：创建视频服务。
//
// 参数：repo 视频仓储、cache Redis 客户端、popularityMQ 热度 MQ；
// 返回值 *VideoService：服务。
func NewVideoService(repo *VideoRepository, cache *rediscache.Client, popularityMQ *rabbitmq.PopularityMQ) *VideoService {
	// 注入依赖，并把详情缓存 TTL 固定设为 5 分钟，返回指针。
	return &VideoService{repo: repo, cache: cache, cacheTTL: 5 * time.Minute, popularityMQ: popularityMQ}
}

// Publish 方法：发布视频的核心业务逻辑。
//
// 在【一个数据库事务】里完成四件事，保证它们要么全成功要么全回滚：
//  1. 写入 videos 表；
//  2. 写入 outbox_msgs 发件箱（之后异步通知时间线）；
//  3. 解析标题/简介里的 #标签，不存在就创建；
//  4. 写入 video_tags 视频-标签关联。
//
// 参数 video：要发布的视频指针；
// 返回值 error：参数/事务错误。
func (vs *VideoService) Publish(ctx context.Context, video *Video) error {
	// 防空指针。
	if video == nil {
		// 返回错误。
		return errors.New("video is nil")
	}
	// 下面三行对用户输入做清洗：TrimSpace 去掉标题、播放地址、封面地址首尾空白。
	video.Title = strings.TrimSpace(video.Title)
	// 清洗播放地址。
	video.PlayURL = strings.TrimSpace(video.PlayURL)
	// 清洗封面地址。
	video.CoverURL = strings.TrimSpace(video.CoverURL)

	// 必填校验：标题空。
	if video.Title == "" {
		// 返回错误。
		return errors.New("title is required")
	}
	// 播放地址空。
	if video.PlayURL == "" {
		// 返回错误。
		return errors.New("play url is required")
	}
	// 封面地址空。
	if video.CoverURL == "" {
		// 返回错误。
		return errors.New("cover url is required")
	}

	//事务保证视频写入库和消息写入本地消息表的一致性
	// err 接收事务最终结果。vs.repo.db：直接用仓储内部的连接开启事务（本服务同包，可访问小写字段）。
	err := vs.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// tx.Create(video)：第 1 步，插入视频（插入后 video.ID 被自增主键回填，后面关联表要用）；err 接收错误。
		if err := tx.Create(video).Error; err != nil {
			// 失败：回滚。
			return err
		}

		// msg 组装发件箱消息。
		msg := OutboxMsg{
			// VideoID 关联刚插入的视频 ID。
			VideoID: video.ID,
			// EventType 事件类型，轮询器/消费者靠它识别。
			EventType: "video_published",
			// Status 初始 pending（待发送）。   status是pending是关键，因为轮询器/消费者会轮询这个状态
			Status: "pending",
			// CreateTime 用视频的创建时间（保证时间线分数就是发布时间）。
			CreateTime: video.CreateTime,
		}

		// 第 2 步：插入发件箱记录；err 接收错误。
		// 和视频在【同一事务】插入 outbox_msgs 表
		if err := tx.Create(&msg).Error; err != nil {
			// 失败：回滚（视频插入也撤销）。
			return err
		}

		// tags 第 3 步：ExtractTags 从"标题 + 空格 + 简介"里提取所有 #标签，返回标签名字切片。
		tags := ExtractTags(video.Title + " " + video.Description)
		// for range 遍历每个标签名 tagName。
		for _, tagName := range tags {
			// tag 准备接收查询/创建结果。
			var tag Tag
			// .Where("name = ?", tagName).FirstOrCreate(&tag, Tag{Name: tagName})：
			// 先按名字查，查不到就创建（多个视频打同一标签时复用同一条 tags 记录）；err 接收错误。
			if err := tx.Where("name = ?", tagName).FirstOrCreate(&tag, Tag{Name: tagName}).Error; err != nil {
				// 失败：回滚。
				return err
			}
			// 第 4 步：创建 video_tags 关联记录（视频ID、标签ID），维护多对多关系；err 接收错误。
			if err := tx.Create(&VideoTag{VideoID: video.ID, TagID: tag.ID}).Error; err != nil {
				// 失败：回滚。
				return err
			}
		}
		// 全部成功：提交事务。
		return nil
	})
	// 返回事务结果（nil 或错误）。
	return err

}

// Delete 方法：删除视频的业务逻辑——先查、确认存在、确认是本人的视频，才允许删，并清详情缓存。
//
// 参数：id 视频编号、authorID 当前登录用户编号；
// 返回值 error：错误（不是作者会返回 apierror.ErrUnauthorized）。
func (vs *VideoService) Delete(ctx context.Context, id uint, authorID uint) error {
	// video 先按 ID 查出视频；err 接收错误。
	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		// 查询出错：返回。
		return err
	}
	// 视频为 nil（查不到）。
	if video == nil {
		// 返回不存在错误。
		return errors.New("video not found")
	}
	// video.AuthorID != authorID：视频作者不是当前登录用户。
	if video.AuthorID != authorID {
		// 返回"未授权"——水平越权防护：你只能删自己的视频。
		return apierror.ErrUnauthorized
	}
	// 调仓储真正删除；err 接收错误。
	if err := vs.repo.DeleteVideo(ctx, id); err != nil {
		// 删除失败：返回。
		return err
	}
	// 配了缓存就顺手删掉详情缓存，避免删了视频还能从缓存读到。
	if vs.cache != nil {
		// cacheKey 拼详情缓存键 video:detail:id={id}。
		cacheKey := vs.cache.Key("video:detail:id=%d", id)
		// _ = Del(...)：用 background 上下文删除（删除动作不依附当前请求），忽略错误。
		_ = vs.cache.Del(context.Background(), cacheKey)
	}
	// 成功：返回 nil。
	return nil
}

// ListByAuthorID 方法：查某作者视频的薄封装。
//
// 参数 authorID：作者编号；
// 返回值：[]Video 切片、error 错误。
func (vs *VideoService) ListByAuthorID(ctx context.Context, authorID uint) ([]Video, error) {
	// videos 调仓储查询（仓储要 int64，这里把 uint 转过去）；err 接收错误。
	videos, err := vs.repo.ListByAuthorID(ctx, int64(authorID))
	if err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回切片。
	return videos, nil
}

// GetDetail 方法：读取视频详情，是本项目缓存设计的代表作。
//
// 要解决的三个经典缓存问题：
//   - 缓存命中：直接返回，不碰数据库；
//   - 缓存穿透/击穿：缓存恰好失效时大量并发涌来，用分布式锁只放一个请求去查库回填，
//     其他请求短暂等待、读回填后的缓存，避免全部打向 MySQL；
//   - Redis 不可用：最终兜底直接查库，功能不挂。
//
// 参数 id：视频编号；
// 返回值：*Video 视频、error 错误。
func (vs *VideoService) GetDetail(ctx context.Context, id uint) (*Video, error) {
	// cacheKey 拼详情缓存键。
	cacheKey := vs.cache.Key("video:detail:id=%d", id)

	// getCached 是定义在方法内部的【闭包】：尝试读缓存并反序列化。
	// 返回值：*Video 缓存里的视频、bool 是否成功拿到可用缓存。
	getCached := func() (*Video, bool) {
		// opCtx 给单次读缓存 50ms 超时；cancel 取消函数。
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 释放。
		defer cancel()

		// b 接收缓存原始字节；err 接收错误。GetBytes 读 cacheKey。
		b, err := vs.cache.GetBytes(opCtx, cacheKey)
		if err != nil {
			// 读不到/出错：返回未命中。
			return nil, false
		}
		// cached 准备接收反序列化结果。
		var cached Video
		// json.Unmarshal(b, &cached)：把 JSON 字节还原成 Video 结构体；err 接收错误。
		if err := json.Unmarshal(b, &cached); err != nil {
			// 缓存内容坏掉：当作未命中。
			return nil, false
		}
		// 返回缓存视频。
		return &cached, true
	}

	// setCached 是另一个内部闭包：把视频 JSON 序列化后写入缓存（TTL 用 cacheTTL）。
	// 参数 video：要缓存的视频；无返回值。
	setCached := func(video *Video) {
		// b 接收序列化字节；err 接收错误。
		b, err := json.Marshal(video)
		if err != nil {
			// 序列化失败（极少）：直接放弃写缓存。
			return
		}
		// opCtx 50ms 超时；cancel 取消函数。
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 释放。
		defer cancel()
		// _ = SetBytes(...)：写入缓存并设置 TTL，错误显式忽略（缓存失败不能影响读请求）。
		_ = vs.cache.SetBytes(opCtx, cacheKey, b, vs.cacheTTL)
	}

	// 配了缓存才走缓存分支。
	if vs.cache != nil {
		// 先尝试一次缓存；v 是视频、ok 表示命中。
		if v, ok := getCached(); ok {
			// 命中：直接返回，最快路径。
			return v, nil
		}

		// opCtx 再开一个 50ms 上下文用于下面的手动读取（这里手动管理 cancel）。
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// b 接收缓存字节、err 接收错误（再读一次，为了细分"未命中"和"其他错误"）。
		b, err := vs.cache.GetBytes(opCtx, cacheKey)
		// cancel()：立刻取消上下文释放资源（不等 50ms），下面不再需要它。
		cancel()
		// err == nil：读到了内容。
		if err == nil {
			// cached 准备反序列化。
			var cached Video
			// 反序列化成功就直接返回缓存（相当于双保险的命中处理）。
			if err := json.Unmarshal(b, &cached); err == nil {
				// 返回缓存视频。
				return &cached, nil
			}
		} else if rediscache.IsMiss(err) {
			// rediscache.IsMiss(err)：确认是"键不存在"（真正的缓存未命中），而不是 Redis 故障。
			// lockKey 拼防击穿用的锁键（在缓存键前加 lock:）。
			lockKey := "lock:" + cacheKey

			// lockCtx 50ms 上下文用于抢锁；lockCancel 取消函数。
			lockCtx, lockCancel := context.WithTimeout(ctx, 50*time.Millisecond)
			// token 是锁的唯一凭证、locked 是否抢成功、lockErr 抢锁错误。
			// Lock(...)：底层 SET key token NX EX 2秒，只有一个请求能 locked=true。
			token, locked, lockErr := vs.cache.Lock(lockCtx, lockKey, 2*time.Second)
			// 立刻释放锁上下文。
			lockCancel()

			// 没出错且抢到了锁：本请求负责查库 + 回填缓存。
			if lockErr == nil && locked {
				// defer 注册：函数返回前用 token 安全释放锁（Lua 校验只删自己的锁），避免死锁。
				defer func() { _ = vs.cache.Unlock(context.Background(), lockKey, token) }()

				// 双重检查：拿到锁后先再读一次缓存——可能在等锁期间别人已经回填了。
				if v, ok := getCached(); ok {
					// 已有缓存：直接返回，不必查库。
					return v, nil
				}

				// video 查数据库；err 接收错误。
				video, err := vs.repo.GetByID(ctx, id)
				if err != nil {
					// 查库失败：返回（锁由 defer 释放）。
					return nil, err
				}
				// setCached(video)：回填缓存供其他等待者读取。
				setCached(video)
				// 返回视频。
				return video, nil
			}

			// 没拿到锁：说明别的请求正在查库回填，本请求不查库，短暂等待缓存出现。
			// for i := 0; i < 5; i++：最多等 5 轮（每轮 20ms，共约 100ms）。
			for i := 0; i < 5; i++ {
				// select 是 Go 专用于"多路通道收发"的语法：
				select {
				// case <-ctx.Done()：上游请求被取消/超时。
				case <-ctx.Done():
					// 立即返回上下文错误，不再傻等。
					return nil, ctx.Err()
				// case <-time.After(20ms)：定时器 20ms 到点，该轮等待结束。
				case <-time.After(20 * time.Millisecond):
				}
				// 等待结束后再读一次缓存。
				if v, ok := getCached(); ok {
					// 别人已回填：返回。
					return v, nil
				}
			}
		}
	}

	// 兜底：没配缓存 / Redis 出错 / 等了 5 轮仍没回填——直接查数据库，保证功能可用。
	// video 查库；err 接收错误。
	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		// 出错返回。
		return nil, err
	}
	// 配了缓存就顺手回填（下一个请求就能命中）。
	if vs.cache != nil {
		// 写缓存。
		setCached(video)
	}
	// 返回视频。
	return video, nil
}

// UpdateLikesCount 方法：直接设置点赞数的薄封装。
//
// 参数：id 视频编号、likesCount 目标值；
// 返回值 error：错误。
func (vs *VideoService) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	// 调仓储更新；err 接收错误。
	if err := vs.repo.UpdateLikesCount(ctx, id, likesCount); err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// UpdatePopularity 方法：热度变化的处理，按"MQ → 缓存 → 库"的优先级做降级。
//
// 参数：id 视频编号、change 热度增量（点赞/评论产生）；
// 返回值 error：错误。
func (vs *VideoService) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	// 调仓储把热度写库（保证最终有持久化结果）；err 接收错误。
	if err := vs.repo.UpdatePopularity(ctx, id, change); err != nil {
		// 写库失败：直接返回错误。
		return err
	}

	// MQ 可用时，优先发事件让专门的 PopularityWorker 去算热度（异步削峰）。
	if vs.popularityMQ != nil {
		// 发布成功（err == nil）就结束，交给 worker。
		if err := vs.popularityMQ.Update(ctx, id, change); err == nil {
			// 返回 nil。
			return nil
		}
	}

	// MQ 没配或发布失败：降级为直接写 Redis，保证热度榜仍更新。
	if vs.cache != nil {
		// 1) 详情缓存：直接失效（最简单靠谱）
		// 删除视频详情缓存——热度变了，让下次读时重新查库拿到新值。
		_ = vs.cache.Del(context.Background(), vs.cache.Key("video:detail:id=%d", id))

		// 2) 热榜：写到“时间窗ZSET”，不要用 detail key
		// now：当前 UTC 时间并【截断到分钟】（Truncate 把秒数清零，同一分钟的事件落进同一个桶）。
		now := time.Now().UTC().Truncate(time.Minute)
		// windowKey 拼这一分钟的桶键 hot:video:1m:202609271530。
		windowKey := vs.cache.Key("hot:video:1m:%s", now.Format("200601021504"))
		// member 用视频编号字符串当 ZSET 成员。
		member := strconv.FormatUint(uint64(id), 10)

		// opCtx 50ms 超时；cancel 取消函数。
		opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 释放。
		defer cancel()

		// ZincrBy：给这一分钟桶里该视频的分数 += change（累加热度增量）。
		_ = vs.cache.ZincrBy(opCtx, windowKey, member, float64(change))
		// Expire：给分钟桶设 2 小时过期，旧桶自动清理，不无限堆积。
		_ = vs.cache.Expire(opCtx, windowKey, 2*time.Hour)
	}
	// 完成：返回 nil。
	return nil
}
