// package redis：Redis 中间件包。
// 这个包把第三方 Redis 客户端再"包一层"，提供项目专用的客户端：
// 统一的 key 前缀、加解锁、计数、以及后面两个文件里的字符串/ZSET 操作。
package redis

import (
	// context：上下文包，用来控制操作的超时/取消（Redis 操作都要带它）。
	"context"
	// crypto/rand：生成加锁用的随机令牌。
	"crypto/rand"
	// encoding/hex：随机字节转十六进制字符串。
	"encoding/hex"
	// errors：错误处理。
	"errors"
	// config：读取 Redis 配置（地址、密码、库号）。
	"feedsystem_video_go/internal/config"
	// fmt：拼接 key。
	"fmt"
	// strconv：端口整数转字符串。
	"strconv"
	// time：锁 TTL、过期时间。
	"time"

	// redis "github.com/redis/go-redis/v9"：
	// 技术点——包重命名：导入路径前面写了个 redis，是给这个包起的别名（这里恰好和默认包名相同）。
	// 之后用 redis.NewClient、redis.Nil 等都指这个第三方库。
	redis "github.com/redis/go-redis/v9"
)

// Client 是项目自定义的 Redis 客户端结构体。
//
// 设计理解：它在官方客户端外面包一层，是为了附加 keyPrefix（统一 key 前缀），
// 以及把项目常用操作封装成方法，业务代码用起来更统一。
type Client struct {
	// rdb 是底层官方 Redis 客户端（真正负责发命令的对象），小写私有，外部不能直接碰。
	rdb *redis.Client
	// keyPrefix 是统一加在所有 key 前面的前缀（比如 "v1:"），
	// 作用：同一个 Redis 可能被多个项目共用，加前缀能区分；换版本时改前缀也能隔离旧数据。
	keyPrefix string
}

// defaultKeyPrefix 常量：默认 key 前缀。
const defaultKeyPrefix = "v1:"

// NewClient 函数的作用：用一个已有的官方客户端和指定前缀，构造项目客户端。
//
// 参数：rdb 官方客户端、keyPrefix 要使用的前缀；
// 返回值 *Client：构造好的客户端指针。
func NewClient(rdb *redis.Client, keyPrefix string) *Client {
	// &Client{...} 创建客户端并返回它的地址（字段按名赋值）。
	return &Client{rdb: rdb, keyPrefix: keyPrefix}
}

// NewFromEnv 函数的作用：根据配置创建 Redis 客户端（配置来自 yaml/环境变量）。
//
// 参数 cfg：Redis 连接配置指针；
// 返回值：*Client 项目客户端、error（当前实现总是返回 nil 错误，真正的连通性由之后 Ping 检查）。
func NewFromEnv(cfg *config.RedisConfig) (*Client, error) {
	// rdb 变量创建官方 Redis 客户端。
	// redis.NewClient(&redis.Options{...}) 按选项建立连接配置（此时还没真正发命令，连接是按需建立的）。
	rdb := redis.NewClient(&redis.Options{
		// Addr 是 Redis 地址，用"主机:端口"格式；strconv.Itoa 把端口整数转成字符串。
		Addr: cfg.Host + ":" + strconv.Itoa(cfg.Port),
		// Password 是 Redis 密码（没有密码就留空）。
		Password: cfg.Password,
		// DB 是使用第几号库。
		DB: cfg.DB,
	})
	// 用官方客户端和默认前缀构造项目客户端返回，错误为 nil。
	return &Client{rdb: rdb, keyPrefix: defaultKeyPrefix}, nil
}

// Close 是 Client 的方法：关闭 Redis 连接，释放资源。
//
// 参数 c：客户端指针；
// 返回值 error：关闭错误，成功为 nil。
func (c *Client) Close() error {
	// c == nil 客户端本身不存在，或 c.rdb == nil 底层客户端不存在。
	if c == nil || c.rdb == nil {
		// 没有可关闭的东西，返回 nil（把"关闭"当成已完成）。
		return nil
	}
	// c.rdb.Close() 关闭底层连接并返回错误。
	return c.rdb.Close()
}

