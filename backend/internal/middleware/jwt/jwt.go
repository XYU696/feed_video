// package jwt：JWT 鉴权中间件包。
// 本文件提供两种"安检严格程度"不同的中间件：
//   - JWTAuth：强校验，没带 token / token 无效 / 已被吊销 → 一律 401 拒绝（用于点赞、关注、私信等必须登录的接口）；
//   - SoftJWTAuth：软校验，没带 token 就当游客放行，带了 token 则必须有效（用于刷 Feed 等游客也能浏览的接口）。
//
// 核心知识点——JWT 为什么还要"比对"？
// JWT 一旦签发，在过期前无法主动作废。用户登出 / 改密码后旧 token 理论上仍有效，
// 所以本系统登录时会把"当前有效的 token"存一份（Redis 优先、MySQL 兜底），
// 每次请求都拿用户带来的 token 和服务器存的做比对，不一致就判定"已吊销"拒绝。
//
// Python 类比：这两个中间件相当于 Django/FastAPI 里的登录依赖（Depends），
// 区别只是一个强制登录、一个登录可选；c.Set/c.Get 类似 request.session 存取。
package jwt

import (
	// context：给 Redis 查询设置 50 毫秒超时。
	"context"
	// errors：取不到 accountID 时创建错误。
	"errors"
	// log：缓存回填失败时打日志。
	"log"
	// net/http：使用其中的状态码常量（如 http.StatusUnauthorized=401）。
	"net/http"
	// strings：按空格切分 Authorization 头、忽略大小写比较 Bearer。
	"strings"
	// time：Redis 超时、token 缓存 24 小时有效期。
	"time"

	// account：账号仓储，Redis 不可用时查 MySQL 兜底。
	"feedsystem_video_go/internal/account"
	// auth：底层 JWT 解析（ParseToken）和 Claims 结构。
	"feedsystem_video_go/internal/auth"
	// rediscache：Redis 客户端类型。
	rediscache "feedsystem_video_go/internal/middleware/redis"

	// gin：Web 框架，gin.HandlerFunc 是中间件的函数类型。
	"github.com/gin-gonic/gin"
)

// JWTAuth 函数的作用：创建一个【强校验】鉴权中间件并返回。
//
// 参数：accountRepo 账号仓储（查库兜底用）、cache Redis 客户端（可能为 nil）；
// 返回值 gin.HandlerFunc：真正处理每个请求的中间件函数。
//
// 为什么外层函数包一层？为了把 accountRepo、cache "捕获"进闭包，
// 返回的中间件在每个请求里都能用到它们（这也是依赖注入的一种形式）。
func JWTAuth(accountRepo *account.AccountRepository, cache *rediscache.Client) gin.HandlerFunc {
	// return func(c *gin.Context)：返回真正的中间件逻辑，c 是本次请求的上下文。
	return func(c *gin.Context) {
		// authHeader 取出请求头里的 Authorization（形如 "Bearer xxx"）。
		authHeader := c.GetHeader("Authorization")
		// 没带头。
		if authHeader == "" {
			// AbortWithStatusJSON：直接返回 401 并【中断】后续 Handler，请求到此为止。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			// 结束中间件。
			return
		}

		// parts 按空格切分头，SplitN 最多切 2 段 → ["Bearer", "token"]。
		parts := strings.SplitN(authHeader, " ", 2)
		// 段数不对，或第一段不是 Bearer（EqualFold 忽略大小写）。
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			// 头格式非法：401 拒绝。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			// 结束。
			return
		}

		// tokenString 取出第二段 = 真正的 JWT 字符串。
		tokenString := parts[1]

		// claims 接收解析出的票据（含 AccountID、Username）；err 接收错误。
		claims, err := auth.ParseToken(tokenString)
		// token 伪造 / 过期 / 签名不对。
		if err != nil {
			// 401 拒绝。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			// 结束。
			return
		}
		// 进入统一的"吊销校验 + 写 context"逻辑（强校验软校验成功解析后都走它）。
		check(c, claims, tokenString, accountRepo, cache)
	}
}

// SoftJWTAuth 函数的作用：创建一个【软校验】鉴权中间件。
//
// 与 JWTAuth 的唯一区别：没带 Authorization 头时不拒绝，调 c.Next() 当游客放行；
// 但只要带了头，就必须格式正确、token 有效（带了假 token 照样 401）。
//
// 参数：accountRepo 账号仓储、cache Redis 客户端；
// 返回值 gin.HandlerFunc：中间件函数。
func SoftJWTAuth(accountRepo *account.AccountRepository, cache *rediscache.Client) gin.HandlerFunc {
	// 返回中间件逻辑。
	return func(c *gin.Context) {
		// authHeader 取 Authorization 头。
		authHeader := c.GetHeader("Authorization")
		// 没带头 = 游客。
		if authHeader == "" {
			// c.Next()：不拦截，直接放行给后面的中间件/Handler（accountID 保持未设置=0）。
			c.Next()
			// 结束。
			return
		}

		// parts 切分 Authorization 头。
		parts := strings.SplitN(authHeader, " ", 2)
		// 格式不对。
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			// 主动带了头却格式错误：401 拒绝（不当游客处理）。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			// 结束。
			return
		}

		// tokenString 取出 JWT。
		tokenString := parts[1]

		// claims、err：解析 token。
		claims, err := auth.ParseToken(tokenString)
		// token 无效。
		if err != nil {
			// 主动带的 token 无效：401 拒绝。
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			// 结束。
			return
		}

		// 同样进入统一吊销校验。
		check(c, claims, tokenString, accountRepo, cache)
	}
}

