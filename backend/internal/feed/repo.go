// package feed：信息流（Feed）业务包。本文件是仓储层，
// 只负责写 SQL/GORM 查询：最新流、点赞榜、关注流、热度榜、按 ID 批量取、按标签取。
package feed

import (
	// context：请求上下文。
	"context"
	// social：关注流子查询要用到 social.Social 结构体（socials 表）。
	"feedsystem_video_go/internal/social"
	// video：所有查询返回的都是 video.Video（videos 表）。
	"feedsystem_video_go/internal/video"
	// time：时间游标类型。
	"time"

	// gorm：ORM。
	"gorm.io/gorm"
)

// FeedRepository 结构体：信息流仓储，持有数据库连接。
type FeedRepository struct {
	// db 数据库连接（包私有，外面拿不到）。
	db *gorm.DB
}

// NewFeedRepository 是构造函数：创建信息流仓储。
//
// 参数 db：数据库连接；
// 返回值 *FeedRepository：仓储指针。
func NewFeedRepository(db *gorm.DB) *FeedRepository {
	// 装配并返回地址。
	return &FeedRepository{db: db}
}

// ListLatest 方法：查"最新视频流"，按发布时间倒序，支持时间游标翻页。
//
// 参数：limit 本页条数；latestBefore 游标——只查这个时间【之前】发的视频，
// 零值 time.Time{} 表示第一页（不加时间条件）；
// 返回值：[]*video.Video 视频指针切片、error 错误。
func (repo *FeedRepository) ListLatest(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error) {
	// videos 声明结果切片（指针切片，元素可空、省拷贝）。
	var videos []*video.Video
	// query 先拼出基础查询链：指定 videos 表 + 按发布时间倒序。
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		// create_time 越大（越新）越靠前。
		Order("create_time DESC")
	// latestBefore 不是零值（即客户端传了游标）。
	if !latestBefore.IsZero() {
		// 追加条件：只要发布时间严格早于游标的视频。
		query = query.Where("create_time < ?", latestBefore)
	}
	// Limit 限条数，Find 执行查询；err 接收错误。
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		// 出错：返回 nil。
		return nil, err
	}
	// 成功：返回视频列表。
	return videos, nil
}

// ListLikesCountWithCursor 方法：查"点赞榜"，先按点赞数倒序、点赞相同再按 id 倒序。
//
// 参数：limit 条数；cursor 复合游标（上次最后一条的点赞数 + id），nil 表示第一页；
// 返回值：视频指针切片、错误。
func (repo *FeedRepository) ListLikesCountWithCursor(ctx context.Context, limit int, cursor *LikesCountCursor) ([]*video.Video, error) {
	// videos 声明结果切片。
	var videos []*video.Video
	// query 基础查询：双字段排序，保证排序【稳定】、不会因为点赞数相同而翻页重复。
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("likes_count DESC, id DESC")

	// 传了游标才加过滤条件。
	if cursor != nil {
		// "排序在游标之后"的 OR 条件：
		query = query.Where(
			// ① 点赞数比游标少（排得更靠后）；
			// ② 点赞数相同但 id 更小（同点赞数里更靠后）。
			"(likes_count < ?) OR (likes_count = ? AND id < ?)",
			// 三个占位符：点赞数（①用）、点赞数（②用）、id（②用）。
			cursor.LikesCount,
			cursor.LikesCount, cursor.ID,
		)
	}

	// 限条数并执行查询；err 接收错误。
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回。
	return videos, nil
}

// ListByFollowing 方法：查"关注流"——当前用户关注的那些博主发的视频，按时间倒序。
//
// 参数：limit 条数；viewerAccountID 正在刷 Feed 的用户编号；
// latestBefore 时间游标（零值=第一页）；
// 返回值：视频指针切片、错误。
func (repo *FeedRepository) ListByFollowing(ctx context.Context, limit int, viewerAccountID uint, latestBefore time.Time) ([]*video.Video, error) {
	// videos 声明结果切片。
	var videos []*video.Video
	// query 基础查询：videos 表 + 发布时间倒序。
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("create_time DESC")
	// 登录用户编号有效时才过滤（游客不过滤，相当于看全站）。
	if viewerAccountID > 0 {
		// followingSubQuery 构造一个【子查询】：从 socials 关注关系表里，
		followingSubQuery := repo.db.WithContext(ctx).
			Model(&social.Social{}).
			// 只选出 vlogger_id（被关注的博主编号）这一列，
			Select("vlogger_id").
			// 条件是 follower_id = 当前用户（我关注的人）。
			Where("follower_id = ?", viewerAccountID)
		// 主查询追加：作者编号在上面那个子查询结果集合里（SQL 里是 author_id IN (SELECT ...)）。
		query = query.Where("author_id IN (?)", followingSubQuery)
	}
	// 传了时间游标。
	if !latestBefore.IsZero() {
		// 追加：只看游标时间之前的视频。   截取
		query = query.Where("create_time < ?", latestBefore)
	}
	// 限条数并执行；err 接收错误。
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回。
	return videos, nil
}

