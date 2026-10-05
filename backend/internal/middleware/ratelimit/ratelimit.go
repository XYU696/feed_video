// package ratelimit：限流中间件包。
//
// 先通俗理解"限流"：要防止有人疯狂刷接口（比如 1 秒点 100 次登录、写脚本刷赞），
// 就得规定"某段时间窗口内最多允许调用 N 次"，超过的请求直接拒绝（返回 429）。
// 本项目用的是"固定时间窗口"算法：在 Redis 里对每个调用者维护一个会自动过期的计数器。
package ratelimit

import (
	// jwt：导入 JWT 中间件包，KeyByAccount 要用它的 GetAccountID 取当前登录用户编号。
	jwt "feedsystem_video_go/internal/middleware/jwt"
	// rediscache：Redis 客户端，计数器就靠它的 IncrementWithExpire 实现。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// fmt：拼接计数器 key。
	"fmt"
	// net/http：使用状态码 429（请求过多）。
	"net/http"
	// strconv：用户编号转字符串。
	"strconv"
	// strings：去除字符串空白。
	"strings"
	// time：时间窗口时长。
	"time"

	// gin：Web 框架，中间件类型和上下文都来自它。
	"github.com/gin-gonic/gin"
)

// KeyFunc 是一个"函数类型"：任何形如 func(*gin.Context) (string, bool) 的函数
// 都属于 KeyFunc 类型。
//
// 技术点——函数在 Go 里是一等公民：函数也能像普通值一样被起类型名、当参数传、当返回值。
// 这个类型的作用是"生成限流用的主体标识"：
//   - 返回的 string 是"谁在调用"（IP 或用户编号）；
//   - bool 表示能不能确定主体（确定不了就不限流、直接放行）。
type KeyFunc func(*gin.Context) (string, bool)

// Limit 是一个"中间件工厂函数"：给它限流规则，它返回一个真正的 Gin 中间件。
//
// 重点技术点——闭包(closure)：返回的匿名函数 func(c *gin.Context) 里，
// 可以直接使用外面 Limit 的参数（cache、maxRequests 等），这些参数被"捕获"进函数里，
// 每次请求时仍然有效。所以调用一次 Limit 就能"定制"出一条专门的限流中间件。
//
// 参数：
//   - cache：Redis 客户端（计数器存这里）；
//   - keyPrefix：限流用途名（比如 "account_login"，拼进 key 里区分不同接口的计数）；
//   - maxRequests：时间窗口内最多允许多少次；
//   - window：时间窗口多长（比如 1 分钟）；
//   - keyFunc：怎么取调用者标识（按 IP 或按账号）；
//
// 返回值 gin.HandlerFunc：可以挂到路由上的中间件函数。
func Limit(
	cache *rediscache.Client,
	keyPrefix string,
	maxRequests int64,
	window time.Duration,
	keyFunc KeyFunc,
) gin.HandlerFunc {
	// return 返回一个匿名函数（没有名字的函数字面量），这才是每个请求真正执行的中间件。
	return func(c *gin.Context) {
		// 先做"要不要启用限流"的兜底判断：
		// cache == nil（Redis 不可用）、keyFunc == nil（没给取标识的方法）、
		// maxRequests <= 0（上限不是正数）、window <= 0（窗口时长不合法）。
		if cache == nil || keyFunc == nil || maxRequests <= 0 || window <= 0 {
			// c.Next()：放行，继续执行后面的中间件/最终处理函数。
			c.Next()
			// return 结束本次中间件（Redis 挂了就不限流，保证业务可用——和全项目的降级思想一致）。
			return
		}
		// subject 变量接收调用者标识（IP 或用户编号字符串），ok 表示是否成功取到。
		subject, ok := keyFunc(c)
		// 取标识失败（比如按账号限流但用户未登录）。
		if !ok {
			// 直接放行（登录态相关接口拿不到账号时，不在这一层拦截）。
			c.Next()
			// 结束。
			return
		}
		// key 变量拼出计数器在 Redis 里的完整键名。
		key := buildKey(keyPrefix, subject)
		// count 变量接收当前窗口内的累计次数；err 接收错误。
		// cache.IncrementWithExpire(...)：原子地"计数 +1"，并且第一次计数时给 key 设置过期时间
		// （底层就是前面 redis 包注释过的 Lua 脚本：INCR + 首次 PEXPIRE）。
		count, err := cache.IncrementWithExpire(c.Request.Context(), key, window)
		if err != nil {
			// Redis 命令出错（不是简单的 key 不存在）：为避免误伤，放行。
			c.Next()
			// 结束。
			return
		}
		// if count > maxRequests：本次计数已经超过上限。
		if count > maxRequests {
			// c.AbortWithStatusJSON(...)：直接中止请求，不再往后执行，并回复一个 JSON 错误：
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				// http.StatusTooManyRequests = 429，表示"请求太频繁"；
				// gin.H 是 map[string]any 的简写，用来快速拼 JSON；这里 error 字段说明原因。
				"error": "too many requests",
			})
			// 结束（请求被拦截）。
			return
		}
		// 没超过上限：放行，正常处理请求。
		c.Next()
	}
}

