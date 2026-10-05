// package video：视频业务包。本文件是视频仓储层，
// 封装对 videos 表和 outbox_msgs 表的数据库操作。
package video

import (
	// context：请求上下文。
	"context"
	// errors：用 errors.Is 判断"记录不存在"。
	"errors"

	// gorm：ORM。
	"gorm.io/gorm"
)

// VideoRepository 结构体是视频仓储，持有数据库连接。
type VideoRepository struct {
	// db 数据库连接（包私有）。
	db *gorm.DB
}

// NewVideoRepository 是构造函数：创建视频仓储。
//
// 参数 db：数据库连接；
// 返回值 *VideoRepository：仓储。
func NewVideoRepository(db *gorm.DB) *VideoRepository {
	// 初始化并返回指针。
	return &VideoRepository{db: db}
}

// CreateVideo 方法：向 videos 表插入一条视频记录。
//
// 参数：video 视频指针（插入后 ID 会回填）；
// 返回值 error：错误。
func (vr *VideoRepository) CreateVideo(ctx context.Context, video *Video) error {
	// .Create(video) 执行 INSERT；err 接收错误。
	if err := vr.db.WithContext(ctx).Create(video).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// CreateMsg 方法：向 outbox_msgs（发件箱）表插入一条待发消息。
//
// 技术点：发视频时业务数据和"待发事件"先在同一个数据库事务里写好，
// 之后由轮询器异步发到 MQ——这是保证"数据库操作和发消息一致性"的 Outbox 模式。
//
// 参数 Msg：发件箱消息指针；
// 返回值 error：错误。
func (vr *VideoRepository) CreateMsg(ctx context.Context, Msg *OutboxMsg) error {
	// 插入发件箱记录；err 接收错误。
	if err := vr.db.WithContext(ctx).Create(Msg).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// DeleteVideo 方法：按主键删除视频。
//
// 参数 id：视频编号；
// 返回值 error：错误。
func (vr *VideoRepository) DeleteVideo(ctx context.Context, id uint) error {
	// .Delete(&Video{}, id) 按主键执行 DELETE；err 接收错误。
	if err := vr.db.WithContext(ctx).Delete(&Video{}, id).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// ListByAuthorID 方法：查某个作者发布的视频，按发布时间倒序，最多 200 条。
//
// 参数 authorID：作者编号（这里类型是 int64，匹配查询参数）；
// 返回值：[]Video 视频值切片、error 错误。
func (vr *VideoRepository) ListByAuthorID(ctx context.Context, authorID int64) ([]Video, error) {
	// videos 声明视频切片。
	var videos []Video
	// 链式查询（一行用 . 续多行）：
	if err := vr.db.WithContext(ctx).
		// Where：条件 author_id = ?。
		Where("author_id = ?", authorID).
		// Order("create_time desc")：按创建时间从新到旧排序。
		Order("create_time desc").
		// Limit(200)：最多 200 条，防止大 V 视频过多一次拉爆。
		Limit(200).
		// Find(&videos)：执行查询填充切片，.Error 取错误。
		Find(&videos).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回切片。
	return videos, nil
}

// ListRecentByAuthorID 方法：查某作者【最近】发布的若干条视频（取证用，只要近期内容）。
//
// 参数：authorID 作者编号、limit 最多返回条数（调用方做兜底）；
// 返回值：[]Video、error。
func (vr *VideoRepository) ListRecentByAuthorID(ctx context.Context, authorID uint, limit int) ([]Video, error) {
	// videos 结果切片。
	var videos []Video
	// 链式查询；返回其错误。
	err := vr.db.WithContext(ctx).
		// 定位作者。
		Where("author_id = ?", authorID).
		// 最新在前。
		Order("create_time desc").
		// 限条数。
		Limit(limit).
		// 执行。
		Find(&videos).Error
	// 返回。
	return videos, err
}

// GetByID 方法：按主键查一个视频。
//
// 参数 id：视频编号；
// 返回值：*Video 视频指针、error 错误。
func (vr *VideoRepository) GetByID(ctx context.Context, id uint) (*Video, error) {
	// video 准备接收结果。
	var video Video
	// .First(&video, id) 按主键查；err 接收错误。
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		// (*Video)(nil)：显式把 nil 转成"Video 指针类型的空值"再返回（和返回值类型严格对应）。
		return (*Video)(nil), err
	}
	// 成功返回视频地址。
	return &video, nil
}

// UpdateLikesCount 方法：把点赞数【直接设置】成给定值（与"增减量"方法区分）。
//
// 参数：id 视频编号、likesCount 目标点赞总数；
// 返回值 error：错误。
func (vr *VideoRepository) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	// UPDATE likes_count = ?；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		// 条件。
		Where("id = ?", id).
		// 设置列值并取错误。
		Update("likes_count", likesCount).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// IsExist 方法：判断某个视频是否存在。
//
// 和 GetByID 的区别：这里把"查不到"这种错误转换成 (false, nil)，
// 调用方只关心"在不在"，不用处理 gorm.ErrRecordNotFound。
//
// 参数 id：视频编号；
// 返回值：bool 是否存在、error 其他数据库错误。
func (vr *VideoRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	// video 准备接收查询。
	var video Video
	// 按主键查；err 接收错误。
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		// errors.Is(err, gorm.ErrRecordNotFound)：错误是"没这条记录"。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 返回 false、nil（不存在但不是系统错误）。
			return false, nil
		}
		// 其他错误（连接断了等）：返回 false 和真正的错误。
		return false, err
	}
	// 查到了：返回 true、nil。
	return true, nil
}

