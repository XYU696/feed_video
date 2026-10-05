// package social：关注业务包。本文件是关注服务层，
// 承载关注/取关的业务校验，并维护"关注流"缓存的一致性。
//
// 和点赞服务的设计取向不同：关注关系本身很小、且要立刻生效，
// 所以这里【先同步写数据库】，再失效缓存，MQ 只用来发"被关注通知"，失败不影响主流程。
package social

import (
	// context：请求上下文。
	"context"
	// errors：返回"不能关注自己"等错误。
	"errors"
	// account：账号仓储，校验粉丝/博主是否真实存在。
	"feedsystem_video_go/internal/account"
	// rabbitmq：关注 MQ（发通知）。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// log：MQ/缓存失败记日志。
	"log"
)

// SocialService 结构体是关注服务层。
type SocialService struct {
	// repo 关注仓储，操作 socials 表。
	repo *SocialRepository
	// accountrepo 账号仓储，校验用户存在。
	accountrepo *account.AccountRepository
	// socialMQ 关注事件发布器（用于通知），可能为 nil。
	socialMQ *rabbitmq.SocialMQ
	// cache Redis 客户端，可能为 nil。
	cache *rediscache.Client
}

// NewSocialService 是构造函数：创建关注服务，注入四个依赖。
//
// 返回值 *SocialService：服务。
func NewSocialService(repo *SocialRepository, accountrepo *account.AccountRepository, socialMQ *rabbitmq.SocialMQ, cache *rediscache.Client) *SocialService {
	// 装配并返回指针。
	return &SocialService{repo: repo, accountrepo: accountrepo, socialMQ: socialMQ, cache: cache}
}

// Follow 方法：关注的完整业务逻辑。
//
// 流程：校验粉丝存在 → 校验博主存在 → 不能关注自己 → 不能重复关注
// → 先写数据库 → 失效关注流缓存 → 最后发 MQ 通知。
//
// 参数 social：关注对象；
// 返回值 error：错误。
func (s *SocialService) Follow(ctx context.Context, social *Social) error {
	// 校验粉丝（主动关注者）真实存在；_ 忽略返回的账号对象，只看 err。
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		// 粉丝账号不存在：返回错误。
		return err
	}
	// 校验被关注博主存在（这里用 = 复用 err）。
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		// 博主不存在：返回。
		return err
	}
	// 粉丝编号 == 博主编号：想关注自己。
	if social.FollowerID == social.VloggerID {
		// 返回错误（不能自己关注自己）。
		return errors.New("can not follow self")
	}
	// isFollowed 查是否已经关注过；err 接收错误。
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		// 查询出错：返回。
		return err
	}
	// 已经关注。
	if isFollowed {
		// 返回错误（防止重复插入产生重复关系/重复通知）。
		return errors.New("already followed")
	}

	// 先写 DB，确保数据持久化
	// 同步插入关注关系；err 接收错误。
	if err := s.repo.Follow(ctx, social); err != nil {
		// 写库失败：返回。
		return err
	}

	// DB 成功后，失效该用户的关注列表缓存
	// 调本服务方法，删掉这个粉丝相关的"关注流"缓存（用 background 上下文）。
	s.invalidateFollowingFeedCache(context.Background(), social.FollowerID)

	// 最后发 MQ（用于通知），失败只记日志不影响业务
	// MQ 可用时。
	if s.socialMQ != nil {
		// 发布关注事件（通知被关注者）；err 接收错误。
		if err := s.socialMQ.Follow(ctx, social.FollowerID, social.VloggerID); err != nil {
			// MQ 失败只记日志——数据库已经成功，关注关系已经生效。
			log.Printf("social MQ Follow 发布失败: %v", err)
		}
	}
	// 完成：返回 nil。
	return nil
}

