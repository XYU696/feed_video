// package video：视频业务包。本文件是点赞服务层，
// 承载点赞/取消点赞的完整业务编排，核心设计是：
//
//	优先异步（发 MQ 交给 worker 落库）
//	  → MQ 不可用时降级同步直写（事务直接改 MySQL）
//	热度同样：先发热度 MQ，失败就直接改 Redis
//
// 这样无论 MQ 在不在，点赞功能都能用，且点赞数、热度都不会丢。
package video

import (
	// context：请求上下文。
	"context"
	// errors：创建/识别错误。
	"errors"
	// rabbitmq：点赞 MQ、热度 MQ 类型。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// time：点赞时间。
	"time"

	// mysql：识别 1062 重复键。
	"github.com/go-sql-driver/mysql"
	// gorm：事务、模型、ErrRecordNotFound。
	"gorm.io/gorm"
)

// LikeService 结构体是点赞服务层，持有仓储、视频仓储、缓存和两个 MQ 发布器。
type LikeService struct {
	// repo 点赞仓储，操作 likes 表。
	repo *LikeRepository
	// VideoRepo 视频仓储（注意它大写开头 = 可被包外访问），用来校验视频是否存在。
	VideoRepo *VideoRepository
	// cache Redis 客户端，可能为 nil。
	cache *rediscache.Client
	// likeMQ 点赞事件发布器，可能为 nil。
	likeMQ *rabbitmq.LikeMQ
	// popularityMQ 热度事件发布器，可能为 nil。
	popularityMQ *rabbitmq.PopularityMQ
}

// NewLikeService 是构造函数：创建点赞服务，把五个依赖全部注入。
//
// 返回值 *LikeService：服务。
func NewLikeService(repo *LikeRepository, videoRepo *VideoRepository, cache *rediscache.Client, likeMQ *rabbitmq.LikeMQ, popularityMQ *rabbitmq.PopularityMQ) *LikeService {
	// 用结构体字面量装配并返回指针。
	return &LikeService{repo: repo, VideoRepo: videoRepo, cache: cache, likeMQ: likeMQ, popularityMQ: popularityMQ}
}

// isDupKey 是包内辅助函数：判断错误是不是 MySQL 1062 唯一键冲突。
//
// 参数 err：待判断错误；
// 返回值 bool：是重复键返回 true。
func isDupKey(err error) bool {
	// me 声明 MySQL 错误类型指针。
	var me *mysql.MySQLError
	// errors.As 能提取出 *mysql.MySQLError，且编号 1062 才返回 true。
	return errors.As(err, &me) && me.Number == 1062
}

