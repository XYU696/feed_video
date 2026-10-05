// package video：视频业务包。本文件是点赞仓储层，
// 封装对 likes 表的增删查，以及"我赞过的视频"这种跨表查询。
package video

import (
	// context：请求上下文。
	"context"
	// errors：errors.As 识别 MySQL 错误。
	"errors"

	// mysql：MySQL 驱动错误类型，用来识别 1062 唯一键冲突。
	"github.com/go-sql-driver/mysql"
	// gorm：ORM。
	"gorm.io/gorm"
)

// LikeRepository 结构体是点赞仓储，持有数据库连接。
type LikeRepository struct {
	// db 数据库连接（包私有）。
	db *gorm.DB
}

// NewLikeRepository 是构造函数：创建点赞仓储。
//
// 参数 db：数据库连接；
// 返回值 *LikeRepository：仓储。
func NewLikeRepository(db *gorm.DB) *LikeRepository {
	// 初始化并返回指针。
	return &LikeRepository{db: db}
}

// Like 方法：向 likes 表插入一条点赞记录。
//
// 参数 like：点赞对象（含视频编号、账号编号）；
// 返回值 error：错误（重复点赞会触发唯一索引 1062 错误）。
func (r *LikeRepository) Like(ctx context.Context, like *Like) error {
	// .Create(like) 执行 INSERT，直接返回其 Error。
	return r.db.WithContext(ctx).Create(like).Error
}

// Unlike 方法：删除"某用户对某视频"的点赞记录。
//
// 参数 like：用里面的 VideoID、AccountID 定位要删的行；
// 返回值 error：错误。
func (r *LikeRepository) Unlike(ctx context.Context, like *Like) error {
	// 链式删除：
	return r.db.WithContext(ctx).
		// Where 两个条件同时满足：video_id 和 account_id 都对得上（? 占位符防注入）。
		Where("video_id = ? AND account_id = ?", like.VideoID, like.AccountID).
		// Delete(&Like{}) 执行 DELETE。
		Delete(&Like{}).Error
}

// LikeIgnoreDuplicate 方法：插入点赞，但如果"已经赞过"（唯一键冲突）不当成错误。
//
// 用途：MQ 消费者可能因消息重投重复执行，用这个方法实现"重复点赞幂等"——
// 第一次真正插入返回 created=true，重复的直接 created=false、err=nil。
//
// 参数 like：点赞对象；
// 返回值：created 是否新插入了一条、err 其他数据库错误。
//
// 技术点：这里用了"命名返回值"——函数签名里直接给返回值起好名字（created、err），
// 函数体内可以直接给它们赋值，裸 return 时自动返回当前值。
func (r *LikeRepository) LikeIgnoreDuplicate(ctx context.Context, like *Like) (created bool, err error) {
	// 参数兜底：空对象、视频编号或账号编号为 0 都是非法输入。
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		// 没插入、也不算错误（直接返回命名值的当前值 false、nil）。
		return false, nil
	}
	// 执行插入，把错误赋给命名返回值 err（注意是赋值 =，不是 :=）。
	err = r.db.WithContext(ctx).Create(like).Error
	// err == nil：插入成功。
	if err == nil {
		// 返回 true、nil。
		return true, nil
	}
	// mysqlErr 准备提取 MySQL 驱动错误。
	var mysqlErr *mysql.MySQLError
	// 错误是 1062 唯一键冲突 = 已经赞过。
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		// 幂等：当作"非新建但成功"，返回 false、nil。
		return false, nil
	}
	// 其他真正的数据库错误：返回 false 和错误。
	return false, err
}

// DeleteByVideoAndAccount 方法：删除指定点赞，并告诉调用方"是否真的删到了一行"。
//
// 参数：videoID 视频编号、accountID 账号编号；
// 返回值：deleted 是否有行被删、err 错误。
func (r *LikeRepository) DeleteByVideoAndAccount(ctx context.Context, videoID, accountID uint) (deleted bool, err error) {
	// 非法编号。
	if videoID == 0 || accountID == 0 {
		// 返回 false、nil。
		return false, nil
	}
	// res 接收删除结果（含 Error、RowsAffected）。
	res := r.db.WithContext(ctx).
		// 条件定位具体那一行。
		Where("video_id = ? AND account_id = ?", videoID, accountID).
		// 执行删除。
		Delete(&Like{})
		// res.RowsAffected > 0：删到了行才是 true（重复取消点赞时为 false，实现幂等判定）；错误一并返回。
	return res.RowsAffected > 0, res.Error
}

