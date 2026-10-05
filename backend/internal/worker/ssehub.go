// package worker：后台工作者包。本文件是 SSEHub——
// 基于 SSE（Server-Sent Events，服务器单向推送）的通知中心。
//
// 工作原理（Python 类比）：每个在线用户对应一个"频道 channel"（类似 asyncio.Queue），
// 后台有通知要发给谁，就往 TA 的频道里塞；用户的 HTTP 长连接一直挂着，
// 不断从自己的频道里取消息、写回浏览器。用户断开就删掉频道。
package worker

import (
	// encoding/json：把通知结构体转成 JSON 文本推给前端。
	"encoding/json"
	// errors：判断空请求体是不是 io.EOF。
	"errors"
	// fmt：按 SSE 文本格式输出。
	"fmt"
	// io：MarkRead 时空 body 会报 io.EOF，这种情况要放行。
	"io"
	// net/http：状态码、http.Flusher 接口。
	"net/http"
	// sync：读写锁保护 clients 这个 map。
	"sync"
	// time：30 秒发一次心跳保活。
	"time"

	// auth：SSE 用 query 参数传 token，需要自己解析 JWT。
	"feedsystem_video_go/internal/auth"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
	// gorm：查/改 notifications 表。
	"gorm.io/gorm"
)

// SSEHub 结构体：SSE 通知中心，管理所有在线用户的频道。
type SSEHub struct {
	// mu 读写锁：读多写少（推送只读 map、订阅/退订才改），用 RWMutex。
	// RLock 可多个读者同时持有；Lock 写时独占。
	mu sync.RWMutex
	// clients 在线用户表：key 是用户 ID，value 是该用户【所有】连接的频道切片
	// （同一用户可能开多个浏览器标签页，所以是切片而不是单个）。
	clients map[uint][]chan *Notification
	// db 数据库连接：查历史通知、标记已读等。
	db *gorm.DB
}

// NewSSEHub 是构造函数：创建通知中心。
//
// 参数 db：数据库连接；
// 返回值 *SSEHub：通知中心。
func NewSSEHub(db *gorm.DB) *SSEHub {
	// make 初始化空 map；装配并返回。
	return &SSEHub{clients: make(map[uint][]chan *Notification), db: db}
}

// Push 方法：给某个用户实时推送一条通知（由通知 worker 调用）。
//
// 参数：userID 接收者编号；n 通知对象指针；
// 无返回值。用户不在线就直接丢弃（通知已入库，TA 之后能查到）。
func (h *SSEHub) Push(userID uint, n *Notification) {
	// 加读锁：只是读 map，允许多个 Push 并发。
	h.mu.RLock()
	// 函数退出时释放读锁。
	defer h.mu.RUnlock()
	// chs 取出该用户的频道切片；ok 表示用户在不在线。
	chs, ok := h.clients[userID]
	// 不在线（没有这个 key）。
	if !ok {
		// 直接返回，不推送。
		return
	}
	// 遍历该用户每个连接的频道；ch 当前频道。
	for _, ch := range chs {
		// select 是 channel 的多路分支：
		select {
		// 尝试把通知塞进频道（频道有 20 个缓冲，塞得下就走这）。
		case ch <- n:
		// default：缓冲满了塞不进去——直接跳过，绝不阻塞推送方
		// （宁可丢这条实时推送，也不能把后台 worker 卡死）。
		default:
		}
	}
}

// Subscribe 方法：用户建立长连接时，给 TA 创建并登记一个频道。
//
// 参数 userID：用户编号；
// 返回值 chan *Notification：新建的频道。
func (h *SSEHub) Subscribe(userID uint) chan *Notification {
	// make 建一个带 20 个缓冲的频道（缓冲让 Push 多数时候不用阻塞）。
	ch := make(chan *Notification, 20)
	// 加写锁：要改 map，独占。
	h.mu.Lock()
	// append 把新频道加到该用户的频道切片后面（多标签页场景）。
	h.clients[userID] = append(h.clients[userID], ch)
	// 立即解锁（缩小锁的持有时间）。
	h.mu.Unlock()
	// 把频道交还给调用方使用。
	return ch
}

// Unsubscribe 方法：用户断开时，从表里删掉指定频道并关闭它。
//
// 参数：userID 用户编号；ch 要删除的频道；
// 无返回值。
func (h *SSEHub) Unsubscribe(userID uint, ch chan *Notification) {
	// 加写锁。
	h.mu.Lock()
	// 退出时释放。
	defer h.mu.Unlock()
	// chs 取该用户全部频道。
	chs := h.clients[userID]
	// 遍历找要删的那个；i 下标、c 当前频道。
	for i, c := range chs {
		// channel 可以用 == 比较：找到同一个频道。
		if c == ch {
			// append 把下标 i 之前和 i+1 之后两段拼起来，即"挖掉"第 i 个；
			// chs[:i] 和 chs[i+1:] 是切片的切片，... 把后半段展开。
			chs = append(chs[:i], chs[i+1:]...)
			// 删完后这个用户一个连接都没有了。
			if len(chs) == 0 {
				// 连 key 一起从 map 删除，避免留个空切片。
				delete(h.clients, userID)
			} else {
				// 还有别的连接：把缩短后的切片写回。
				h.clients[userID] = chs
			}
			// close 关闭频道，通知所有接收方"不会再有消息了"。
			close(c)
			// 已找到并删除，结束函数。
			return
		}
	}
}