// check 是包内普通函数：token 解析成功后的统一"吊销校验"。
//
// 流程：先查 Redis 里存的当前 token 做比对；Redis 查不到/故障 → 查 MySQL 兜底并回填 Redis。
// 比对一致 → c.Set 写入 accountID、username 后 c.Next() 放行；不一致 → 401"已吊销"。
//
// 参数：c 请求上下文、claims 解析出的票据、tokenString 用户本次带来的 token、
//       accountRepo 账号仓储、cache Redis 客户端；
// 无返回值（结果通过 c 上的方法体现）。
func check(c *gin.Context, claims *auth.Claims, tokenString string, accountRepo *account.AccountRepository, cache *rediscache.Client) {
	// key 拼出该账号的 token 缓存键，形如 "account:7"。
	key := cache.Key("account:%d", claims.AccountID)

	// 先查 Redis
	// cache != nil：Redis 客户端可用时才查。
	if cache != nil {
		// cacheCtx 带 50 毫秒超时的上下文（基于本次请求 context）；cancel 取消函数。
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		// defer cancel：函数退出时释放上下文资源（固定规矩）。
		defer cancel()

		// b 接收缓存里的 token 字节；err 接收错误。
		b, err := cache.GetBytes(cacheCtx, key)
		// err == nil：缓存命中。
		if err == nil {
			// 缓存的 token 和本次带来的不一致 → 服务器已换新 token，本 token 被吊销。
			if string(b) != tokenString {
				// 401 拒绝。
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
				// 结束。
				return
			}
			// 一致：把账号编号写进 context，后续 Handler 用 GetAccountID 取。
			c.Set("accountID", claims.AccountID)
			// 把用户名也写进 context。
			c.Set("username", claims.Username)
			// c.Next()：放行给后续处理。
			c.Next()
			// 结束。
			return
		}
	}

	// Redis 故障/未启用/未命中：查 DB 兜底
	// accountInfo 接收账号信息；err 接收错误。
	accountInfo, err := accountRepo.FindByID(c.Request.Context(), claims.AccountID)
	// 查询失败，或库里没存 token，或库里 token 与本次不一致。
	if err != nil || accountInfo.Token == "" || accountInfo.Token != tokenString {
		// 数据库也证明该 token 无效/已吊销：401。
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
		// 结束。
		return
	}

	// 库里校验通过，把 token 回填进 Redis，下次就不必查库。
	// cache != nil：Redis 可用时才回填。
	if cache != nil {
		// cacheCtx 带 50 毫秒超时；cancel 取消。
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		// defer 释放。
		defer cancel()

		// SetBytes 写入键值、有效期 24 小时；err 接收错误。
		if err := cache.SetBytes(cacheCtx, key, []byte(tokenString), 24*time.Hour); err != nil {
			// 回填失败只记日志，不影响本次放行。
			log.Printf("failed to set cache: %v", err)
		}
	}

	// 把账号编号写进 context。
	c.Set("accountID", claims.AccountID)
	// 把用户名写进 context。
	c.Set("username", claims.Username)
	// 放行。
	c.Next()

}

// GetAccountID 是包内普通函数：供各 Handler 从 context 取出当前登录账号编号。
//
// 参数 c：请求上下文；
// 返回值：accountID 账号编号、error 未登录或类型不对时的错误。
func GetAccountID(c *gin.Context) (uint, error) {
	// uidValue 取出 accountID 值；exists 表示中间件有没有设置过它。
	uidValue, exists := c.Get("accountID")
	// 没设置 = 未登录（游客）。
	if !exists {
		// 返回 0 和错误。
		return 0, errors.New("accountID not found")
	}

	// accountID 做类型断言：context 里存的是 uint 才成功；ok 标记是否成功。
	accountID, ok := uidValue.(uint)
	// 存了但类型不是 uint（理论上不该发生）。
	if !ok {
		// 返回类型错误。
		return 0, errors.New("accountID has invalid type")
	}

	// 返回账号编号。
	return accountID, nil
}

// GetUsername 是包内普通函数：从 context 取出当前登录用户名，结构同 GetAccountID。
//
// 参数 c：请求上下文；
// 返回值：username 用户名、error 取不到/类型错误。
func GetUsername(c *gin.Context) (string, error) {
	// val 取 username 值；exists 表示是否设置。
	val, exists := c.Get("username")
	// 没设置。
	if !exists {
		// 返回空串和错误。
		return "", errors.New("username not found")
	}

	// username 类型断言成 string；ok 标记。
	username, ok := val.(string)
	// 类型不符。
	if !ok {
		// 返回错误。
		return "", errors.New("username has invalid type")
	}

	// 返回用户名。
	return username, nil
}
