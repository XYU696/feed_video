// package message：私信（消息）模块。
// 这个模块管用户之间一对一的私信：发私信、查看和某个人的最近聊天记录。
package message

// import 导入 time 时间包，用来表示私信的发送时间。
import "time"

// Message 是"一条私信"在程序里的样子，对应 MySQL 里 messages 表的一行。
//
// 业务理解：A 给 B 发了一句"在吗"，就产生一行；
// 查看 A 和 B 的对话，就是把"from=A,to=B"和"from=B,to=A"两个方向的消息都取出来，按时间排列。
type Message struct {
	// ID 是这条私信的唯一编号（主键）。
	ID uint `gorm:"primaryKey" json:"id"`

	// FromID 记录是谁发的私信（发送方用户编号）。
	// index:idx_message_from 普通索引（按"我发出的消息"查询时更快）；not null 不能为空。
	FromID uint `gorm:"index:idx_message_from;not null" json:"from_id"`

	// ToID 记录发给谁（接收方用户编号）。
	// index:idx_message_to 普通索引（按"我收到的消息"查询时更快）；not null 不能为空。
	ToID uint `gorm:"index:idx_message_to;not null" json:"to_id"`

	// Content 是私信的文字内容。
	// gorm:"type:text;not null" 用 text 长文本类型（内容不限长度），且不能为空。
	Content string `gorm:"type:text;not null" json:"content"`

	// IsRead 记录这条私信是否已被接收方读过。
	// gorm:"default:false" 新私信默认是"未读"状态（false）。
	// 技术点：bool 类型的"零值"（没有主动赋值时的默认值）就是 false，
	// Go 里每种类型都有零值：数字是 0、字符串是 ""、bool 是 false、指针/切片/map 是 nil。
	IsRead bool `gorm:"default:false" json:"is_read"`

	// CreatedAt 记录私信发送时间。autoCreateTime 插入时 GORM 自动填入当前时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// SendRequest 是"发送私信"接口接收的请求体。
// 发送方身份（我是谁）从 JWT 取，前端只传"发给谁、说什么"。
type SendRequest struct {
	// ToID 接收收信人的用户编号。
	ToID uint `json:"to_id"`
	// Content 接收私信内容。
	Content string `json:"content"`
}

// ListRequest 是"查看对话"接口接收的请求体。
// 告诉后端：我要看我和 PeerID（对端那个人）之间的聊天记录。
type ListRequest struct {
	// PeerID 接收"聊天对象"的用户编号（peer 的意思是"对等的另一方"）。
	PeerID uint `json:"peer_id"`
}

// ListResponse 是"对话列表"返回的数据：私信消息列表。
type ListResponse struct {
	// Messages 是私信消息的切片（列表）。
	//
	// 对比一个细节：这里写的是 []Message（直接装结构体本身，没有 *），
	// 而上一个 social 模块用的是 []*account.Account（装指针）。两种写法都合法：
	//   - []Message：列表不大、结构体字段不多时直接装值，代码更简单，不用操心指针；
	//   - []*XXX  ：列表很长、结构体很大、或需要修改元素/用 nil 时用指针。
	// 这个对话接口最多只返回最近 50 条，所以直接用 []Message 就够了。
	Messages []Message `json:"messages"`
}
