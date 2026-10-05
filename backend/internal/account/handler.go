// package account：账号业务包。本文件是"处理层(Handler)"，
// 直接面向 HTTP：负责解析请求参数 → 调 AccountService → 把结果/错误转成 HTTP 响应。
//
// Handler 不写业务规则（那是 Service 的事），只做"翻译"：把 HTTP 世界的东西
// （JSON、表单、文件、URL）翻译成 Service 的方法调用，再把返回值翻译回 JSON。
package account

import (
	// crypto/rand：生成安全随机数（头像文件名）。
	"crypto/rand"
	// encoding/hex：随机字节转十六进制字符串。
	"encoding/hex"
	// errors：errors.Is 识别具体业务错误。
	"errors"
	// fmt：拼 URL、包装错误。
	"fmt"
	// net/http：HTTP 状态码。
	"net/http"
	// os：创建上传目录。
	"os"
	// path：拼 URL 形式的路径（用正斜杠）。
	"path"
	// path/filepath：拼磁盘文件路径（Windows 下会用反斜杠）。
	"path/filepath"
	// strconv：编号转字符串。
	"strconv"
	// strings：取扩展名、转小写。
	"strings"

	// apierror：统一把错误归类成 HTTP 状态码。
	"feedsystem_video_go/internal/apierror"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
	// gorm：识别"记录不存在"错误。
	"gorm.io/gorm"
)

// AccountHandler 结构体是账号处理层，持有它依赖的账号服务。
type AccountHandler struct {
	// accountService 账号服务，所有业务动作都调它。
	accountService *AccountService
}

// NewAccountHandler 是构造函数：创建账号处理器。
//
// 参数 accountService：账号服务；
// 返回值 *AccountHandler：初始化好的处理器。
func NewAccountHandler(accountService *AccountService) *AccountHandler {
	// 注入服务并返回指针。
	return &AccountHandler{accountService: accountService}
}

// CreateAccount 方法：处理"注册账号"的 HTTP 请求。
//
// 流程：解析 JSON 请求体 → 调 Service 注册 → 返回成功/错误 JSON。
//
// 参数 c：Gin 请求上下文（装着请求、也用来写响应）；无返回值（响应直接写进 c）。
func (h *AccountHandler) CreateAccount(c *gin.Context) {
	// req 声明注册请求结构体（字段和 binding 校验规则在 entity.go 里定义）。
	var req CreateAccountRequest
	// c.ShouldBindJSON(&req)：把请求体 JSON 解析进 req，并按 binding 标签做校验；
	// 必须传 &req（地址），否则没法往里填；err 接收解析/校验错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：apierror.ClassifyHTTPStatus(err) 把错误归类成对应状态码（校验错误通常 400），
		// c.JSON(状态码, 内容) 写一个 JSON 响应，gin.H 快速拼出 {"error": ...}。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// return 结束，不再往下走。
		return
	}
	// 调 Service 注册：用结构体字面量临时组一个 Account（只填用户名、密码）；
	// & 取地址传进去；err 接收错误。
	if err := h.accountService.CreateAccount(c.Request.Context(), &Account{
		// Username 来自请求 req.Username。
		Username: req.Username,
		// Password 来自请求 req.Password（Service 内部会做 bcrypt 哈希）。
		Password: req.Password,
	}); err != nil {
		// 注册失败：这里直接返回 500（实际用户名冲突也会走这，是本项目可以改进的细节）。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 注册成功：返回 200 和提示信息。
	c.JSON(200, gin.H{"message": "account created"})
}

