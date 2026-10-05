// package video：视频业务包。本文件是视频处理器，
// 负责发布视频、上传视频文件、上传封面、删除、列表、详情等 HTTP 接口的参数解析和响应。
package video

import (
	// crypto/rand：随机文件名。
	"crypto/rand"
	// encoding/hex：随机字节转 hex。
	"encoding/hex"
	// fmt：拼路径、包装错误。
	"fmt"
	// net/http：状态码。
	"net/http"
	// os：建目录。
	"os"
	// path：拼 URL 路径。
	"path"
	// path/filepath：拼磁盘路径、取扩展名。
	"path/filepath"
	// strings：扩展名小写。
	"strings"
	// time：按日期分目录。
	"time"

	// account：账号服务类型（处理器需要它，但本文件接口实际主要用 jwt 包取值）。
	"feedsystem_video_go/internal/account"
	// apierror：错误归类、状态码映射。
	"feedsystem_video_go/internal/apierror"
	// jwt：从上下文取当前登录账号编号/用户名。
	"feedsystem_video_go/internal/middleware/jwt"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// VideoHandler 结构体是视频处理器。
type VideoHandler struct {
	// service 视频服务，发布/删除/查询都调它。
	service *VideoService
	// accountService 账号服务（构造时注入，供需要作者信息的场景使用）。
	accountService *account.AccountService
}

// NewVideoHandler 是构造函数：创建视频处理器。
//
// 参数：service 视频服务、accountService 账号服务；
// 返回值 *VideoHandler：处理器。
func NewVideoHandler(service *VideoService, accountService *account.AccountService) *VideoHandler {
	// 注入两个服务并返回指针。
	return &VideoHandler{service: service, accountService: accountService}
}

// PublishVideo 方法：处理"发布视频"请求——前端在拿到视频/封面 URL 后，
// 提交标题等元数据，本接口组装 Video 并交给 Service 落库。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) PublishVideo(c *gin.Context) {
	// req 声明发布请求结构体（含标题、简介、播放/封面地址）。
	var req PublishVideoRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// authorId 取当前登录账号编号（视频作者必须是登录者本人，不能冒充）；err 接收错误。
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录等：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// username 取当前登录用户名（冗余存进视频，列表展示时不用再连账号表）；err 接收错误。
	username, err := jwt.GetUsername(c)
	if err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// video 组装完整视频对象（指针）。
	video := &Video{
		// AuthorID 作者编号。
		AuthorID: authorId,
		// Username 作者名（冗余字段）。
		Username: username,
		// Title 标题来自请求。
		Title: req.Title,
		// Description 简介来自请求。
		Description: req.Description,
		// PlayURL 视频地址（来自上传接口返回）。
		PlayURL: req.PlayURL,
		// CoverURL 封面地址。
		CoverURL: req.CoverURL,
		// CreateTime 发布时间取服务器当前时间（以后端时间为准，不信任前端）。
		CreateTime: time.Now(),
	}
	// 调 Service 发布（事务写视频+发件箱+标签）；err 接收错误。
	if err := vh.service.Publish(c.Request.Context(), video); err != nil {
		// 发布失败：返回归类状态码和信息。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回 200 和完整视频（含回填的 ID）。
	c.JSON(200, video)
}

// UploadVideo 方法：处理"上传视频文件"请求，把 mp4 文件存到磁盘并返回可访问 URL。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) UploadVideo(c *gin.Context) {
	// authorId 取登录编号；err 接收错误。
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		// 未登录：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// f 取上传文件（字段名 "file"）；err 接收错误。
	f, err := c.FormFile("file")
	if err != nil {
		// 没带文件：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		// 结束。
		return
	}

	// maxSize：最大 200MB（200 << 20 = 200×1MiB）。
	const maxSize = 200 << 20
	// 空文件或超限。
	if f.Size <= 0 || f.Size > maxSize {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		// 结束。
		return
	}

	// ext 取扩展名转小写。
	ext := strings.ToLower(filepath.Ext(f.Filename))
	// 只允许 mp4。
	if ext != ".mp4" {
		// 非 mp4：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .mp4 is allowed"})
		// 结束。
		return
	}

	// date 取今天日期字符串（格式 20060102 是 Go 固定的"参考时间"占位写法，代表 YYYYMMDD）。
	date := time.Now().Format("20060102")
	// relDir 相对目录：videos/{作者编号}/{日期}——按人、按天分目录，避免单目录文件爆炸。
	relDir := filepath.Join("videos", fmt.Sprintf("%d", authorId), date)
	// root 上传根目录。
	root := filepath.Join(".run", "uploads")
	// absDir 磁盘绝对目录。
	absDir := filepath.Join(root, relDir)
	// MkdirAll 递归建目录；err 接收错误。
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// filename 生成 16 字节随机名；err 接收错误。
	filename, err := randHex(16)
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		// 结束。
		return
	}
	// filename 拼上 .mp4。
	filename = filename + ext
	// absPath 文件完整磁盘路径。
	absPath := filepath.Join(absDir, filename)

	// SaveUploadedFile 把文件内容写到磁盘；err 接收错误。
	if err := c.SaveUploadedFile(f, absPath); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// urlPath 浏览器访问路径 /static/videos/{作者}/{日期}/{文件名}。
	urlPath := path.Join("/static", "videos", fmt.Sprintf("%d", authorId), date, filename)

	// 200 返回两个键（url 和 play_url 同值，兼容前端字段命名）。
	c.JSON(http.StatusOK, gin.H{
		// url 完整地址。
		"url": buildAbsoluteURL(c, urlPath),
		// play_url 同地址，发布时填给视频。
		"play_url": buildAbsoluteURL(c, urlPath),
	})
}

