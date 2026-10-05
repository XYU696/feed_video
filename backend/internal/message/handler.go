// package message：私信业务包。本文件比较特别——
// Repository / Service / Handler 三层结构体和两个数据库方法、两个 HTTP 方法
// 全部挤在这一个文件里（其他模块通常拆成 repo.go / service.go / handler.go）。
package message

import (
	// context：贯穿请求的上下文对象。
	"context"
	// errors：用 errors.New 造一个简单错误。
	"errors"
	// strings：TrimSpace 去掉内容首尾空白。
	"strings"
	// time：给消息盖服务器时间戳。
	"time"

	// apierror：把错误归类成 HTTP 状态码。
	"feedsystem_video_go/internal/apierror"
	// jwt：从 Gin 上下文取当前登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"
	// net/http：HTTP 状态码常量。
	"net/http"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
	// gorm：ORM，结构体里持有 *gorm.DB。
	"gorm.io/gorm"
)

// Repository 结构体：私信仓储，持有数据库连接。
// 这是 Go 的一行写法：type Repository struct{ db *gorm.DB }
// 等价于 Python 的 class Repository:
//
//	def __init__(self, db): self.db = db
type Repository struct{ db *gorm.DB }

// Service 结构体：私信服务层，持有仓储。注意它【没有任何自己的方法】，
// 只是个中转壳，下面 Handler 直接透过它访问 repo（h.service.repo）。
type Service struct{ repo *Repository }

// Handler 结构体：私信处理器，持有服务。
type Handler struct{ service *Service }

// NewRepository 是构造函数：创建仓储。一行写法，& 是取地址返回指针。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// NewService 是构造函数：创建服务，注入仓储。
func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// NewHandler 是构造函数：创建处理器，注入服务。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// AutoMigrate 方法：让 GORM 自动建/更新 messages 表结构。
//
// 参数：r 仓储接收者（类似 Python 的 self）；ctx 请求上下文；
// 返回值 error：建表错误。
func (r *Repository) AutoMigrate(ctx context.Context) error {
	// WithContext 把 ctx 绑给本次操作（超时/取消能传到 SQL）；
	// AutoMigrate(&Message{}) 按 Message 结构体建表；返回其错误。
	return r.db.WithContext(ctx).AutoMigrate(&Message{})
}

// Send 方法（仓储层）：把一条私信写进数据库。
//
// 参数：m *Message 待发送的消息指针；
// 返回值 error：内容为空或插入失败时的错误。
func (r *Repository) Send(ctx context.Context, m *Message) error {
	// m.Content 消息正文；strings.TrimSpace 去掉首尾空格/换行后写回 m.Content。
	m.Content = strings.TrimSpace(m.Content)
	// 内容为空（全是空格也算）。
	if m.Content == "" {
		// 返回"内容必填"错误。
		return errors.New("content is required")
	}
	// m.CreatedAt 消息创建时间；time.Now() 取服务器当前时间赋值。
	// （不信任前端传的时间，时间以服务器为准。）
	m.CreatedAt = time.Now()
	// Create(m) 执行 INSERT 插入这条消息；.Error 取插入错误并返回。
	return r.db.WithContext(ctx).Create(m).Error
}

// List 方法（仓储层）：查两个用户之间的聊天记录（双向都要查出来）。
//
// 参数：userID 当前登录用户编号；peerID 聊天对方编号；limit 最多返回条数；
// 返回值：[]Message 消息切片、error 错误。
func (r *Repository) List(ctx context.Context, userID, peerID uint, limit int) ([]Message, error) {
	// msgs 声明消息切片，用来装查询结果。
	var msgs []Message
	// 用链式调用拼 SQL 查询；err 接收错误。
	err := r.db.WithContext(ctx).
		// Where 条件：(我发给他) 或 (他发给我)，两个方向都算同一段对话。
		// 四个 ? 占位符依次填入 userID、peerID、peerID、userID。
		Where("(from_id = ? AND to_id = ?) OR (from_id = ? AND to_id = ?)", userID, peerID, peerID, userID).
		// 按创建时间【倒序】：最新的消息排最前。
		Order("created_at desc").
		// 限制条数。
		Limit(limit).
		// Find 执行查询，结果写进 msgs。
		Find(&msgs).Error
	// 返回消息列表和错误。
	return msgs, err
}

// Send 方法（Handler 层）：处理"发送私信"的 HTTP 请求。
//
// 参数 c：Gin 上下文（类似 Python Flask 里的 request 对象 + 响应工具合体）。
func (h *Handler) Send(c *gin.Context) {
	// fromID 发件人编号，从 JWT 里取当前登录账号；err 接收错误。
	fromID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：返回 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// return 结束本次请求。
		return
	}
	// req 声明请求体结构体（收件人、正文）。
	var req SendRequest
	// ShouldBindJSON 把请求 JSON 解析进 req；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// JSON 格式错：返回 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 收件人为 0，或正文去掉空格后为空：参数不完整。
	if req.ToID == 0 || strings.TrimSpace(req.Content) == "" {
		// 返回 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_id and content are required"})
		// 结束。
		return
	}
	// m 组装要入库的消息对象（指针）：
	m := &Message{FromID: fromID, ToID: req.ToID, Content: req.Content}
	// 发件人来自 JWT、收件人和正文来自请求体。
	// 调仓储写库（这里直接 h.service.repo 跨层访问，见文件尾说明）；err 接收错误。
	if err := h.service.repo.Send(c.Request.Context(), m); err != nil {
		// 失败：用 apierror 归类状态码返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200，把入库后的完整消息（含 CreatedAt、ID）返回给前端。
	c.JSON(http.StatusOK, m)
}

// List 方法（Handler 层）：处理"查看与某人的聊天记录"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *Handler) List(c *gin.Context) {
	// userID 当前登录用户编号，从 JWT 取；err 接收错误。
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// req 声明请求体（要查和谁的对话）。
	var req ListRequest
	// 解析 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 格式错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 没指定聊天对方编号。
	if req.PeerID == 0 {
		// 返回 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "peer_id is required"})
		// 结束。
		return
	}
	// msgs 调仓储查对话记录，固定最多 50 条；err 接收错误。
	msgs, err := h.service.repo.List(c.Request.Context(), userID, req.PeerID, 50)
	if err != nil {
		// 查询失败：归类状态码返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// msgs == nil（一条记录都没有时 GORM 会给 nil）。
	if msgs == nil {
		// 换成空切片，保证前端收到的是 [] 而不是 null。
		msgs = []Message{}
	}
	// 200 返回聊天记录列表。
	c.JSON(http.StatusOK, ListResponse{Messages: msgs})
}