// Rename 方法：处理"修改用户名"的 HTTP 请求（需登录）。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) Rename(c *gin.Context) {
	// req 声明改名请求结构体。
	var req RenameRequest
	// 绑定并校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回归类后的状态码和错误信息。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// accountID 从上下文取当前登录账号编号（鉴权中间件提前写入的）；err 接收错误。
	accountID, err := getAccountID(c)
	if err != nil {
		// 未登录等：返回错误（会被归类成 401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// token 调 Service 改名，拿回重签的 JWT；err 接收错误。
	token, err := h.accountService.Rename(c.Request.Context(), accountID, req.NewUsername)
	if err != nil {
		// errors.Is(err, ErrNewUsernameRequired)：没传新用户名。
		if errors.Is(err, ErrNewUsernameRequired) {
			// 返回归类状态码（400）和信息。
			c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// errors.Is(err, ErrUsernameTaken)：用户名被占用。
		if errors.Is(err, ErrUsernameTaken) {
			// 返回 409 Conflict（资源冲突）。
			c.JSON(409, gin.H{"error": err.Error()})
			// 结束。
			return
		}
		// errors.Is(err, gorm.ErrRecordNotFound)：账号不存在。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 返回 404。
			c.JSON(404, gin.H{"error": "account not found"})
			// 结束。
			return
		}
		// 其他错误：500 服务器内部错误。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回 200 和新 token（前端要用它替换旧 token）。
	c.JSON(200, gin.H{"token": token})
}

// ChangePassword 方法：处理"修改密码"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) ChangePassword(c *gin.Context) {
	// req 声明改密码请求结构体（含用户名、旧密码、新密码）。
	var req ChangePasswordRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 改密码，把请求里的三个字段传进去；err 接收错误。
	if err := h.accountService.ChangePassword(c.Request.Context(), req.Username, req.OldPassword, req.NewPassword); err != nil {
		// 旧密码不对/用户不存在等：统一回 400 和失败提示（不告诉对方具体是哪一步，避免泄露信息）。
		c.JSON(400, gin.H{"error": "unsuccessfully password changed"})
		// 结束。
		return
	}
	// 成功：返回 200 和提示（Service 内部已强制登出，前端应跳登录页）。
	c.JSON(200, gin.H{"message": "successfully password changed"})
}

// FindByID 方法：处理"按编号查账号"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) FindByID(c *gin.Context) {
	// req 声明按编号查询的请求结构体。
	var req FindByIDRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 按编号查；account 接收账号；err 接收错误。
	if account, err := h.accountService.FindByID(c.Request.Context(), req.ID); err != nil {
		// 查询失败：返回 500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	} else {
		// 成功：直接把整个 account 结构体序列化成 JSON 返回（GORM 结构体带 json 标签决定字段名）。
		c.JSON(200, account)
	}
}

// FindByUsername 方法：处理"按用户名查账号"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) FindByUsername(c *gin.Context) {
	// req 声明按用户名查询的请求结构体。
	var req FindByUsernameRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 按用户名查；account、err 接收结果。
	if account, err := h.accountService.FindByUsername(c.Request.Context(), req.Username); err != nil {
		// 查询失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	} else {
		// 成功：返回账号 JSON。
		c.JSON(200, account)
	}
}

// Login 方法：处理"登录"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) Login(c *gin.Context) {
	// req 声明登录请求结构体。
	var req LoginRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// account 先按用户名查到账号（为了响应里带上编号、用户名）；err 接收错误。
	account, err := h.accountService.FindByUsername(c.Request.Context(), req.Username)
	if err != nil {
		// 用户不存在：返回 500（本项目登录失败不细分用户/密码错误）。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// accessToken、refreshToken 调 Service 完成真正的登录校验和令牌签发；err 接收错误。
	accessToken, refreshToken, err := h.accountService.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		// 密码错误等：返回 500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回 200，用 LoginResponse 结构体装令牌和账号信息（字段 json 标签决定返回键名）。
	c.JSON(200, LoginResponse{Token: accessToken, RefreshToken: refreshToken, AccountID: account.ID, Username: account.Username})
}