// Like 方法：点赞的完整业务逻辑。
//
// 流程：参数校验 → 确认视频存在 → 确认此前没赞过 → 尝试发两条 MQ
// → 两条都成功直接返回 → 失败的那一侧走同步降级直写。
//
// 参数 like：点赞对象（AccountID、VideoID）；
// 返回值 error：错误。
func (s *LikeService) Like(ctx context.Context, like *Like) error {
	// 空对象。
	if like == nil {
		// 返回错误。
		return errors.New("like is nil")
	}
	// 视频编号或账号编号为 0。
	if like.VideoID == 0 || like.AccountID == 0 {
		// 返回错误。
		return errors.New("video_id and account_id are required")
	}

	// 视频仓储可用时先校验视频真实存在（防止给不存在的视频点赞）。
	if s.VideoRepo != nil {
		// ok 视频是否存在；err 接收错误。
		ok, err := s.VideoRepo.IsExist(ctx, like.VideoID)
		if err != nil {
			// 查询出错：返回。
			return err
		}
		// 视频不存在。
		if !ok {
			// 返回错误。
			return errors.New("video not found")
		}
	}

	// isLiked 查该用户是否已赞过；err 接收错误。
	isLiked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		// 出错返回。
		return err
	}
	// 已经赞过。
	if isLiked {
		// 返回错误（防止重复点赞导致计数 +1）。
		return errors.New("user has liked this video")
	}

	// like.CreatedAt 记录点赞时间（走 MQ 异步路径时事件时间由它体现）。
	like.CreatedAt = time.Now()
	// mysqlEnqueued 标记"点赞落库事件"是否已成功发到 MQ。
	mysqlEnqueued := false
	// redisEnqueued 标记"热度变更事件"是否已成功发到 MQ。
	redisEnqueued := false
	// 点赞 MQ 可用时尝试发布点赞事件。
	if s.likeMQ != nil {
		// s.likeMQ.Like(账号, 视频) 发布 like.like 事件；err == nil 才算入队成功。
		if err := s.likeMQ.Like(ctx, like.AccountID, like.VideoID); err == nil {
			// 标记落库事件已入队（之后由 LikeWorker 异步写 likes 表 + 点赞数）。
			mysqlEnqueued = true
		}
	}
	// 热度 MQ 可用时尝试发布热度 +1 事件。
	if s.popularityMQ != nil {
		// Update(视频, 1)：点赞给热度加 1；成功才标记。
		if err := s.popularityMQ.Update(ctx, like.VideoID, 1); err == nil {
			// 标记热度事件已入队。
			redisEnqueued = true
		}
	}
	// 两条 MQ 都成功：本请求处理完毕，异步交给两个 worker。
	if mysqlEnqueued && redisEnqueued {
		// 返回 nil。
		return nil
	}

	// Fallback: direct MySQL write when like MQ publish fails.
	// 点赞事件没入队（MQ 挂了或没配）：同步开事务直接写库，保证点赞不丢。
	if !mysqlEnqueued {
		// err 接收事务结果。s.repo.db.Transaction 开启事务，回调内全用 tx。
		err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// 第 1 步：在事务里确认视频存在。Select("id") 只查主键列（够用且更快）。
			if err := tx.Select("id").First(&Video{}, like.VideoID).Error; err != nil {
				// 视频不存在。
				if errors.Is(err, gorm.ErrRecordNotFound) {
					// 返回错误 → 回滚。
					return errors.New("video not found")
				}
				// 其他查询错误 → 回滚。
				return err
			}
			// 第 2 步：插入点赞记录。
			if err := tx.Create(like).Error; err != nil {
				// 唯一键冲突 = 其实已经赞过（并发下的重复请求）。
				if isDupKey(err) {
					// 返回业务错误 → 回滚。
					return errors.New("user has liked this video")
				}
				// 其他错误 → 回滚。
				return err
			}
			// 第 3 步：视频点赞数 +1（原子自增）。
			if err := tx.Model(&Video{}).Where("id = ?", like.VideoID).
				// UpdateColumn + gorm.Expr 在数据库端 likes_count + 1。
				UpdateColumn("likes_count", gorm.Expr("likes_count + 1")).Error; err != nil {
				// 失败 → 回滚。
				return err
			}
			// 第 4 步：视频热度也 +1（与 worker 落库逻辑保持一致）。
			return tx.Model(&Video{}).Where("id = ?", like.VideoID).
				// popularity + 1；返回其错误（nil → 提交事务）。
				UpdateColumn("popularity", gorm.Expr("popularity + 1")).Error
		})
		if err != nil {
			// 事务失败：返回错误。
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	// 热度事件没入队：直接更新 Redis（UpdatePopularityCache 定义在 popularity_cache.go，#40 会注释到）。
	if !redisEnqueued {
		// 传入上下文、缓存、视频编号、增量 +1，直接改 Redis 的热度结构。
		UpdatePopularityCache(ctx, s.cache, like.VideoID, 1)
	}
	// 全部完成：返回 nil。
	return nil
}