// UpdatePopularity 方法：把热度【直接设置】成给定值。
//
// 参数：id 视频编号、change 这里其实是目标热度值（命名沿用 change）；
// 返回值 error：错误。
func (vr *VideoRepository) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	// UPDATE popularity = ?；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		// 条件。
		Where("id = ?", id).
		// 设置列。
		Update("popularity", change).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// ChangeLikesCount 方法：在现有点赞数上【增减】change，并保证结果不会变成负数。
//
// 技术点 1：gorm.Expr 允许写原生 SQL 表达式，"likes_count + ?" 在数据库端做原子加法，
// 比"先查出来再加再写回"更安全（并发下不会丢更新）。
// 技术点 2：GREATEST(likes_count + ?, 0) 取"计算结果和 0 里较大的那个"，
// 防止异常事件（重复取消点赞）把点赞数减成负数。
// 技术点 3：UpdateColumn 与 Update 区别——它会【跳过 GORM 的自动更新时间钩子】，只改这一列。
//
// 参数：id 视频编号、change 增减量（正/负）；
// 返回值 error：错误。
func (vr *VideoRepository) ChangeLikesCount(ctx context.Context, id uint, change int64) error {
	// 执行带 GREATEST 的原子增减；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		// 条件。
		Where("id = ?", id).
		// UpdateColumn 写 SQL 表达式。
		UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count + ?, 0)", change)).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// ChangePopularity 方法：在现有热度上【增减】change，同样用 GREATEST 兜底不为负。
//
// 参数：id 视频编号、change 热度增减量；
// 返回值 error：错误。
func (vr *VideoRepository) ChangePopularity(ctx context.Context, id uint, change int64) error {
	// UPDATE popularity = GREATEST(popularity + ?, 0)；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		// 条件。
		Where("id = ?", id).
		// 原子增减。
		UpdateColumn("popularity", gorm.Expr("GREATEST(popularity + ?, 0)", change)).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// CountByAuthor 方法：统计某作者发布了多少个视频（用户主页"作品数"）。
//
// 参数 authorID：作者编号；
// 返回值：int64 数量、error 错误。
func (vr *VideoRepository) CountByAuthor(ctx context.Context, authorID uint) (int64, error) {
	// count 准备接收计数结果。
	var count int64
	// .Model(&Video{}).Where(...).Count(&count)：生成 SELECT COUNT(*)；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Count(&count).Error; err != nil {
		// 出错返回 0。
		return 0, err
	}
	// 成功返回数量。
	return count, nil
}

// TotalLikesByAuthor 方法：统计某作者所有视频的点赞总数（用户主页"获赞数"）。
//
// 技术点：Select("COALESCE(SUM(likes_count), 0)")：
//   - SUM(likes_count) 把该作者所有视频的点赞数加起来；
//   - COALESCE(..., 0) 处理"一条视频都没有"的情况——SUM 空集会返回 NULL，COALESCE 把 NULL 替换成 0。
//
// 参数 authorID：作者编号；
// 返回值：int64 总点赞数、error 错误。
func (vr *VideoRepository) TotalLikesByAuthor(ctx context.Context, authorID uint) (int64, error) {
	// total 接收聚合结果。
	var total int64
	// Select 指定聚合表达式，Scan(&total) 把扫描结果写进单个变量；err 接收错误。
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Select("COALESCE(SUM(likes_count), 0)").Scan(&total).Error; err != nil {
		// 出错返回 0。
		return 0, err
	}
	// 成功返回总数。
	return total, nil
}
