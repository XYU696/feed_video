// package video：视频模块。
// 这个包是整个项目最大的包，视频、点赞、评论、话题标签、分片上传都放在这里。
// 本文件只定义两张表：Video（视频表）和 OutboxMsg（发件箱表）。
package video

// import 用来导入别的包，类似 Python 的 import。
// 这里导入 time 包，是为了使用 time.Time 类型（表示时间）和 time.Now()（取当前时间）。
import "time"

// Video 是"视频"这个东西在程序里的样子，同时也是 MySQL 里 videos 表的一行。
// 一个 Video 记录 = 用户发布的一条短视频（标题、视频文件地址、点赞数等）。
type Video struct {
	// ID 是视频的唯一编号（主键），每发布一个视频就自增一个。
	ID uint `gorm:"primaryKey" json:"id"`

	// AuthorID 是"作者的用户编号"，表示这个视频是谁发的。
	// gorm:"index;not null"：
	//   - index 表示给这一列建普通索引（按作者查视频时更快）；
	//   - not null 表示这一列不允许为空（视频必须有作者）。
	AuthorID uint `gorm:"index;not null" json:"author_id"`

	// Username 是作者的用户名，在这里冗余存一份。
	// 技术点："冗余字段"指把作者名字复制到视频表里，这样展示视频时就不用再去用户表查名字了（拿空间换时间）。
	// gorm:"type:varchar(255);not null" 指定列类型为最长 255 的字符串，且不能为空。
	Username string `gorm:"type:varchar(255);not null" json:"username"`

	// Title 是视频标题，发布视频时必填。
	Title string `gorm:"type:varchar(255);not null" json:"title"`

	// Description 是视频的文字描述（可以不写）。
	// omitempty 表示描述为空时，返回的 JSON 里就不出现这个字段。
	Description string `gorm:"type:varchar(255);" json:"description,omitempty"`

	// PlayURL 是视频文件的播放地址（上传视频文件后得到的网址），前端用它播放视频。
	PlayURL string `gorm:"type:varchar(255);not null" json:"play_url"`

	// CoverURL 是视频封面图的地址（视频列表里先显示封面图，点了再播放视频）。
	CoverURL string `gorm:"type:varchar(255);not null" json:"cover_url"`

	// CreateTime 是视频的发布时间。
	// gorm:"autoCreateTime"：插入记录时 GORM 自动填入当前时间，代码不用手动赋值。
	//
	// 重点技术点——复合索引：
	//   index:idx_videos_create_time,sort:desc
	//     表示加入一个名叫 idx_videos_create_time 的索引，这一列在索引里按"倒序(desc，从大到小/从新到旧)"排列，
	//     专门用来加速"按发布时间从新到旧翻页"的最新流查询。
	//   index:idx_videos_popularity_time_id,priority:2,sort:desc
	//     表示加入另一个名叫 idx_videos_popularity_time_id 的【复合索引】（多个列组成的同一个索引），
	//     这一列在该索引里排第 2 位(priority:2)，倒序。它用来加速热榜查询（先按热度排，热度相同再按时间排）。
	// 复合索引必须和下面的字段合起来看才完整。
	CreateTime time.Time `gorm:"autoCreateTime;index:idx_videos_create_time,sort:desc;index:idx_videos_popularity_time_id,priority:2,sort:desc" json:"create_time"`

	// LikesCount 是这个视频被点赞的数量（冗余计数，点赞 +1、取消 -1）。
	// column:likes_count 指定数据库列名叫 likes_count（不写的话 GORM 默认会把 LikesCount 变成 likes_count）。
	// default:0 表示新视频点赞数默认是 0。
	// index:idx_videos_likes_count_id,priority:1,sort:desc：
	//   加入复合索引 idx_videos_likes_count_id，这一列排第 1 位、倒序，
	//   配合 id 列组成 (点赞数倒序, id倒序)，用来加速"点赞榜"的游标分页。
	LikesCount int64 `gorm:"column:likes_count;not null;default:0;index:idx_videos_likes_count_id,priority:1,sort:desc" json:"likes_count"`

	// Popularity 是这个视频的"热度值"（点赞、评论都会让它增加），热榜就是按它排序的。
	// index:idx_videos_popularity_time_id,priority:1,sort:desc：
	//   这个列和上面的 CreateTime、下面（实际由主键 ID 参与）组成复合索引，
	//   排第 1 位的是热度。所以整个索引顺序是 (热度倒序, 时间倒序)，热榜查询直接走这个索引就很快。
	Popularity int64 `gorm:"column:popularity;not null;default:0;index:idx_videos_popularity_time_id,priority:1,sort:desc" json:"popularity"`
}