// Unlike 方法：取消点赞的完整业务逻辑，结构和 Like 对称，方向相反（删除记录、计数 -1）。
//
// 参数 like：点赞对象；
// 返回值 error：错误。
func (s *LikeService) Unlike(ctx context.Context, like *Like) error {
	// 空对象。
	if like == nil {
		// 返回错误。
		return errors.New("like is nil")
	}
	// 编号缺失。
	if like.VideoID == 0 || like.AccountID == 0 {
		// 返回错误。
		return errors.New("video_id and account_id are required")
	}

	// 校验视频存在。
	if s.VideoRepo != nil {
		// ok、err 接收结果。
		ok, err := s.VideoRepo.IsExist(ctx, like.VideoID)
		if err != nil {
			// 出错返回。
			return err
		}
		// 视频不存在。
		if !ok {
			// 返回错误。
			return errors.New("video not found")
		}
	}

	// isLiked 确认当前确实是赞过的状态。
	isLiked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		// 出错返回。
		return err
	}
	// 根本没赞过。
	if !isLiked {
		// 返回错误（防止对不存在的点赞做 -1）。
		return errors.New("user has not liked this video")
	}

	// 两个入队标记（同 Like）。
	mysqlEnqueued := false
	// redisEnqueued 热度入队标记。
	redisEnqueued := false
	// 发取消点赞事件。
	if s.likeMQ != nil {
		// s.likeMQ.Unlike 发布 like.unlike 事件。
		if err := s.likeMQ.Unlike(ctx, like.AccountID, like.VideoID); err == nil {
			// 标记落库事件已入队。
			mysqlEnqueued = true
		}
	}
	// 发热度 -1 事件。
	if s.popularityMQ != nil {
		// Update(视频, -1)：取消点赞热度减 1。
		if err := s.popularityMQ.Update(ctx, like.VideoID, -1); err == nil {
			// 标记热度事件已入队。
			redisEnqueued = true
		}
	}
	// 两条都成功：交给 worker。
	if mysqlEnqueued && redisEnqueued {
		// 返回 nil。
		return nil
	}

	// Fallback: direct MySQL write when like MQ publish fails.
	// 取消点赞事件没入队：同步事务直写。
	if !mysqlEnqueued {
		// err 接收事务结果。
		err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// del 执行删除点赞记录（用两条件定位）。
			del := tx.Where("video_id = ? AND account_id = ?", like.VideoID, like.AccountID).Delete(&Like{})
			// 删除本身出错。
			if del.Error != nil {
				// 返回错误 → 回滚。
				return del.Error
			}
			// 一行都没删掉 = 本来就没赞。
			if del.RowsAffected == 0 {
				// 返回错误 → 回滚。
				return errors.New("user has not liked this video")
			}

			// 点赞数 -1，并用 GREATEST 兜底不为负。
			if err := tx.Model(&Video{}).Where("id = ?", like.VideoID).
				// likes_count - 1，最小为 0。
				UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count - 1, 0)")).Error; err != nil {
				// 失败 → 回滚。
				return err
			}
			// 热度 -1，同样兜底不为负。
			return tx.Model(&Video{}).Where("id = ?", like.VideoID).
				// popularity - 1，返回错误决定提交/回滚。
				UpdateColumn("popularity", gorm.Expr("GREATEST(popularity - 1, 0)")).Error
		})
		if err != nil {
			// 事务失败返回。
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	// 热度事件没入队：直接改 Redis，增量 -1。
	if !redisEnqueued {
		// UpdatePopularityCache 直接更新 Redis 热度。
		UpdatePopularityCache(ctx, s.cache, like.VideoID, -1)
	}
	// 完成：返回 nil。
	return nil
}

// IsLiked 方法：查询是否已赞的薄封装（直接委托仓储）。
//
// 参数：videoID 视频编号、accountID 账号编号；
// 返回值：bool、error。
func (s *LikeService) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	// 返回仓储查询结果。
	return s.repo.IsLiked(ctx, videoID, accountID)
}

// ListLikedVideos 方法：查"我赞过的视频"的薄封装。
//
// 参数 accountID：账号编号；
// 返回值：[]Video、error。
func (s *LikeService) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	// 返回仓储的 JOIN 查询结果。
	return s.repo.ListLikedVideos(ctx, accountID)
}