// IsLiked 方法：判断某用户是否赞过某视频。
//
// 参数：videoID 视频编号、accountID 账号编号；
// 返回值：bool 是否赞过、error 错误。
func (r *LikeRepository) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	// count 接收计数。
	var count int64
	// 统计满足两条件的 likes 行数；err 接收错误。
	err := r.db.WithContext(ctx).Model(&Like{}).
		// 条件。
		Where("video_id = ? AND account_id = ?", videoID, accountID).
		// Count 生成 SELECT COUNT(*)。
		Count(&count).Error
	if err != nil {
		// 出错返回。
		return false, err
	}
	// count > 0：有记录就是赞过。
	return count > 0, nil
}

// BatchGetLiked 方法：一次性查"这批视频里，哪些被该用户赞过"，返回视频编号→bool 的映射。
//
// 为什么要批量？Feed 一次返回几十个视频，如果每个视频单独查一次 IsLiked 就是几十次 SQL
// （N+1 问题）；本方法用一条 IN 查询全部搞定。
//
// 参数：videoIDs 一批视频编号、accountID 当前用户；
// 返回值：map[uint]bool 视频编号→是否赞过、error 错误。
func (r *LikeRepository) BatchGetLiked(ctx context.Context, videoIDs []uint, accountID uint) (map[uint]bool, error) {
	// likeMap 创建结果映射（make 分配 map；命中的视频编号才放进去，默认全是 false）。
	likeMap := make(map[uint]bool)
	// len(videoIDs) == 0：空列表没什么可查。
	if len(videoIDs) == 0 {
		// 返回空映射。
		return likeMap, nil
	}
	// accountID == 0：游客没有点赞记录。
	if accountID == 0 {
		// 返回空映射。
		return likeMap, nil
	}
	// likes 接收查询结果。
	var likes []Like
	// 查该用户对这批视频的所有点赞记录；err 接收错误。
	err := r.db.WithContext(ctx).Model(&Like{}).
		// "video_id IN ?"：GORM 会把切片 videoIDs 展开成 IN (?,?,...)；再限定当前账号。
		Where("video_id IN ? AND account_id = ?", videoIDs, accountID).
		// 执行查询。
		Find(&likes).Error
	if err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// for range 遍历查到的点赞记录。
	for _, like := range likes {
		// 把对应视频编号在映射里标记为 true。
		likeMap[like.VideoID] = true
	}
	// 返回映射（没出现在 map 里的视频即未赞）。
	return likeMap, nil
}

// ListLikedVideos 方法：查某用户点赞过的所有视频（"我的喜欢"列表）。
//
// 技术点：这是 JOIN 跨表查询——likes 和 videos 两张表通过视频编号连起来，
// 返回的是 Video 记录，但条件和排序来自 likes。
//
// 参数 accountID：账号编号；
// 返回值：[]Video 视频切片、error 错误。
func (r *LikeRepository) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	// videos 声明结果切片。
	var videos []Video
	// accountID == 0：游客。
	if accountID == 0 {
		// 返回空切片、nil。
		return videos, nil
	}
	// 链式 JOIN 查询；err 接收错误。
	err := r.db.WithContext(ctx).
		// Model(&Video{})：最终要取 videos 表数据。
		Model(&Video{}).
		// Joins：写 JOIN 子句，把 likes 表连上来（likes.video_id = videos.id）。
		Joins("JOIN likes ON likes.video_id = videos.id").
		// 只取当前用户点赞过的。
		Where("likes.account_id = ?", accountID).
		// 按点赞时间倒序（最近赞的排前面）。
		Order("likes.created_at desc").
		// 最多 200 条。
		Limit(200).
		// 执行并填充 videos。
		Find(&videos).Error
	if err != nil {
		// 出错返回 nil。
		return nil, err
	}
	// 成功返回视频列表。
	return videos, nil
}
