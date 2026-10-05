// package video：视频模块（这个文件定义评论表）。
package video

// import 导入 time 时间包，用来表示评论时间。
import "time"

// Comment 是"一条评论记录"在程序里的样子，同时也是 MySQL 里 comments 表的一行。
//
// 业务理解：用户在某个视频下面发表一条评论，就在这张表里产生一行；
// 评论列表就是把某个视频的所有评论行按时间顺序取出来展示。
type Comment struct {
	// ID 是这条评论的唯一编号（主键），删除评论时靠它定位。
	ID uint `gorm:"primaryKey" json:"id"`

	// Username 是评论者的用户名（冗余存一份，展示评论"xxx：说得好"时不用再去用户表查名字）。
	// gorm:"index" 给这一列单独建普通索引（比如以后想按用户名查他发过的评论时更快）。
	Username string `gorm:"index" json:"username"`

	// VideoID 记录这条评论是在哪个视频下面发的（查"某视频的全部评论"全靠它过滤）。
	// gorm:"index" 建索引，因为评论列表是最高频的查询，WHERE video_id = ? 走索引很快。
	VideoID uint `gorm:"index" json:"video_id"`

	// AuthorID 记录发评论的用户编号（删除评论时用来判断"这条评论是不是你发的"，只有作者本人能删）。
	// gorm:"index" 建索引。
	AuthorID uint `gorm:"index" json:"author_id"`

	// Content 是评论的文字内容。
	// gorm:"type:text" 指定数据库列类型为 text（长文本，没有 255 字符的长度限制，评论可能写得比较长）。
	Content string `gorm:"type:text" json:"content"`

	// CreatedAt 是评论发布时间。
	// gorm:"autoCreateTime" 插入时 GORM 自动填入当前时间，代码不用手动赋值。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// PublishCommentRequest 是"发表评论"接口接收的请求体。
// 评论者身份同样从 JWT 里取，前端只需要传"在哪个视频下、说了什么"。
type PublishCommentRequest struct {
	// VideoID 接收要在哪个视频下发评论。
	VideoID uint `json:"video_id"`
	// Content 接收评论的文字内容。
	Content string `json:"content"`
}

// DeleteCommentRequest 是"删除评论"接口接收的请求体。
type DeleteCommentRequest struct {
	// CommentID 接收要删除的评论编号（后端还会再核对这条评论是不是当前登录用户发的）。
	CommentID uint `json:"comment_id"`
}

// GetAllCommentsRequest 是"查询某视频全部评论"接口接收的请求体。
type GetAllCommentsRequest struct {
	// VideoID 接收要查看哪个视频的评论列表。
	VideoID uint `json:"video_id"`
}