// Logout 方法：处理"退出登录"的 HTTP 请求（需登录）。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) Logout(c *gin.Context) {
	// accountID 取当前登录账号编号；err 接收错误。
	accountID, err := getAccountID(c)
	if err != nil {
		// 未登录：返回归类状态码（401）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 登出（清缓存 + 清库）；err 接收错误。
	if err := h.accountService.Logout(c.Request.Context(), accountID); err != nil {
		// 失败：500。
		c.JSON(500, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回提示。
	c.JSON(200, gin.H{"message": "account logged out"})
}

// UploadAvatar 方法：处理"上传头像图片"的 HTTP 请求（multipart 表单文件上传，需登录）。
//
// 流程：取登录编号 → 取上传文件 → 校验大小/扩展名 → 建目录、生成随机文件名
// → 保存到磁盘 → 拼可访问 URL → 更新库 → 返回 URL。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) UploadAvatar(c *gin.Context) {
	// accountID 取登录账号编号；err 接收错误。
	accountID, err := getAccountID(c)
	if err != nil {
		// 未登录：直接返回 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// f 接收上传的文件对象（*multipart.FileHeader）；err 接收错误。
	// c.FormFile("file")：从表单里取字段名叫 "file" 的文件。
	f, err := c.FormFile("file")
	if err != nil {
		// 没带文件：返回 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		// 结束。
		return
	}
	// maxSize 常量：最大允许 10MB。
	// 技术点：10 << 20 是位运算，左移 20 位 = ×2^20 = 10×1048576 字节 = 10MiB。
	const maxSize = 10 << 20
	// f.Size 是文件字节数；<=0（空文件）或 > maxSize（超大）都不合法。
	if f.Size <= 0 || f.Size > maxSize {
		// 返回 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		// 结束。
		return
	}
	// ext 取文件扩展名并转小写（filepath.Ext 从文件名取后缀，比如 ".PNG"）。
	ext := strings.ToLower(filepath.Ext(f.Filename))
	// switch ext 按扩展名分支。
	switch ext {
	// 只放行这四种图片格式（多个 case 列在一起表示"命中任意一个即可"）。
	case ".jpg", ".jpeg", ".png", ".webp":
	// default：其他所有扩展名。
	default:
		// 拒绝：返回 400 和提示（校验扩展名防止传可执行文件等）。
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp allowed"})
		// 结束。
		return
	}
	// dir 拼出存盘目录：.run/uploads/avatars/{账号编号}（每个用户一个目录）。
	dir := filepath.Join(".run", "uploads", "avatars", strconv.FormatUint(uint64(accountID), 10))
	// os.MkdirAll(dir, 0o755)：递归创建目录（已存在不报错），0o755 是目录权限。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// 创建失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// filename 生成 16 字节随机十六进制名（防止用户文件名冲突/猜路径）；err 接收错误。
	filename, err := randHex(16)
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// filename 拼上原始扩展名（随机名 + 原后缀）。
	filename = filename + ext
	// absPath 拼出文件在磁盘上的完整路径。
	absPath := filepath.Join(dir, filename)
	// c.SaveUploadedFile(f, absPath)：把上传文件内容真正写到磁盘目标路径；err 接收错误。
	if err := c.SaveUploadedFile(f, absPath); err != nil {
		// 写盘失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// urlPath 拼出浏览器访问用的 URL 路径：/static/avatars/{编号}/{文件名}。
	urlPath := path.Join("/static", "avatars", strconv.FormatUint(uint64(accountID), 10), filename)
	// avatarURL 调 buildAbsoluteURL 补全成带协议、主机的完整 URL。
	avatarURL := buildAbsoluteURL(c, urlPath)
	// 调 Service 把头像 URL 更新到账号；err 接收错误。
	if err := h.accountService.UpdateAvatar(c.Request.Context(), accountID, avatarURL); err != nil {
		// 更新库失败：500（图片已存盘，但库里没记上）。
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回 200 和头像 URL。
	c.JSON(http.StatusOK, gin.H{"avatar_url": avatarURL})
}

// UpdateProfile 方法：处理"编辑个人资料（简介/头像地址）"的 HTTP 请求（需登录）。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) UpdateProfile(c *gin.Context) {
	// accountID 取登录编号；err 接收错误。
	accountID, err := getAccountID(c)
	if err != nil {
		// 未登录：401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// req 声明编辑资料请求结构体。
	var req UpdateProfileRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 调 Service 更新资料（传 req 的地址）；err 接收错误。
	if err := h.accountService.UpdateProfile(c.Request.Context(), accountID, &req); err != nil {
		// 业务错误（比如没东西可更新）：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 成功：返回提示。
	c.JSON(http.StatusOK, gin.H{"message": "profile updated"})
}

// Refresh 方法：处理"用 refresh token 换新 access token"的 HTTP 请求。
//
// 参数 c：Gin 上下文。
func (h *AccountHandler) Refresh(c *gin.Context) {
	// req 声明刷新请求结构体。
	var req RefreshRequest
	// 绑定校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错误：返回归类状态码。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// newToken、accountID、username 调 Service 换发新令牌；err 接收错误。
	newToken, accountID, username, err := h.accountService.RefreshAccessToken(c.Request.Context(), req.RefreshToken)
	if err != nil {
		// refresh token 无效/过期：统一回 401。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		// 结束。
		return
	}
	// 成功：返回 200 和新 token（RefreshToken 字段留空，刷新接口不换发 refresh token 本身）。
	c.JSON(http.StatusOK, LoginResponse{Token: newToken, AccountID: accountID, Username: username})
}

// randHex 是本文件内部使用的辅助函数：生成 n 字节随机数并转成十六进制字符串。
//
// 参数 n：需要的随机字节数；
// 返回值：string 十六进制串（长度是 2n）、error 错误。
func randHex(n int) (string, error) {
	// b 创建长度为 n 的字节切片，准备装随机字节。
	b := make([]byte, n)
	// rand.Read(b)：用密码学安全随机源填满 b；返回写入字节数和错误；
	// _ 占住字节数（确定等于 n，不需要）；err 接收错误。
	if _, err := rand.Read(b); err != nil {
		// 失败：用 %w 包装错误返回，保留底层原因。
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	// hex.EncodeToString(b)：把字节切片编码成十六进制字符串返回。
	return hex.EncodeToString(b), nil
}

// buildAbsoluteURL 辅助函数：把站内相对路径补成完整 URL（http(s)://主机/路径）。
//
// 参数：c 请求上下文（用来判断协议和主机）、p 相对路径；
// 返回值 string：完整 URL。
func buildAbsoluteURL(c *gin.Context, p string) string {
	// scheme 默认协议是 "http"。
	scheme := "http"
	// c.Request.TLS != nil：当前连接是 TLS（HTTPS）。
	if c.Request.TLS != nil {
		// 协议改为 "https"。
		scheme = "https"
	}
	// xf 取反向代理写入的 X-Forwarded-Proto 头（服务在 Nginx/网关后面时，用它识别真实协议）。
	if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" {
		// 代理头非空：以它为准。
		scheme = xf
	}
	// fmt.Sprintf 拼出 协议://主机名+路径 并返回（c.Request.Host 是请求的主机）。
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, p)
}

// getAccountID 辅助函数：从 Gin 上下文里取出鉴权中间件存好的账号编号。
//
// 技术点：中间件和 Handler 之间通过 c.Set / c.Get 传值（类似一个请求级别的字典）。
//
// 参数 c：Gin 上下文；
// 返回值：uint 账号编号、error 未找到或类型不对的错误。
func getAccountID(c *gin.Context) (uint, error) {
	// value 取出键 "accountID" 对应的任意类型值；exists 表示该键是否存在。
	value, exists := c.Get("accountID")
	// 键不存在（说明鉴权中间件没跑或没解析出账号）。
	if !exists {
		// 返回 0 和错误。
		return 0, errors.New("accountID not found")
	}
	// id, ok := value.(uint)：类型断言，把 any 类型的 value 断言成 uint；
	// ok 表示断言是否成功。
	id, ok := value.(uint)
	// 不是 uint 类型。
	if !ok {
		// 返回 0 和类型错误。
		return 0, errors.New("accountID has invalid type")
	}
	// 成功：返回编号。
	return id, nil
}