// Unfollow 方法：取关的完整业务逻辑，与 Follow 对称。
//
// 流程：校验双方存在 → 必须此前是关注状态 → 先删数据库 → 失效缓存 → 发 MQ。
//
// 参数 social：关注对象；
// 返回值 error：错误。
func (s *SocialService) Unfollow(ctx context.Context, social *Social) error {
	// 校验粉丝存在；err 接收错误。
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		// 返回。
		return err
	}
	// 校验博主存在。
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		// 返回。
		return err
	}
	// isFollowed 查当前是否关注；err 接收错误。
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		// 出错返回。
		return err
	}
	// 本来就没关注。
	if !isFollowed {
		// 返回错误（防止对不存在的关系做多余操作）。
		return errors.New("not followed")
	}

	// 先写 DB
	// 删除关注关系；err 接收错误。
	if err := s.repo.Unfollow(ctx, social); err != nil {
		// 失败返回。
		return err
	}

	// 失效缓存
	// 删掉该粉丝的关注流缓存。
	s.invalidateFollowingFeedCache(context.Background(), social.FollowerID)

	// 最后发 MQ
	// MQ 可用时发取关事件；err 接收错误。
	if s.socialMQ != nil {
		// UnFollow 发布取关事件。
		if err := s.socialMQ.UnFollow(ctx, social.FollowerID, social.VloggerID); err != nil {
			// 失败只记日志。
			log.Printf("social MQ UnFollow 发布失败: %v", err)
		}
	}
	// 完成：返回 nil。
	return nil
}

// invalidateFollowingFeedCache 方法：按通配模式删除某用户所有的"关注流"缓存。
//
// 为什么需要？关注/取关后，"我关注的人的视频列表"内容就变了，
// 但关注流缓存键里带有分页游标（有很多个键），没法精确删一个，只能按模式批量删。
//
// 参数：ctx 上下文、accountID 粉丝编号；无返回值。
func (s *SocialService) invalidateFollowingFeedCache(ctx context.Context, accountID uint) {
	// 没配缓存：直接返回。
	if s.cache == nil {
		// 返回。
		return
	}
	// pattern 拼通配模式：feed:listByFollowing:*:accountID={账号}:*
	// * 匹配任意分页游标等段落（DelByPattern 内部用 SCAN 逐个找，不会阻塞 Redis）。
	pattern := s.cache.Key("feed:listByFollowing:*:accountID=%d:*", accountID)
	// DelByPattern 按模式批量删除；err 接收错误。
	if err := s.cache.DelByPattern(ctx, pattern); err != nil {
		// 失败只记日志，不影响关注主流程。
		log.Printf("失效 Following 缓存失败: accountID=%d, err=%v", accountID, err)
	}
}

// GetAllFollowers 方法：查某博主粉丝列表，先确认博主存在。
//
// 参数 VloggerID：博主编号；
// 返回值：[]*account.Account 粉丝列表、error 错误。
func (s *SocialService) GetAllFollowers(ctx context.Context, VloggerID uint) ([]*account.Account, error) {
	// 校验博主账号存在；err 接收错误。
	_, err := s.accountrepo.FindByID(ctx, VloggerID)
	if err != nil {
		// 不存在：返回 nil。
		return nil, err
	}
	// 返回仓储查询结果。
	return s.repo.GetAllFollowers(ctx, VloggerID)
}

// GetAllVloggers 方法：查某用户关注的博主列表，先确认用户存在。
//
// 参数 FollowerID：用户编号；
// 返回值：[]*account.Account 博主列表、error 错误。
func (s *SocialService) GetAllVloggers(ctx context.Context, FollowerID uint) ([]*account.Account, error) {
	// 校验用户存在；err 接收错误。
	_, err := s.accountrepo.FindByID(ctx, FollowerID)
	if err != nil {
		// 返回。
		return nil, err
	}
	// 返回仓储查询结果。
	return s.repo.GetAllVloggers(ctx, FollowerID)
}

// CountFollowers 方法：粉丝数的薄封装。
//
// 参数 vloggerID：博主编号；
// 返回值：int64、error。
func (s *SocialService) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	// 返回仓储计数。
	return s.repo.CountFollowers(ctx, vloggerID)
}

// CountVloggers 方法：关注数的薄封装。
//
// 参数 followerID：用户编号；
// 返回值：int64、error。
func (s *SocialService) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	// 返回仓储计数。
	return s.repo.CountVloggers(ctx, followerID)
}

// IsFollowed 方法：判断是否关注，先校验双方账号都存在。
//
// 参数 social：关注对象；
// 返回值：bool、error。
func (s *SocialService) IsFollowed(ctx context.Context, social *Social) (bool, error) {
	// 校验粉丝存在；err 接收错误。
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		// 返回。
		return false, err
	}
	// 校验博主存在。
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		// 返回。
		return false, err
	}
	// 返回仓储的关注判定。
	return s.repo.IsFollowed(ctx, social)
}
