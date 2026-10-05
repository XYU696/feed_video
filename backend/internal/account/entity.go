// package account：账号模块。
// Go 里每个目录就是一个"包(package)"，同一个目录下所有 .go 文件开头都写 package account，
// 它们可以互相直接调用；别的目录想用这里的东西，就要写 import 并通过 包名.名字 的方式访问（类似 Python 的 import module）。
package account

// Account 是"用户账号"这个东西在程序里的样子，同时也是 MySQL 里 accounts 表的一行。
//
// 技术点：
//  1. struct 结构体 = Python 里只放数据的 class，用来把一组相关字段打包在一起。
//  2. 每个字段后面反引号 `...` 包着的内容叫"标签(tag)"，它不会影响程序运行，
//     是给工具/框架看的"说明书"：
//       - gorm:"..."  给 GORM 框架（操作数据库的工具）看，告诉它这个字段对应数据库的什么列、有什么约束；
//       - json:"..."  给 encoding/json 包看，告诉它这个结构体和 JSON 互相转换时字段叫什么名字。
//  3. Go 中字段名首字母大写 = "公开的"（别的包能访问，相当于 Python 的 public）；
//     小写开头 = 私有的。结构体字段要给 GORM、JSON 用，必须大写开头。
type Account struct {
	// ID 是用户的唯一编号，相当于数据库表的主键（每一行的身份证号，自增数字 1、2、3...）。
	// tag gorm:"primaryKey" 告诉 GORM：这是主键。
	// tag json:"id" 表示转成 JSON 时这个字段叫 "id"，例如 {"id":1}。
	ID uint `gorm:"primaryKey" json:"id"`

	// Username 是用户名，用户登录时用的名字。
	// uint 是"无符号整数"（只有 0 和正数）；string 是字符串。
	// gorm:"unique" 告诉数据库：用户名不能重复，重复插入会报错（就是代码里处理的 1062 错误）。
	Username string `gorm:"unique" json:"username"`

	// Password 是用户的密码（数据库里存的是用 bcrypt 加密后的结果，不是明文）。
	// json:"-" 表示转成 JSON 返回给前端时【忽略这个字段】，防止密码泄露给浏览器。
	Password string `json:"-"`

	// Token 是该用户当前有效的"登录凭证"(access token，一串 JWT 字符串)。
	// 服务端把它存下来，是为了能主动让 token 失效：登出/改密码时把它清空，旧 token 就不能用了。
	// 同样 json:"-" 不会返回给前端——token 是登录时单独返回的，不需要每次带用户信息都返回。
	Token string `json:"-"`

	// RefreshToken 是"刷新令牌"：access token 只有 15 分钟有效期，过期后用这个长效令牌（7 天）换新的，
	// 这样用户不用频繁重新登录。
	RefreshToken string `json:"-"`

	// AvatarURL 是用户头像图片的网址（字符串），前端拿到后用 <img src="这个地址"> 显示头像。
	// gorm:"type:varchar(512)" 指定数据库里这一列最长 512 个字符（网址可能比较长）。
	// json 里的 omitempty 表示：如果这个字段是空字符串，转 JSON 时就干脆不输出这个字段。
	AvatarURL string `gorm:"type:varchar(512)" json:"avatar_url,omitempty"`

	// Bio 是个人简介，用户在自己主页写的一句话介绍自己。
	// gorm:"type:varchar(255)" 指定数据库列最长 255 个字符。
	Bio string `gorm:"type:varchar(255)" json:"bio,omitempty"`
}

// CreateAccountRequest 是"注册账号"接口接收的请求体。
// 技术点：Request 结尾的结构体专门用来【接收前端传来的 JSON】。
// 前端发 POST 请求时 body 里是 {"username":"张三","password":"123456"}，
// Gin 框架会自动把它"对号入座"装进这个结构体（这个过程叫绑定 bind）。
type CreateAccountRequest struct {
	// Username 接收前端传来的用户名。json 名叫 "username"。
	Username string `json:"username"`
	// Password 接收前端传来的明文密码，后端随后会用 bcrypt 加密再存库。
	Password string `json:"password"`
}

// RenameRequest 是"修改用户名"接口接收的请求体。
type RenameRequest struct {
	// NewUsername 接收用户想要改成的新名字。
	NewUsername string `json:"new_username"`
}

// FindByIDRequest 是"按用户 ID 查询用户"接口接收的请求体。
type FindByIDRequest struct {
	// ID 接收要查询的用户编号。
	ID uint `json:"id"`
}