// buildKey 函数的作用：根据"限流用途 + 调用者标识"拼出统一格式的计数器 key。
//
// 参数：keyPrefix 用途名、subject 调用者标识；
// 返回值 string：形如 feedsystem:ratelimit:account_login:127.0.0.1 的键名。
func buildKey(keyPrefix, subject string) string {
	// strings.TrimSpace(keyPrefix)：去掉用途名首尾空白，重新赋值给 keyPrefix。
	keyPrefix = strings.TrimSpace(keyPrefix)
	// 用途名为空时。
	if keyPrefix == "" {
		// 给个默认名 "default"，防止 key 里出现空段。
		keyPrefix = "default"
	}
	// fmt.Sprintf 按固定模板拼 key（同样对 subject 去空白），让所有限流键整齐好辨认。
	return fmt.Sprintf("feedsystem:ratelimit:%s:%s", keyPrefix, strings.TrimSpace(subject))
}

// KeyByIP 是 KeyFunc 类型的一个具体实现：取"客户端 IP"作为限流主体。
//
// 用途：保护注册、登录这类"没登录也能调"的接口——只能按来源 IP 计数。
//
// 参数 c：请求上下文；
// 返回值：string 客户端 IP、bool 是否成功取到非空 IP。
func KeyByIP(c *gin.Context) (string, bool) {
	// ip 变量取客户端 IP 并去掉空白。c.ClientIP() 是 Gin 提供的方法（会处理代理头，取真实来源 IP）。
	ip := strings.TrimSpace(c.ClientIP())
	// IP 为空（极少见的异常情况）。
	if ip == "" {
		// 返回空串、false（无法确定主体）。
		return "", false
	}
	// 返回 IP、true。
	return ip, true
}

// KeyByAccount 是 KeyFunc 类型的另一个实现：取"当前登录用户编号"作为限流主体。
//
// 用途：保护点赞、评论、关注这类已登录的写接口——按账号计数，
// 防止登录用户用脚本刷互动（换 IP 也逃不掉，因为认的是账号）。
//
// 参数 c：请求上下文；
// 返回值：string 用户编号字符串、bool 是否取到有效编号。
func KeyByAccount(c *gin.Context) (string, bool) {
	// accountID 从上下文里取当前登录用户编号；err 接收错误（比如中间件没设置该值）。
	accountID, err := jwt.GetAccountID(c)
	// 出错或编号为 0（无效）。
	if err != nil || accountID == 0 {
		// 返回空串、false。
		return "", false
	}
	// uint64(accountID) 先把 uint 转成 uint64；strconv.FormatUint(..., 10) 再按十进制转成字符串，
	// 返回它和 true。（计数器 key 里主体统一用字符串）
	return strconv.FormatUint(uint64(accountID), 10), true
}