// Ping 是 Client 的方法：测试和 Redis 是否连通（程序启动时用很短的超时时间调用它）。
//
// 参数：c 客户端、ctx 上下文（用来限定超时）；
// 返回值 error：连不上时返回错误，连通为 nil。
func (c *Client) Ping(ctx context.Context) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回"客户端未初始化"错误。
		return errors.New("redis client not initialized")
	}
	// c.rdb.Ping(ctx) 发出 PING 命令；.Err() 只取这条命令的错误部分（连通时为 nil）。
	return c.rdb.Ping(ctx).Err()
}

// IsMiss 是一个普通函数（不是方法）：判断错误是不是"key 不存在"。
//
// 技术点：Redis 执行 GET 等命令而 key 不存在时，客户端会返回特殊错误 redis.Nil，
// 业务代码靠 IsMiss(err) 区分"key 真没有"和"Redis 出故障了"——这两种情况要区别处理。
//
// 参数 err：命令返回的错误；
// 返回值 bool：true 表示只是 key 不存在。
func IsMiss(err error) bool {
	// err == redis.Nil 时返回 true。
	return err == redis.Nil
}

// Key 是 Client 的方法：按格式拼出一个【带统一前缀】的完整 key。
//
// 参数：
//   - format：格式模板（比如 "account:%d"）；
//   - args ...any：可变参数（any 是任意类型，类似 Python 的 *args），数量不固定，填进模板；
//
// 返回值 string：带前缀的完整 key。
func (c *Client) Key(format string, args ...any) string {
	// prefix 变量先设为空字符串。
	prefix := ""
	// 客户端存在时。
	if c != nil {
		// 取它的前缀（比如 "v1:"）。
		prefix = c.keyPrefix
	}
	// fmt.Sprintf(format, args...)：
	// 技术点：args... 里的三个点表示"把切片里的元素逐个展开"传进可变参数函数（类似 Python 的 *args 解包）。
	// 最后把前缀和拼好的 key 连起来返回。
	return prefix + fmt.Sprintf(format, args...)
}

// randToken 函数的作用：生成 n 个随机字节的十六进制字符串（加锁时当作锁的唯一标识）。
//
// 参数 n：需要多少字节随机数；
// 返回值：string 十六进制随机串、error 生成失败错误。
func randToken(n int) (string, error) {
	// b 创建长度 n 的字节切片。
	b := make([]byte, n)
	// rand.Read(b) 填充随机数，err 接收错误。
	if _, err := rand.Read(b); err != nil {
		// 失败：返回空串和错误。
		return "", err
	}
	// 成功：编码成十六进制字符串返回。
	return hex.EncodeToString(b), nil
}

// Lock 是 Client 的方法：尝试加一个"分布式锁"。
//
// 通俗理解分布式锁：多个请求/多台机器同时只想让一个去做某件事（比如重建缓存），
// 就靠 Redis 里占一个 key——谁先把 key 写成功谁就拿到锁，其他人发现 key 已存在就放弃/等待。
//
// 参数：c 客户端、ctx 上下文、key 锁的键名、ttl 锁的存活时间（防止持锁者崩溃导致锁永远不释放）；
// 返回值（这里用了"命名返回值"）：
//   - token string：锁的随机标识（解锁时要核对，防止误删别人的锁）；
//   - ok bool：是否成功拿到锁；
//   - err error：命令错误。
func (c *Client) Lock(ctx context.Context, key string, ttl time.Duration) (token string, ok bool, err error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回空 token、false（没拿到锁）、nil 错误。
		return "", false, nil
	}
	// token, err = randToken(16)：生成 16 字节随机标识（注意这里用 = 不是 :=，因为返回值 token、err 已在签名里声明）。
	token, err = randToken(16)
	if err != nil {
		// 生成失败：返回空 token、false 和错误。
		return "", false, err
	}
	// c.rdb.SetNX(ctx, key, token, ttl)：
	//   SetNX = "Set if Not eXists"，只有 key 不存在时才写入并设置过期，是天然的原子抢锁操作；
	//   .Result() 取出 bool 结果（true=成功写入=拿到锁）和错误，赋值给 ok、err。
	ok, err = c.rdb.SetNX(ctx, key, token, ttl).Result()
	// 返回锁标识、是否拿到、错误。
	return token, ok, err
}