// ListByPopularity 方法：查"热度榜"，按热度值倒序；热度相同再比发布时间、再比 id。
//
// 参数：limit 条数；popularityBefore 游标热度；timeBefore 游标发布时间；
// idBefore 游标视频 id；
// 返回值：视频指针切片、错误。
func (repo *FeedRepository) ListByPopularity(ctx context.Context, limit int, popularityBefore int64, timeBefore time.Time, idBefore uint) ([]*video.Video, error) {
	// videos 声明结果切片。
	var videos []*video.Video
	// query 基础查询：三字段排序，保证同热度时顺序依然确定、可稳定翻页。
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("popularity DESC, create_time DESC, id DESC")

	// 只有当游标完整提供时才加过滤（popularity 允许为 0，所以不能用它判断有没有游标）
	// 发布时间非零值且 id 有效，才算有效游标。
	if !timeBefore.IsZero() && idBefore > 0 {
		// 三种"排在游标之后"的情况（对应三字段排序的字典序）：
		query = query.Where(
			// ① 热度比游标低；
			// ② 热度相同但发布时间更早；
			// ③ 热度、时间都相同但 id 更小。
			"(popularity < ?) OR (popularity = ? AND create_time < ?) OR (popularity = ? AND create_time = ? AND id < ?)",
			// 六个占位符依次填入。
			popularityBefore,
			popularityBefore, timeBefore,
			popularityBefore, timeBefore, idBefore,
		)
	}

	// 限条数、执行查询；err 接收错误。
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回。
	return videos, nil
}

// GetByIDs 方法：按一批视频编号一次性把视频查出来（批量查询，避免 N+1）。
//
// 参数 ids：视频编号切片；
// 返回值：视频指针切片、错误。
func (repo *FeedRepository) GetByIDs(ctx context.Context, ids []uint) ([]*video.Video, error) {
	// videos 声明结果切片（注意：空查询时返回的是非 nil 的空切片）。
	var videos []*video.Video
	// 没有编号：没必要发 SQL。
	if len(ids) == 0 {
		// 直接返回空切片、无错误。
		return videos, nil
	}
	// id IN ? 配合切片，GORM 会展开成 id IN (?, ?, ...)；err 接收错误。
	if err := repo.db.WithContext(ctx).Model(&video.Video{}).
		Where("id IN ?", ids).Find(&videos).Error; err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回。
	return videos, nil
}

// ListByTag 方法：按标签名查带这个标签的视频（需要 JOIN 两张关联表）。
//
// 参数：tagName 标签名；limit 条数；
// 返回值：视频指针切片、错误。
func (repo *FeedRepository) ListByTag(ctx context.Context, tagName string, limit int) ([]*video.Video, error) {
	// videos 声明结果切片。
	var videos []*video.Video
	// 链式拼一个三表连接查询；err 接收错误。
	err := repo.db.WithContext(ctx).Model(&video.Video{}).Table("videos").
		// JOIN 视频-标签关联表：关联表的 video_id 对上 videos.id。
		Joins("JOIN video_tags ON video_tags.video_id = videos.id").
		// JOIN 标签表：tags.id 对上关联表里的 tag_id。
		Joins("JOIN tags ON tags.id = video_tags.tag_id").
		// 过滤：标签名等于指定名字。
		Where("tags.name = ?", tagName).
		// 按视频发布时间倒序（加 videos. 前缀避免三表同名字段歧义）。
		Order("videos.create_time desc").
		// 限条数。
		Limit(limit).
		// 执行。
		Find(&videos).Error
	// 返回结果和错误。
	return videos, err
}
