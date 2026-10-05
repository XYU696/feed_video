// package social：关注业务包。本文件是关注仓储层，
// 封装对 socials 表（粉丝-博主关注关系）的增删查和计数。
package social

import (
	// context：请求上下文。
	"context"
	// account：返回粉丝/博主信息时用到 account.Account 结构体。
	"feedsystem_video_go/internal/account"

	// gorm：ORM。
	"gorm.io/gorm"
)

// SocialRepository 结构体是关注仓储，持有数据库连接。
type SocialRepository struct {
	// db 数据库连接（包私有）。
	db *gorm.DB
}

// NewSocialRepository 是构造函数：创建关注仓储。
//
// 参数 db：数据库连接；
// 返回值 *SocialRepository：仓储。
func NewSocialRepository(db *gorm.DB) *SocialRepository {
	// 初始化并返回指针。
	return &SocialRepository{db: db}
}

// Follow 方法：向 socials 表插入一条关注关系。
//
// 参数 social：关注对象（FollowerID 粉丝、VloggerID 被关注博主）；
// 返回值 error：错误（重复关注会触发复合唯一索引冲突）。
func (r *SocialRepository) Follow(ctx context.Context, social *Social) error {
	// .Create 执行 INSERT，直接返回 Error。
	return r.db.WithContext(ctx).Create(social).Error
}

// Unfollow 方法：删除"某粉丝关注某博主"的关系。
//
// 参数 social：用两字段定位要删的行；
// 返回值 error：错误。
func (r *SocialRepository) Unfollow(ctx context.Context, social *Social) error {
	// 链式删除：
	return r.db.WithContext(ctx).
		// Where 两个条件同时成立。
		Where("follower_id = ? AND vlogger_id = ?", social.FollowerID, social.VloggerID).
		// Delete 执行删除。
		Delete(&Social{}).Error
}

// GetAllFollowers 方法：查某博主的全部粉丝（返回粉丝的账号信息列表）。
//
// 实现分两步（而不是直接 JOIN）：
//  1. 先从 socials 表查出所有关注关系，收集粉丝编号；
//  2. 再用 IN 一次性把这些粉丝的账号信息查出来。
//
// 参数 VloggerID：博主编号；
// 返回值：[]*account.Account 粉丝账号指针切片、error 错误。
func (r *SocialRepository) GetAllFollowers(ctx context.Context, VloggerID uint) ([]*account.Account, error) {
	// relations 声明关注关系切片。
	var relations []Social
	// 查该博主的所有关注关系（谁关注了 TA）；err 接收错误。
	if err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Social{}).
		// 条件 vlogger_id = 博主。
		Where("vlogger_id = ?", VloggerID).
		// 最多 200 条。
		Limit(200).
		// 执行查询。
		Find(&relations).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}

	// followerIDs 创建粉丝编号切片。
	// make([]uint, 0, len(relations))：长度 0、【容量】预分配为关系数，
	// 这样 append 时不用反复扩容（性能小优化；容量 cap 是切片底层数组能装多少）。
	followerIDs := make([]uint, 0, len(relations))
	// 遍历关系；rel 是当前关注关系。
	for _, rel := range relations {
		// append 把该关系的粉丝编号加进切片，返回新切片重新赋值。
		followerIDs = append(followerIDs, rel.FollowerID)
	}
	// 一个粉丝都没有。
	if len(followerIDs) == 0 {
		// 返回【空指针切片】（而不是 nil），JSON 会序列化成 []。
		return []*account.Account{}, nil
	}

	// followers 声明粉丝账号指针切片，准备第二步查询。
	var followers []*account.Account
	// 按收集到的粉丝编号批量查账号信息；err 接收错误。
	if err := r.db.WithContext(ctx).
		// 指定 accounts 表。
		Model(&account.Account{}).
		// IN 一次查全部粉丝。
		Where("id IN ?", followerIDs).
		// 执行查询。
		Find(&followers).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 返回粉丝账号列表。
	return followers, nil
}

// GetAllVloggers 方法：查某用户关注的全部博主（"我的关注"列表）。
//
// 结构与 GetAllFollowers 完全对称，只是查询字段换成 follower_id、收集 VloggerID。
//
// 参数 FollowerID：粉丝（当前用户）编号；
// 返回值：[]*account.Account 博主账号指针切片、error 错误。
func (r *SocialRepository) GetAllVloggers(ctx context.Context, FollowerID uint) ([]*account.Account, error) {
	// relations 声明关系切片。
	var relations []Social
	// 查该用户关注别人的所有关系；err 接收错误。
	if err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Social{}).
		// 条件 follower_id = 当前用户。
		Where("follower_id = ?", FollowerID).
		// 最多 200。
		Limit(200).
		// 执行。
		Find(&relations).Error; err != nil {
		// 出错返回。
		return nil, err
	}

	// vloggerIDs 创建博主编号切片（预分配容量）。
	vloggerIDs := make([]uint, 0, len(relations))
	// 遍历关系。
	for _, rel := range relations {
		// 收集被关注的博主编号。
		vloggerIDs = append(vloggerIDs, rel.VloggerID)
	}
	// 没关注任何人。
	if len(vloggerIDs) == 0 {
		// 返回空切片。
		return []*account.Account{}, nil
	}

	// vloggers 声明博主账号指针切片。
	var vloggers []*account.Account
	// 批量查博主账号信息；err 接收错误。
	if err := r.db.WithContext(ctx).
		// accounts 表。
		Model(&account.Account{}).
		// IN 查全部博主。
		Where("id IN ?", vloggerIDs).
		// 执行。
		Find(&vloggers).Error; err != nil {
		// 出错返回。
		return nil, err
	}
	// 返回博主列表。
	return vloggers, nil
}

// IsFollowed 方法：判断某粉丝是否已关注某博主。
//
// 参数 social：用两字段定位；
// 返回值：bool 是否关注、error 错误。
func (r *SocialRepository) IsFollowed(ctx context.Context, social *Social) (bool, error) {
	// count 接收计数。
	var count int64
	// 统计满足两条件的关系行数；err 接收错误。
	if err := r.db.WithContext(ctx).
		// 指定表。
		Model(&Social{}).
		// 两条件。
		Where("follower_id = ? AND vlogger_id = ?", social.FollowerID, social.VloggerID).
		// COUNT。
		Count(&count).Error; err != nil {
		// 出错返回。
		return false, err
	}
	// count > 0 即已关注。
	return count > 0, nil
}

// CountFollowers 方法：统计某博主的粉丝数量。
//
// 参数 vloggerID：博主编号；
// 返回值：int64 数量、error 错误。
func (r *SocialRepository) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	// count 接收计数。
	var count int64
	// COUNT WHERE vlogger_id = ?；err 接收错误。
	if err := r.db.WithContext(ctx).Model(&Social{}).Where("vlogger_id = ?", vloggerID).Count(&count).Error; err != nil {
		// 出错返回 0。
		return 0, err
	}
	// 返回数量。
	return count, nil
}

// CountVloggers 方法：统计某用户关注了多少个博主（"关注数"）。
//
// 参数 followerID：用户编号；
// 返回值：int64 数量、error 错误。
func (r *SocialRepository) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	// count 接收计数。
	var count int64
	// COUNT WHERE follower_id = ?；err 接收错误。
	if err := r.db.WithContext(ctx).Model(&Social{}).Where("follower_id = ?", followerID).Count(&count).Error; err != nil {
		// 出错返回 0。
		return 0, err
	}
	// 返回数量。
	return count, nil
}
