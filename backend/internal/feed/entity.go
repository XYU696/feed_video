// package feed：Feed 信息流模块。
// "Feed 流"就是 App 打开后一屏一屏刷出来的视频列表：推荐流、点赞榜、热榜、关注流、话题流。
// 本文件定义 Feed 列表统一长什么样，以及各种流的请求体、响应体。
package feed

// import 导入 time 时间包：这里主要用来表示"某一时刻"(time.Time)。
import "time"

// FeedAuthor 是"视频作者"在 Feed 列表里的精简信息。
//
// 设计理解：Feed 列表里展示一个作者只需要"编号+名字"（点头像进主页用编号，显示名字用用户名），
// 不需要头像、简介等一大堆资料，所以单独做一个精简结构体，列表数据更小、传得更快。
type FeedAuthor struct {
	// ID 是作者的用户编号。
	ID uint `json:"id"`
	// Username 是作者的用户名。
	Username string `json:"username"`
}

// FeedVideoItem 是"Feed 列表里的一个视频条目"，也就是前端每刷出来的一个视频卡片所需要的全部信息。
//
// 设计理解：这是专门给 Feed 列表用的"视频展示模型"，和数据库的 video.Video 不完全一样：
//   - 作者不是一个干巴巴的 author_id 数字，而是一个带名字的 FeedAuthor 结构体（前端直接显示）；
//   - 还多了 IsLiked 字段，表示"当前正在刷视频的这个人有没有赞过它"（红心是否点亮）。
type FeedVideoItem struct {
	// ID 是视频编号。
	ID uint `json:"id"`

	// Author 是作者信息（内嵌 FeedAuthor 结构体：一个结构体作为另一个结构体的字段）。
	// 转成 JSON 时会变成 "author":{"id":1,"username":"张三"} 这样的嵌套对象。
	Author FeedAuthor `json:"author"`

	// Title 是视频标题。
	Title string `json:"title"`

	// Description 是视频描述，omitempty 表示没有描述时该字段不输出。
	Description string `json:"description,omitempty"`

	// PlayURL 是视频播放地址。
	PlayURL string `json:"play_url"`

	// CoverURL 是封面图地址。
	CoverURL string `json:"cover_url"`

	// CreateTime 是发布时间。
	// 注意类型是 int64（整数），不是 time.Time：这里存的是"Unix 时间戳"
	// （从 1970-01-01 到那个时刻经过的秒数，一个很大的整数，比如 1780000000）。
	// 用整数时间戳传输是前后端常见约定，前端拿到后自己格式化成 "2026-09-27" 等显示。
	CreateTime int64 `json:"create_time"`

	// LikesCount 是点赞数量。
	LikesCount int64 `json:"likes_count"`

	// IsLiked 表示"当前看这个列表的用户"是否已点赞该视频：true 前端就把红心点亮。
	// 匿名用户（没登录）刷 Feed 时，这个字段会全是 false。
	IsLiked bool `json:"is_liked"`
}

// ListLatestRequest 是"最新视频流"接口（/feed/listLatest）接收的请求体。
//
// 技术点——游标分页(cursor)：
// 刷视频列表不用传统的"第 1 页、第 2 页"，而是用"时间戳"当书签：
// 第一页不传时间（要最新的）；服务器返回一页视频，并告诉前端"本页最早一条的时间"(NextTime)；
// 前端想刷下一页，就把这个时间作为 LatestTime 传回来，意思是"给我比这个时间更早的视频"。
// 这样无论中间有没有新视频发布，翻页都不会重复、不会漏掉。
type ListLatestRequest struct {
	// Limit 接收"这一页想要多少条"（比如一次刷 10 条）。
	Limit int `json:"limit"`

	// LatestTime 接收游标：上一页最后一条视频的时间戳（秒）。
	// 第一页传 0 或不传，0 在业务里表示"从头要最新的"。
	LatestTime int64 `json:"latest_time"`
}

// ListLatestResponse 是"最新视频流"返回的数据。
type ListLatestResponse struct {
	// VideoList 返回这一页的视频条目列表（元素类型是 FeedVideoItem）。
	VideoList []FeedVideoItem `json:"video_list"`

	// NextTime 返回"下一页要用的游标"：本页最后一条视频的时间戳，前端刷下一页时原样传回。
	NextTime int64 `json:"next_time"`

	// HasMore 返回"后面是否还有更多视频"：true 表示还能继续往下刷。
	HasMore bool `json:"has_more"`
}

// ListLikesCountRequest 是"点赞榜"接口（/feed/listLikesCount）接收的请求体。
//
// 技术点——复合游标：点赞榜按点赞数从高到低排。但只靠点赞数定位下一页有个问题：
// 如果好几个视频点赞数相同，翻页边界可能拿不准，导致重复或漏掉。
// 所以要用"点赞数 + 视频ID"两个值一起当书签（点赞数相同时用 ID 区分先后）。
type ListLikesCountRequest struct {
	// Limit 接收每页条数。
	Limit int `json:"limit"`

	// LikesCountBefore 接收游标之一：上一页最后一条的点赞数。
	//
	// 重点新语法——*int64 指针类型字段：
	//   字段名前的 * 表示这是一个"指针"。这里用指针不是为了省内存，而是为了区分两种状态：
	//     - nil（指针为空）：前端没传这个字段（第一页）；
	//     - 指向 0：前端明确传了 0。
	//   如果用普通 int64，没传和传 0 就分不清了（零值都是 0）。
	//   所以"请求里可选、要能判断到底传没传"的字段，常用指针 + omitempty。
	LikesCountBefore *int64 `json:"likes_count_before,omitempty"`

	// IDBefore 接收游标之二：上一页最后一条视频的 ID，类型 *uint 同样为了区分"没传"和"传 0"。
	IDBefore *uint `json:"id_before,omitempty"`
}