// sseAccountID 是普通函数：从 Gin 上下文取出已认证的用户 ID。
//
// 参数 c：Gin 上下文；
// 返回值：用户编号、bool 是否有效。
func sseAccountID(c *gin.Context) (uint, bool) {
	// c.Get 取中间件存的 accountID；ok 表示存没存。
	accountID, ok := c.Get("accountID")
	if !ok {
		// 没存：返回无效。
		return 0, false
	}
	// 类型断言成 uint；userID 是断言后的值、ok 类型对不对。
	userID, ok := accountID.(uint)
	// 类型正确且编号非 0 才算有效。
	return userID, ok && userID != 0
}

// SSERequireAuth 方法：返回一个 SSE 专用的鉴权中间件。
//
// 为什么单独写？SSE 的浏览器原生 EventSource 不支持自定义请求头，
// 没法像普通接口那样在 Header 里带 JWT，所以约定把 token 放在 URL 查询参数 ?token=xxx。
//
// 返回值 gin.HandlerFunc：中间件函数。
func (h *SSEHub) SSERequireAuth() gin.HandlerFunc {
	// 返回一个闭包中间件。
	return func(c *gin.Context) {
		// token 先从 query 参数取。
		token := c.Query("token")
		// query 里没有。
		if token == "" {
			// 再尝试从 Authorization 头取（兼容用 header 的客户端）。
			token = c.GetHeader("Authorization")
			// 长度 > 7 且前 7 个字符是 "Bearer "。
			if len(token) > 7 && token[:7] == "Bearer " {
				// 切掉 "Bearer " 前缀，只留 token 本体（切片 [7:]）。
				token = token[7:]
			}
		}
		// 两种方式都没拿到 token。
		if token == "" {
			// Abort 拦截请求，返回 401。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			// 结束。
			return
		}
		// auth.ParseToken 解析并校验 JWT；claims 是载荷、err 错误。
		claims, err := auth.ParseToken(token)
		if err != nil {
			// token 无效/过期：401。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			// 结束。
			return
		}
		// 把解析出的账号 ID 存进上下文，供后续 SSEHandler 用。
		c.Set("accountID", claims.AccountID)
		// c.Next 放行到下一个处理函数。
		c.Next()
	}
}

// SSEHandler 方法：SSE 长连接的主处理函数——挂住连接，有通知就推，30 秒发一次心跳。
//
// 参数 c：Gin 上下文。
func (h *SSEHub) SSEHandler(c *gin.Context) {
	// userID 取当前认证用户；ok 有效性。
	userID, ok := sseAccountID(c)
	if !ok {
		// 无效：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		// 结束。
		return
	}

	// 下面三个响应头是 SSE 协议规定的：
	// 内容类型必须是 text/event-stream。
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	// no-cache：不让中间代理缓存这条流。
	c.Writer.Header().Set("Cache-Control", "no-cache")
	// keep-alive：保持长连接。
	c.Writer.Header().Set("Connection", "keep-alive")
	// 先写出 200 响应头，告诉浏览器连接建立。
	c.Writer.WriteHeader(http.StatusOK)

	// ch 订阅本用户的频道。
	ch := h.Subscribe(userID)
	// defer：函数退出（连接结束）时一定退订、关闭频道。
	defer h.Unsubscribe(userID, ch)

	// ctx 取 HTTP 请求自带的上下文，浏览器断开时它会被取消。
	ctx := c.Request.Context()
	// 类型断言成 http.Flusher：必须能"立即把数据冲出去"，否则消息会憋在缓冲区；
	// _ 忽略 ok（下面用前判空）。
	flusher, _ := c.Writer.(http.Flusher)

	// 无限循环，直到连接断开。
	for {
		// select 同时等三件事：
		select {
		// ① 浏览器断开/请求取消：ctx.Done() 这个频道会有信号。
		case <-ctx.Done():
			// 结束函数（defer 会负责退订）。
			return
		// ② 自己的频道来了通知；n 是通知、ok 表示频道是否还开着。
		case n, ok := <-ch:
			// 频道已被关闭。
			if !ok {
				// 结束。
				return
			}
			// json.Marshal 把通知转成 JSON；b 字节、_ 忽略错误。
			b, _ := json.Marshal(n)
			// SSE 协议格式：data: <内容>\n\n，写到响应体。
			fmt.Fprintf(c.Writer, "data: %s\n\n", b)
			// 拿到了 flusher。
			if flusher != nil {
				// Flush 立即把数据推给浏览器，不等缓冲攒满。
				flusher.Flush()
			}
		// ③ 30 秒定时器：没有消息也要发个心跳。
		case <-time.After(30 * time.Second):
			// SSE 里以冒号开头的是注释行，浏览器 EventSource 会忽略——用来保活，
			// 防止代理/浏览器因为长时间没数据而掐断连接。
			fmt.Fprintf(c.Writer, ": keepalive\n\n")
			// 立即冲出去。
			if flusher != nil {
				// Flush。
				flusher.Flush()
			}
		}
	}
}