// unlockScript 是一个预加载的 Lua 脚本对象，用来安全地释放锁。
//
// 技术点——为什么解锁要用 Lua 脚本？
// "判断 key 的值是不是我的 token"和"删除 key"如果是两条命令，中间可能被插进别的操作
// （比如我的锁刚好过期、别人已拿到新锁，我再 DEL 就会误删别人的锁）。
// Redis 执行 Lua 脚本是【原子的】（执行期间不会被其他命令打断），把判断+删除放一个脚本里就安全了。
//
// 脚本内容解释：
//
//	redis.call("GET", KEYS[1]) == ARGV[1]  —— 先取锁 key 的值，看是否等于我传入的 token；
//	  相等则 redis.call("DEL", KEYS[1])     —— 是我的锁才删除；
//	  否则 return 0                          —— 不是我的就什么都不做。
//	KEYS[1] 是脚本的第一个 key 参数，ARGV[1] 是第一个普通参数（token）。
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

// incrementWithExpireScript 是另一个 Lua 脚本：给计数器 +1，并且只在第一次（值为 1）时设置过期时间。
//
// 用途：接口限流。要统计"某用户 1 分钟内调用了多少次"，就得有个会自动过期的计数器。
// 脚本内容：
//
//	local count = redis.call("INCR", KEYS[1])  —— 计数 +1（key 不存在时 INCR 会先当 0 再 +1）；
//	if count == 1 then                          —— 如果是第一次计数（这一分钟刚开始）；
//	  redis.call("PEXPIRE", KEYS[1], ARGV[1])   —— 给 key 设置过期（PEXPIRE 单位是毫秒）；
//	end
//	return count                                —— 返回当前计数值。
//
// 技术点：INCR + 设置过期必须是原子的，否则可能出现"加了计数却没设上过期"，key 永不过期限流就永远生效了。
var incrementWithExpireScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return count
`)

// Unlock 是 Client 的方法：释放分布式锁（带 token 核对，安全删除）。
//
// 参数：c 客户端、ctx 上下文、key 锁键名、token 加锁时拿到的随机标识；
// 返回值 error：脚本执行错误，成功为 nil。
func (c *Client) Unlock(ctx context.Context, key string, token string) error {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 当作成功，直接返回 nil。
		return nil
	}
	// unlockScript.Run(ctx, c.rdb, []string{key}, token)：
	//   - []string{key} 作为脚本的 KEYS（key 列表）；
	//   - token 作为脚本的 ARGV（普通参数）；
	//   - .Result() 执行脚本并取结果；下划线丢掉返回值（这里只关心有没有出错）。
	_, err := unlockScript.Run(ctx, c.rdb, []string{key}, token).Result()
	// 返回执行错误。
	return err
}

// IncrementWithExpire 是 Client 的方法：计数器原子 +1 并设置过期（限流用）。
//
// 参数：c 客户端、ctx 上下文、key 计数键名、expire 过期时长；
// 返回值：int64 当前计数值、error 脚本错误。
func (c *Client) IncrementWithExpire(ctx context.Context, key string, expire time.Duration) (int64, error) {
	// 客户端或底层对象不存在。
	if c == nil || c.rdb == nil {
		// 返回计数 0、nil 错误（Redis 不可用时不限流，保证业务可用）。
		return 0, nil
	}
	// incrementWithExpireScript.Run(...) 执行脚本：
	return incrementWithExpireScript.Run(
		// ctx 上下文。
		ctx,
		// c.rdb 底层客户端。
		c.rdb,
		// []string{key} 脚本的 KEYS。
		[]string{key},
		// expire.Milliseconds() 把过期时长转成毫秒数，作为脚本的 ARGV。
		expire.Milliseconds(),
		// .Int64() 把脚本返回值转成 int64（当前计数）和错误，一并返回。
	).Int64()
}
