// package video：视频模块（这个文件定义点赞表）。
package video

// import 导入 time 时间包，用来表示点赞时间。
import "time"

// Like 是"一条点赞记录"在程序里的样子，同时也是 MySQL 里 likes 表的一行。

//告诉 Go 程序，MySQL 的 likes 表里一行数据长什么样。
// 一条点赞 = 一个 Like 结构体。Go 操作数据库时，就是在搬这些结构体。

// 业务理解：用户每给一个视频点一次赞，就会在这张表里产生一行。
// 表里只要存在 (某用户, 某视频) 这一行，就表示"这个用户赞过这个视频"；
// 取消点赞就是把这一行删掉。
type Like struct {
	// ID 是这条点赞记录的唯一编号（主键）。
	ID uint `gorm:"primaryKey" json:"id"`

	// VideoID 记录被点赞的是哪个视频。
	//
	// 重点技术点——联合唯一索引：
	//   gorm:"uniqueIndex:idx_like_video_account;not null"
	//   - uniqueIndex 表示"唯一索引"：这一列的值在整张表里不许重复；
	//   - 后面的 idx_like_video_account 是这个索引的【名字】。
	//   注意下面 AccountID 字段用的是【同一个索引名】，意思是这两列合起来组成一个"联合唯一索引"。
	//
	// 联合唯一索引的含义：(video_id, account_id) 这一对组合在整张表里只能出现一次。
	// 也就是说【同一个用户对同一个视频最多只能有一条点赞记录】，从数据库底层杜绝了重复点赞。即哪一个用户点赞了哪一个视频
	// 这正是异步消费时保证"幂等"的关键：同一条点赞消息哪怕因为重试被处理两次，
	// 第二次插入会因为违反唯一索引而失败（MySQL 报 1062 错误），点赞数就不会被多加。
	// not null 表示这一列不能为空。
	VideoID uint `gorm:"uniqueIndex:idx_like_video_account;not null" json:"video_id"`

	// AccountID 记录点赞的是哪个用户，和上面的 VideoID 共用同一个唯一索引名，
	// 两列一起构成 (video_id, account_id) 联合唯一约束。
	AccountID uint `gorm:"uniqueIndex:idx_like_video_account;not null" json:"account_id"`

	// CreatedAt 记录点赞发生的时间。
	// 这个字段没有 autoCreateTime 标签，所以代码里会手动用 time.Now() 赋值（在 service/worker 里能看到）。
	CreatedAt time.Time `json:"created_at"`
}

// LikeRequest 是"点赞/取消点赞/判断是否已赞"接口接收的请求体。
// 因为账号信息（我是谁）是从 JWT token 里解析出来的，所以前端请求里只需要告诉我"对哪个视频操作"。
type LikeRequest struct {
	// VideoID 接收要操作的视频编号（点赞、取消、查询是否已赞都用它）。
	VideoID uint `json:"video_id"`
}