// PublishVideoRequest 是"发布视频"接口接收的请求体。
// 注意：发视频分两步——先上传文件拿到播放地址，再调用发布接口把这些地址和标题存成一条记录。
type PublishVideoRequest struct {
	// Title 接收视频标题。
	Title string `json:"title"`
	// Description 接收视频描述。
	Description string `json:"description"`
	// PlayURL 接收上传视频文件后返回的播放地址。
	PlayURL string `json:"play_url"`
	// CoverURL 接收上传封面后返回的封面地址。
	CoverURL string `json:"cover_url"`
}

// DeleteVideoRequest 是"删除视频"接口接收的请求体（本项目里目前没有对外暴露删除接口，结构体先定义着）。
type DeleteVideoRequest struct {
	// ID 接收要删除的视频编号。
	ID uint `json:"id"`
}

// ListByAuthorIDRequest 是"按作者查视频列表"接口接收的请求体（用户主页里展示他发过的视频）。
type ListByAuthorIDRequest struct {
	// AuthorID 接收作者的用户编号。
	AuthorID uint `json:"author_id"`
}

// GetDetailRequest 是"查询视频详情"接口接收的请求体。
type GetDetailRequest struct {
	// ID 接收要查看的视频编号。
	ID uint `json:"id"`
}

// UpdateLikesCountRequest 是用来"更新点赞数"的结构体（定义了备用，业务里主要靠数据库自增自减来改计数）。
type UpdateLikesCountRequest struct {
	// ID 表示要更新哪个视频的点赞数。
	ID uint `json:"id"`
	// LikesCount 表示要设置成的点赞数量。
	LikesCount int64 `json:"likes_count"`
}

// OutboxMsg 是"发件箱消息"表 outbox_msgs 的一行。
//
// 重点技术点——Outbox（发件箱）模式，解决"数据库写成功了，但发消息这一步丢了"的问题：
// 用户发布视频时，后端在【同一个数据库事务】里同时写 videos 表和 outbox_msgs 表（要成功一起成功）；
// 后台有一个"发件箱轮询器(OutboxPoller)"不断把这张表里的消息发给 RabbitMQ，发成功后才把这行删掉。
// 这样即使当时 MQ 挂了，消息也安安稳稳躺在数据库表里，等 MQ 恢复后一定会被发出去。
type OutboxMsg struct {
	// ID 是这条发件箱消息的唯一编号（主键）。
	ID uint `gorm:"primaryKey"`
	// VideoID 记录这条消息是关于哪个视频的（发布视频事件里主要要带上视频编号）。
	// gorm:"index" 给它建索引（按视频查找/去重时更快）。
	VideoID uint `gorm:"index"`
	// EventType 记录这是什么类型的事件（比如 "publish" 发布事件），方便以后扩展多种事件。
	EventType string `gorm:"type:varchar(50)"`
	// CreateTime 是消息产生的时间。autoCreateTime 让 GORM 在插入时自动填入。
	CreateTime time.Time `gorm:"autoCreateTime"`
	// Status 记录消息的处理状态，比如 "pending"（待发送）。
	// gorm:"type:varchar(50);index"：列最长 50 字符，并建索引——
	// 轮询器每次都要查"状态为 pending 的消息"，给状态列建索引可以让这个查询很快。
	Status string `gorm:"type:varchar(50);index"`
}