// LikesCountCursor 是 service 层内部使用的"点赞榜游标"。
//
// 设计理解：请求里的两个 *指针（可能为 nil）经过 handler 处理后，
// 在业务内部用一个零值就有明确含义的结构体来表示更方便：第一页时 LikesCount=0、ID=0。
type LikesCountCursor struct {
	// LikesCount 表示"只要点赞数比这个值小的视频"（第一页为 0，表示不限制）。
	LikesCount int64
	// ID 表示"点赞数相同的情况下，只要 ID 比这个小的"（第一页为 0）。
	ID uint
}

// ListLikesCountResponse 是"点赞榜"返回的数据。
type ListLikesCountResponse struct {
	// VideoList 返回这一页视频。
	VideoList []FeedVideoItem `json:"video_list"`

	// NextLikesCountBefore 返回下一页游标之一：本页最后一条的点赞数（指针，末尾没有更多数据时可能为 nil）。
	NextLikesCountBefore *int64 `json:"next_likes_count_before,omitempty"`

	// NextIDBefore 返回下一页游标之二：本页最后一条的视频 ID。
	NextIDBefore *uint `json:"next_id_before,omitempty"`

	// HasMore 返回是否还有下一页。
	HasMore bool `json:"has_more"`
}

// ListByFollowingRequest 是"关注流"接口（/feed/listByFollowing）接收的请求体。
// 关注流只显示"我关注的人"发的视频，所以这个接口必须登录。
type ListByFollowingRequest struct {
	// Limit 接收每页条数。
	Limit int `json:"limit"`
	// LatestTime 接收时间游标（上一页最后一条的时间戳，第一页传 0）。
	LatestTime int64 `json:"latest_time"`
}

// ListByFollowingResponse 是"关注流"返回的数据，结构和最新流一样。
type ListByFollowingResponse struct {
	// VideoList 返回这一页视频。
	VideoList []FeedVideoItem `json:"video_list"`
	// NextTime 返回下一页时间游标。
	NextTime int64 `json:"next_time"`
	// HasMore 返回是否还有更多。
	HasMore bool `json:"has_more"`
}

// ListByPopularityRequest 是"热榜"接口（/feed/listByPopularity）接收的请求体。
//
// 技术点——快照分页：热度是实时变化的，翻页过程中如果榜单一直在变，就可能跳页/重复。
// 解决办法：第一页时服务器生成一个"快照版本号" AsOf（当前是第几分钟），
// 之后每翻一页都带着同一个 AsOf，相当于大家都在看"同一分钟拍的榜单照片"，榜单再怎么变也不影响翻页；
// Offset 表示"从这张照片的第几个位置开始取"。
type ListByPopularityRequest struct {
	// Limit 接收每页条数。
	Limit int `json:"limit"`

	// AsOf 接收快照版本号（分钟级时间戳）；第一页传 0，服务器会自己生成并在响应里返回。
	AsOf int64 `json:"as_of"` // 服务器返回的分钟时间戳；第一页传0

	// Offset 接收"从快照的第几个位置开始取"：第一页传 0，第二页传上一页响应里的 NextOffset。
	Offset int `json:"offset"` // 下一页从这里开始；第一页传0

	// LatestIDBefore 是"MySQL 兜底分页"用的游标（Redis 不可用时才用），可选所以用指针。
	LatestIDBefore *uint `json:"latest_id_before,omitempty"`

	// LatestPopularity 是 MySQL 兜底用的游标之一：上一页最后一条的热度值（可选）。
	// DB fallback 用（可选）
	LatestPopularity int64 `json:"latest_popularity"`

	// LatestBefore 是 MySQL 兜底用的游标之二：上一页最后一条的发布时间，类型是 time.Time（不是时间戳）。
	LatestBefore time.Time `json:"latest_before"`
}

// ListByPopularityResponse 是"热榜"返回的数据。
type ListByPopularityResponse struct {
	// VideoList 返回这一页视频。
	VideoList []FeedVideoItem `json:"video_list"`

	// AsOf 返回本榜单的快照版本号，前端之后每一页都必须原样带回。
	AsOf int64 `json:"as_of"`

	// NextOffset 返回"下一页从快照第几个位置开始"（一般就是当前 offset 加上本页条数）。
	NextOffset int `json:"next_offset"`

	// HasMore 返回是否还有更多。
	HasMore bool `json:"has_more"`

	// NextLatestPopularity 返回 MySQL 兜底用的下一页热度游标（走 Redis 快照分页时用不到，但一并返回备用）。
	NextLatestPopularity *int64 `json:"next_latest_popularity,omitempty"`

	// NextLatestBefore 返回 MySQL 兜底用的下一页时间游标（*time.Time 指针，可空）。
	NextLatestBefore *time.Time `json:"next_latest_before,omitempty"`

	// NextLatestIDBefore 返回 MySQL 兜底用的下一页 ID 游标。
	NextLatestIDBefore *uint `json:"next_latest_id_before,omitempty"`
}