// UploadCover 方法：处理"上传封面图片"请求，流程与上传视频几乎一致，只是类型和大小限制不同。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) UploadCover(c *gin.Context) {
	// authorId 取登录编号；err 接收错误。
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// f 取文件；err 接收错误。
	f, err := c.FormFile("file")
	if err != nil {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		// 结束。
		return
	}

	// maxSize：封面最大 10MB。
	const maxSize = 10 << 20
	// 大小不合法。
	if f.Size <= 0 || f.Size > maxSize {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		// 结束。
		return
	}

	// ext 扩展名小写。
	ext := strings.ToLower(filepath.Ext(f.Filename))
	// switch 白名单校验图片格式。
	switch ext {
	// 允许这四种。
	case ".jpg", ".jpeg", ".png", ".webp":
	// 其他格式。
	default:
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp is allowed"})
		// 结束。
		return
	}

	// date 今天日期。
	date := time.Now().Format("20060102")
	// relDir 封面相对目录 covers/{作者}/{日期}。
	relDir := filepath.Join("covers", fmt.Sprintf("%d", authorId), date)
	// root 上传根目录。
	root := filepath.Join(".run", "uploads")
	// absDir 磁盘目录。
	absDir := filepath.Join(root, relDir)
	// 建目录；err 接收错误。
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		// 500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// filename 随机名；err 接收错误。
	filename, err := randHex(16)
	if err != nil {
		// 500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		// 结束。
		return
	}
	// 拼扩展名。
	filename = filename + ext
	// absPath 完整路径。
	absPath := filepath.Join(absDir, filename)

	// 写盘；err 接收错误。
	if err := c.SaveUploadedFile(f, absPath); err != nil {
		// 500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// urlPath 访问路径。
	urlPath := path.Join("/static", "covers", fmt.Sprintf("%d", authorId), date, filename)

	// 200 返回 url 和 cover_url。
	c.JSON(http.StatusOK, gin.H{
		// url 完整地址。
		"url": buildAbsoluteURL(c, urlPath),
		// cover_url 发布时填给视频。
		"cover_url": buildAbsoluteURL(c, urlPath),
	})
}

// randHex 辅助函数：生成 n 字节随机数的十六进制字符串（与 account 包中同名函数作用一样，各包独立不冲突）。
//
// 参数 n：字节数；
// 返回值：string hex 串、error 错误。
func randHex(n int) (string, error) {
	// b 字节切片。
	b := make([]byte, n)
	// rand.Read 填充随机字节；_ 忽略长度；err 接收错误。
	if _, err := rand.Read(b); err != nil {
		// 失败：%w 包装返回。
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	// hex 编码返回。
	return hex.EncodeToString(b), nil
}

// buildAbsoluteURL 辅助函数：把相对路径补成完整 URL（同 account 包逻辑）。
//
// 参数：c 上下文、p 相对路径；
// 返回值 string：完整 URL。
func buildAbsoluteURL(c *gin.Context, p string) string {
	// scheme 默认 http。
	scheme := "http"
	// TLS 连接。
	if c.Request.TLS != nil {
		// 改 https。
		scheme = "https"
	}
	// X-Forwarded-Proto 反代头。
	if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" {
		// 以代理头为准。
		scheme = xf
	}
	// 拼完整 URL 返回。
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, p)
}

// DeleteVideo 方法：处理"删除视频"请求。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) DeleteVideo(c *gin.Context) {
	// req 声明删除请求。
	var req DeleteVideoRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// authorId 取登录编号；err 接收错误。
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 删除（Service 内部校验是不是本人）；err 接收错误。
	if err := vh.service.Delete(c.Request.Context(), req.ID, authorId); err != nil {
		// 失败：返回归类状态码（越权会被归成 401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200。
	c.JSON(200, gin.H{"message": "video deleted"})
}

// ListByAuthorID 方法：处理"查某作者视频列表"请求。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) ListByAuthorID(c *gin.Context) {
	// req 声明请求。
	var req ListByAuthorIDRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// videos 调 Service 查询；err 接收错误。
	videos, err := vh.service.ListByAuthorID(c.Request.Context(), req.AuthorID)
	if err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// videos == nil：一个视频都没有时切片是 nil。
	if videos == nil {
		// 显式换成空切片 []Video{}，让 JSON 序列化成 [] 而不是 null（前端更好处理）。
		videos = []Video{}
	}
	// 200 返回列表。
	c.JSON(200, videos)
}

// GetDetail 方法：处理"视频详情"请求。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) GetDetail(c *gin.Context) {
	// req 声明请求。
	var req GetDetailRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// video 调 Service 取详情（内部带缓存防击穿）；err 接收错误。
	video, err := vh.service.GetDetail(c.Request.Context(), req.ID)
	if err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 200 返回视频详情。
	c.JSON(200, video)
}

// UpdateLikesCount 方法：处理"直接设置点赞数"请求（多为内部/管理用途）。
//
// 参数 c：Gin 上下文。
func (vh *VideoHandler) UpdateLikesCount(c *gin.Context) {
	// req 声明请求。
	var req UpdateLikesCountRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 更新点赞数；err 接收错误。
	if err := vh.service.UpdateLikesCount(c.Request.Context(), req.ID, req.LikesCount); err != nil {
		// 出错：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：200。
	c.JSON(200, gin.H{"message": "likes count updated"})
}
