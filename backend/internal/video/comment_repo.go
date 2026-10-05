// package video：视频业务包。本文件是评论仓储层，
// 封装对 comments 表的增删查。
package video

import (
	// context：请求上下文。
	"context"

	// gorm：ORM、ErrRecordNotFound。
	"gorm.io/gorm"
)

// CommentRepository 结构体是评论仓储，持有数据库连接。
type CommentRepository struct {
	// db 数据库连接（包私有）。
	db *gorm.DB
}

// NewCommentRepository 是构造函数：创建评论仓储。
//
// 参数 db：数据库连接；
// 返回值 *CommentRepository：仓储。
func NewCommentRepository(db *gorm.DB) *CommentRepository {
	// 初始化并返回指针。
	return &CommentRepository{db: db}
}

// CreateComment 方法：向 comments 表插入一条评论。
//
// 参数 comment：评论对象；
// 返回值 error：错误。
func (r *CommentRepository) CreateComment(ctx context.Context, comment *Comment) error {
	// .Create 执行 INSERT，直接返回 Error。
	return r.db.WithContext(ctx).Create(comment).Error
}

// DeleteComment 方法：按评论对象（主键）删除一条评论。
//
// 参数 comment：要删除的评论（GORM 用它的主键定位）；
// 返回值 error：错误。
func (r *CommentRepository) DeleteComment(ctx context.Context, comment *Comment) error {
	// .Delete(comment) 执行 DELETE，返回 Error。
	return r.db.WithContext(ctx).Delete(comment).Error
}

// GetAllComments 方法：查某视频下的全部评论，按发表时间【正序】（先评论的排前面）。
//
// 参数 videoID：视频编号；
// 返回值：[]Comment 评论切片、error 错误。
func (r *CommentRepository) GetAllComments(ctx context.Context, videoID uint) ([]Comment, error) {
	// comments 声明结果切片。
	var comments []Comment
	// 链式查询；err 接收错误。
	err := r.db.WithContext(ctx).
		// 只查该视频的评论。
		Where("video_id = ?", videoID).
		// Order("created_at asc")：从早到晚（注意和视频列表的 desc 相反，评论区习惯按时间正序）。
		Order("created_at asc").
		// 最多 200 条。
		Limit(200).
		// 执行查询。
		Find(&comments).Error
	// 返回切片和错误（调用方自己判断 err）。
	return comments, err
}

// IsExist 方法：判断某条评论是否存在。
//
// 参数 id：评论编号；
// 返回值：bool 是否存在、error 其他数据库错误。
func (r *CommentRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	// comment 准备接收查询。
	var comment Comment
	// 按主键查；err 接收错误。
	if err := r.db.WithContext(ctx).First(&comment, id).Error; err != nil {
		// 注意这里用的是 err == gorm.ErrRecordNotFound 直接比较，
		// 而不是 errors.Is。对"未被包装过的原始哨兵错误"两者等价；errors.Is 更通用（能识别被 %w 包过的）。
		if err == gorm.ErrRecordNotFound {
			// 不存在：false、nil。
			return false, nil
		}
		// 其他错误：返回。
		return false, err
	}
	// 查到了：true、nil。
	return true, nil
}

// GetByID 方法：按主键查一条评论；查不到时返回 (nil, nil)。
//
// 设计细节：和 IsExist 不同，这里把"查不到"转成 nil 指针 + nil 错误，
// 调用方只需判断 comment == nil，不必处理错误。
//
// 参数 id：评论编号；
// 返回值：*Comment 评论指针（查不到为 nil）、error 错误。
func (r *CommentRepository) GetByID(ctx context.Context, id uint) (*Comment, error) {
	// comment 准备接收结果。
	var comment Comment
	// 按主键查；err 接收错误。
	if err := r.db.WithContext(ctx).First(&comment, id).Error; err != nil {
		// 是"记录不存在"。
		if err == gorm.ErrRecordNotFound {
			// 返回 nil、nil（没这条，但不是系统故障）。
			return nil, nil
		}
		// 其他错误：nil、err。
		return nil, err
	}
	// 查到：返回评论地址。
	return &comment, nil
}