// ListHandler 方法：查当前用户最近 50 条通知（历史列表）。
//
// 参数 c：Gin 上下文。
func (h *SSEHub) ListHandler(c *gin.Context) {
	// userID 认证用户；ok 有效性。
	userID, ok := sseAccountID(c)
	if !ok {
		// 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		// 结束。
		return
	}

	// notifications 声明结果切片。
	var notifications []Notification
	// 链式查询；err 接收错误。
	if err := h.db.WithContext(c.Request.Context()).
		// 只查发给当前用户的（recipient_id 隔离，看不到别人的通知）。
		Where("recipient_id = ?", userID).
		// 最新的在前。
		Order("created_at desc").
		// 最多 50 条。
		Limit(50).
		// 执行。
		Find(&notifications).Error; err != nil {
		// 查询失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 一条都没有时 GORM 给 nil。
	if notifications == nil {
		// 换成空切片，输出 []。
		notifications = []Notification{}
	}
	// 200 返回。
	c.JSON(200, gin.H{"notifications": notifications})
}

// MarkReadHandler 方法：标记通知已读。传了 id 就标单条，没传就把该用户所有通知标已读。
//
// 参数 c：Gin 上下文。
func (h *SSEHub) MarkReadHandler(c *gin.Context) {
	// userID 认证用户；ok 有效性。
	userID, ok := sseAccountID(c)
	if !ok {
		// 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		// 结束。
		return
	}

	// req 用匿名结构体接收：ID 是 *uint 指针——
	// 用指针才能区分"没传 id"（nil，全部已读）和"传了 id=0"。
	var req struct {
		// ID 通知编号指针。
		ID *uint `json:"id"`
	}
	// 绑定 JSON；空请求体会报 io.EOF，这种情况允许（当作"全部已读"）；其他错误才拒绝。
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// err 声明更新错误。
	var err error
	// 传了具体 id：只标这一条。
	if req.ID != nil {
		// WHERE id=? AND recipient_id=?（带收件人条件，防止改别人的通知——水平越权防护）；
		// Update 把 is_read 改成 true。
		err = h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("id = ? AND recipient_id = ?", *req.ID, userID).Update("is_read", true).Error
	} else {
		// 没传 id：把该用户【所有】未读通知标已读。
		err = h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ?", userID).Update("is_read", true).Error
	}
	// 更新失败。
	if err != nil {
		// 500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200。
	c.JSON(200, gin.H{"message": "ok"})
}

// UnreadCountHandler 方法：返回当前用户的未读通知数量（小红点数字）。
//
// 参数 c：Gin 上下文。
func (h *SSEHub) UnreadCountHandler(c *gin.Context) {
	// userID 认证用户；ok 有效性。
	userID, ok := sseAccountID(c)
	if !ok {
		// 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		// 结束。
		return
	}

	// count 接收计数。
	var count int64
	// COUNT WHERE 收件人是我 且 is_read=false；err 错误。
	if err := h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ? AND is_read = ?", userID, false).Count(&count).Error; err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 200 返回未读数。
	c.JSON(200, gin.H{"count": count})
}

// RegisterRoutes 方法：把通知相关的四个路由注册到指定路由组。
//
// 参数：r Gin 引擎（本函数未直接使用）；group 已挂好鉴权中间件的路由组。
func (h *SSEHub) RegisterRoutes(r *gin.Engine, group *gin.RouterGroup) {
	// GET /stream：SSE 长连接。
	group.GET("/stream", h.SSEHandler)
	// POST /list：历史通知。
	group.POST("/list", h.ListHandler)
	// POST /markRead：标记已读。
	group.POST("/markRead", h.MarkReadHandler)
	// POST /unreadCount：未读数量。
	group.POST("/unreadCount", h.UnreadCountHandler)
}

// 这是一行"编译期接口断言"：var _ 把变量名丢弃（空白标识符），
// (*SSEHub)(nil) 是一个类型为 *SSEHub 的 nil 指针；
// 如果将来 SSEHub 没实现 NotificationHub 接口（少了 Push 方法），这里编译就会报错。
var _ NotificationHub = (*SSEHub)(nil)