// FindByIDResponse 是"按 ID 查到用户"后返回给前端的数据。
// 技术点：Response 结尾的结构体专门用来【组装要返回给前端的 JSON】。
// 注意它比 Account 少了字段——密码、token 这些敏感信息不返回，只返回能公开展示的资料。
type FindByIDResponse struct {
	// ID 返回用户编号。
	ID uint `json:"id"`
	// Username 返回用户名。
	Username string `json:"username"`
	// AvatarURL 返回头像网址；omitempty 表示没设头像时这个字段不出现。
	AvatarURL string `json:"avatar_url,omitempty"`
	// Bio 返回个人简介。
	Bio string `json:"bio,omitempty"`
}

// FindByUsernameRequest 是"按用户名查用户"接口接收的请求体。
// 前端发视频、发私信时常常只知道对方用户名，需要先用这个接口查出对方的 ID。
type FindByUsernameRequest struct {
	// Username 接收要查询的用户名。
	Username string `json:"username"`
}

// FindByUsernameResponse 是"按用户名查询"后返回的结果，只需要告诉前端"这个名字对应的 ID 是多少"。
type FindByUsernameResponse struct {
	// ID 返回查到的用户编号（前端主要拿这个去发视频/私信）。
	ID uint `json:"id"`
	// Username 返回用户名。
	Username string `json:"username"`
}

// ChangePasswordRequest 是"修改密码"接口接收的请求体。
// 改密码需要验证旧密码，防止账号被别人改走。
type ChangePasswordRequest struct {
	// Username 接收用户名（用来找到是哪个账号）。
	Username string `json:"username"`
	// OldPassword 接收旧密码，后端会校验它是否正确。
	OldPassword string `json:"old_password"`
	// NewPassword 接收新密码，校验旧密码通过后替换成它（同样会先 bcrypt 加密）。
	NewPassword string `json:"new_password"`
}

// LoginRequest 是"登录"接口接收的请求体。
type LoginRequest struct {
	// Username 接收登录用的用户名。
	Username string `json:"username"`
	// Password 接收登录用的密码，后端会和数据库里存的加密密码比对。
	Password string `json:"password"`
}

// LoginResponse 是"登录成功"后返回给前端的数据。
type LoginResponse struct {
	// Token 返回新签发的 access token，前端之后访问需要登录的接口时把它放在请求头里证明身份。
	Token string `json:"token"`
	// RefreshToken 返回刷新令牌，access token 过期后用它换新的，不用重新输账号密码。
	RefreshToken string `json:"refresh_token"`
	// AccountID 返回用户编号，前端会保存下来，发视频等场景要用。
	AccountID uint `json:"account_id"`
	// Username 返回用户名，前端用来在页面上显示"你好，xxx"。
	Username string `json:"username"`
}

// UpdateProfileRequest 是"更新个人资料"接口接收的请求体，可以改头像和简介。
type UpdateProfileRequest struct {
	// AvatarURL 接收新头像的网址（头像是先通过 /account/uploadAvatar 上传后得到网址，再调用这里更新）。
	AvatarURL string `json:"avatar_url"`
	// Bio 接收新的个人简介。
	Bio string `json:"bio"`
}

// RefreshRequest 是"刷新 token"接口接收的请求体。
type RefreshRequest struct {
	// RefreshToken 接收登录时拿到的刷新令牌，后端验证它有效后发一个新的 access token。
	RefreshToken string `json:"refresh_token"`
}

// GetProfileRequest 是"获取用户主页信息"接口接收的请求体。
type GetProfileRequest struct {
	// AccountID 接收要查看主页的用户编号。
	AccountID uint `json:"account_id"`
}

// GetProfileResponse 是"用户主页"返回的数据：除了用户基本资料，还有一组统计数字
// （页面上常见的"作品 N 个 / 获赞 N / 粉丝 N / 关注 N"）。
type GetProfileResponse struct {
	// Account 内嵌一个用户公开资料结构体。
	// 技术点：把一个结构体放在另一个结构体里（字段名就是类型名）叫"嵌套/内嵌"，
	// 转成 JSON 时 Account 里面的字段会被"摊平"输出 id、username 等，相当于把两份数据合在一个 JSON 里。
	Account FindByIDResponse `json:"account"`
	// VideoCount 返回该用户发布了多少个视频。
	VideoCount int64 `json:"video_count"`
	// TotalLikes 返回该用户所有视频一共收到多少个赞。
	TotalLikes int64 `json:"total_likes"`
	// FollowerCount 返回粉丝数（有多少人关注了他）。
	FollowerCount int64 `json:"follower_count"`
	// VloggerCount 返回关注数（他关注了多少人）。
	VloggerCount int64 `json:"vlogger_count"`
}
