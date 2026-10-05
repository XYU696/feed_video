// package social：关注（社交）模块。
// 这个模块专门管"谁关注了谁"：关注、取关、粉丝列表、关注列表、粉丝/关注计数。
package social

// import 导入账号模块的 account 包。
//
// 重点技术点——导入路径：
// 写的不是目录名 "account"，而是完整路径 "feedsystem_video_go/internal/account"。
// 规则是：【模块名(go.mod 里 module 后面的名字 feedsystem_video_go) + 包所在的目录路径】。
// 但导入后在代码里使用时，还是用包自身的名字 account，例如 account.Account。
import "feedsystem_video_go/internal/account"

// Social 是"一条关注关系"在程序里的样子，对应 MySQL 里 socials 表的一行。
//
// 业务理解：用户 A 关注了用户 B，就产生一行；
//   - FollowerID = 粉丝（主动点关注的人，A）；
//   - VloggerID  = 被关注的人（B，vlogger 本意是视频博主，这里指被关注对象）。
// 取关就是把这一行删掉。
type Social struct {
	// ID 是这条关注关系的唯一编号（主键）。
	ID uint `gorm:"primaryKey"`

	// FollowerID 记录粉丝（关注者）的用户编号。
	// gorm 标签里同时出现了两个索引：
	//   - index:idx_social_follower
	//       普通索引，加速"查我关注了哪些人"（WHERE follower_id = ?）。
	//   - uniqueIndex:idx_social_follower_vlogger
	//       和下面 VloggerID 字段【同名】，组成 (粉丝, 被关注者) 联合唯一索引：
	//       同一个人对同一个人最多只能关注一次，防止重复关注（和点赞表的设计是同一个套路）。
	// not null 表示不能为空。
	FollowerID uint `gorm:"not null;index:idx_social_follower;uniqueIndex:idx_social_follower_vlogger"`

	// VloggerID 记录被关注者的用户编号。
	//   - index:idx_social_vlogger 普通索引，加速"查我的粉丝有哪些人"（WHERE vlogger_id = ?）；
	//   - uniqueIndex:idx_social_follower_vlogger 与 FollowerID 同名，共同构成联合唯一约束。
	VloggerID uint `gorm:"not null;index:idx_social_vlogger;uniqueIndex:idx_social_follower_vlogger"`
}

// FollowRequest 是"关注"接口接收的请求体。
// 粉丝身份（我是谁）从 JWT 取，前端只需要传"要关注谁"。
type FollowRequest struct {
	// VloggerID 接收要关注的人的用户编号。
	VloggerID uint `json:"vlogger_id"`
}

// UnfollowRequest 是"取关"接口接收的请求体。
type UnfollowRequest struct {
	// VloggerID 接收要取消关注的人的用户编号。
	VloggerID uint `json:"vlogger_id"`
}

// GetAllFollowersRequest 是"查询粉丝列表"接口接收的请求体。
// VloggerID 可以不传（传 0），后端默认查"当前登录用户"的粉丝。
type GetAllFollowersRequest struct {
	// VloggerID 接收要查谁的粉丝；为 0 时表示查我自己的。
	VloggerID uint `json:"vlogger_id"`
}

// GetAllFollowersResponse 是"粉丝列表"返回的数据：粉丝账号列表 + 粉丝总数。
type GetAllFollowersResponse struct {
	// Followers 是粉丝账号的列表。
	//
	// 重点技术点——[]*account.Account：
	//   方括号 [] 表示切片（列表）；中间的 * 表示每一个元素都是【指针】，指向一个 account.Account 结构体。
	//   合起来就是"Account 指针的列表"，类似 Python 的 list[Account]。
	//   为什么用指针而不是直接放 Account？
	//     ① 结构体直接放进切片会复制一整份，用指针只存地址，省内存（列表可能很长）；
	//     ② 指针列表里可以用 nil 表示空，而且修改元素时改的是原对象。
	//   初学阶段记住：Go 里"结构体列表"很常见的写法就是 []*结构体名。
	// json:"followers" 返回字段名叫 "followers"。
	Followers []*account.Account `json:"followers"`

	// FollowerCount 返回粉丝总人数。
	FollowerCount int64 `json:"follower_count"`
}

// GetAllVloggersResponse 是"关注列表"返回的数据：我关注的人列表 + 关注总数。
type GetAllVloggersResponse struct {
	// Vloggers 是我关注的人的账号列表（同样是 Account 指针切片 []*account.Account）。
	Vloggers []*account.Account `json:"vloggers"`

	// VloggerCount 返回我一共关注了多少人。
	VloggerCount int64 `json:"vlogger_count"`
}

// SocialCounts 是"粉丝数/关注数"计数接口返回的数据（页面头像旁边显示的两个数字）。
type SocialCounts struct {
	// FollowerCount 返回粉丝数。
	FollowerCount int64 `json:"follower_count"`
	// VloggerCount 返回关注数。
	VloggerCount int64 `json:"vlogger_count"`
}

// GetAllVloggersRequest 是"查询关注列表"接口接收的请求体。
// FollowerID 可以不传（传 0），后端默认查"当前登录用户"关注了谁。
type GetAllVloggersRequest struct {
	// FollowerID 接收要查谁的关注列表；为 0 时表示查我自己的。
	FollowerID uint `json:"follower_id"`
}
