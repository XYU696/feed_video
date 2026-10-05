// internal/auth/jwt.go
// package auth：鉴权包，专门负责 JWT 令牌的生成和解析。
//
// 先通俗理解 JWT：它是登录成功后服务器发给用户的一张"电子通行证"，
// 是一串用【密钥签名】的字符串，里面写着"你是哪个用户、什么时候过期"。
// 用户之后访问需要登录的接口时带上它，服务器不用查库，验签通过就知道你是谁。
package auth

import (
	// crypto/rand：密码学安全的随机数生成包（生成不可预测的随机字节）。
	"crypto/rand"
	// encoding/hex：十六进制编码包（把随机字节转成一串十六进制字符）。
	"encoding/hex"
	// errors：错误处理包。
	"errors"
	// log：日志包，打印警告信息。
	"log"
	// os：读取环境变量 JWT_SECRET。
	"os"
	// time：设置令牌过期时间。
	"time"

	// github.com/golang-jwt/jwt/v5：第三方 JWT 库（v5 是版本）。
	"github.com/golang-jwt/jwt/v5"
)

// cachedSecret 是包级变量，缓存"JWT 签名密钥"（字节切片 []byte），
// 初始为 nil。
//
// 技术点：为什么要缓存？环境变量只需要读一次，缓存起来避免每次签发/解析令牌都去读环境变量；
// 更重要的是——如果环境变量没设置，程序会生成一个随机密钥，必须在整个进程生命周期里
// 一直用同一个随机密钥（否则前后生成的令牌互相验不过）。
var cachedSecret []byte

// jwtSecret 函数的作用：获取用来签名的密钥（返回字节切片）。
//
// 逻辑：优先返回已缓存的密钥；没有缓存就读环境变量 JWT_SECRET；
// 环境变量也没设置，就临时生成一个随机密钥（并打印警告：进程一重启所有旧令牌都会失效）。
// 没有参数；返回值 []byte：密钥内容。
func jwtSecret() []byte {
	// if cachedSecret != nil：已经缓存过密钥（字节切片非空）。
	if cachedSecret != nil {
		// 直接返回缓存的密钥，不用重复读取/生成。
		return cachedSecret
	}
	// secret 变量读取环境变量 JWT_SECRET（生产环境应该设置它）。
	secret := os.Getenv("JWT_SECRET")
	// if secret == ""：环境变量没有设置（空字符串）。
	if secret == "" {
		// b 变量创建一个长度 32 的字节切片（make 用来分配切片，32 字节 = 256 位，足够安全）。
		b := make([]byte, 32)
		// rand.Read(b)：用密码学随机数填满 b；下划线 _ 丢掉"读到的字节数"（这里必然等于 len(b)），err 接收错误。
		if _, err := rand.Read(b); err != nil {
			// 连随机数都生成失败（极罕见）：打印致命级日志，说明原因。
			log.Printf("FATAL: cannot generate JWT secret: %v", err)
			// 兜底使用一个写死的不安全密钥（保证程序还能跑，但绝不该在生产出现）。
			cachedSecret = []byte("fallback-unsafe-key-change-me")
			// 返回这个兜底密钥。
			return cachedSecret
		}
		// hex.EncodeToString(b)：把 32 个随机字节编码成 64 个十六进制字符，赋值给 secret。
		secret = hex.EncodeToString(b)
		// 打印警告：用随机密钥意味着进程重启后所有已签发令牌全部失效，用户需要重新登录。
		log.Printf("WARNING: JWT_SECRET not set, generated random key. All tokens invalid on restart.")
	}
	// []byte(secret) 把字符串密钥转成字节切片，存入缓存 cachedSecret。
	cachedSecret = []byte(secret)
	// 返回密钥。
	return cachedSecret
}

// Claims 是"令牌里装的内容"（声明信息）。
//
// 业务理解：每一张 JWT 里主要就装两件事——用户编号 AccountID 和用户名 Username，
// 服务器验签后直接从这里读出"你是谁"，不用查数据库。
type Claims struct {
	// AccountID 是用户编号（令牌 JSON 里叫 account_id）。
	AccountID uint `json:"account_id"`
	// Username 是用户名。
	Username string `json:"username"`

	// jwt.RegisteredClaims 是 JWT 库提供的"标准登记声明"结构体（内嵌进来），
	// 里面装过期时间 ExpiresAt、签发时间 IssuedAt 等标准字段。
	jwt.RegisteredClaims
}

// GenerateToken 函数的作用：为指定用户生成一张 access token（访问令牌，15 分钟有效）。
//
// 参数：accountID 用户编号、username 用户名；
// 返回值：string 签好名的令牌字符串、error 生成失败时的错误。
func GenerateToken(accountID uint, username string) (string, error) {
	// now 变量取当前时间，作为签发时刻。
	now := time.Now()

	// claims 变量构造令牌内容（结构体字面量）。
	claims := Claims{
		// AccountID 写入用户编号。
		AccountID: accountID,
		// Username 写入用户名。
		Username: username,
		// RegisteredClaims 填写标准时间字段。
		RegisteredClaims: jwt.RegisteredClaims{
			// ExpiresAt 是过期时刻：now.Add(15 * time.Minute) 表示当前时间往后 15 分钟。
			// jwt.NewNumericDate(...) 把时间包装成 JWT 要求的数字日期格式。
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			// IssuedAt 是签发时刻（now）。
			IssuedAt: jwt.NewNumericDate(now),
			// NotBefore 是"最早生效时刻"（now，表示立即生效）。
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	// token 变量创建一个待签名的令牌对象。
	// jwt.NewWithClaims(签名算法, 内容)：
	//   - jwt.SigningMethodHS256 表示用 HMAC-SHA256 算法签名（对称加密：签发和验证用同一个密钥）。
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// token.SignedString(jwtSecret())：用密钥签名，返回最终的令牌字符串（和可能的错误）。
	return token.SignedString(jwtSecret())
}

// GenerateRefreshToken 函数的作用：生成一个 refresh token（刷新令牌）。
//
// 技术点：刷新令牌不用 JWT 结构，而是直接生成 32 字节随机数的十六进制字符串，
// 这样它足够长、不可猜测，并且不存在"过期前内容可被解读"的问题。它的有效期（7 天）由数据库记录控制。
//
// 参数 accountID：用户编号（此处未参与生成，保留参数是为了语义对称/将来扩展）；
// 返回值：string 随机刷新令牌、error 随机数生成失败时的错误。
func GenerateRefreshToken(accountID uint) (string, error) {
	// b 创建 32 字节切片存放随机数。
	b := make([]byte, 32)
	// rand.Read(b) 填充随机数，err 接收错误。
	if _, err := rand.Read(b); err != nil {
		// 失败：返回空字符串和错误。
		return "", err
	}
	// 成功：把随机字节编码成十六进制字符串返回（64 个字符），错误为 nil。
	return hex.EncodeToString(b), nil
}

// ParseToken 函数的作用：解析并校验一张令牌，确认它有效后，取出里面的 Claims（用户信息）。
//
// 参数 tokenString：前端传来的令牌字符串；
// 返回值：*Claims 指向解析出的声明内容的指针、error 令牌无效/过期/签名不对时的错误。
// ① 我收到一串 token 字符串
// ② 我请 JWT 库帮我解析，告诉它：
//      · 这是待解析的字符串
//      · 解析内容请装进空的 Claims 盒子（&Claims{}）
//      · 验签要用密钥时，调用我给你的函数——
//        我会先检查算法是不是 HS256（防篡改攻击），
//        是就把密钥给你，不是就报错
// ③ 库解析完，如果出错（签名/过期/算法），我直接返回错误
// ④ 没出错，我把库里那个"任意类型"的内容，
//    用类型断言确认它确实是 *Claims 并取出来
// ⑤ 再确认断言成功、且库标记 token 有效
// ⑥ 全部通过，返回 Claims；调用方由此知道用户是谁

func ParseToken(tokenString string) (*Claims, error) {
	// token 变量接收解析后的令牌对象；err 接收错误。
	// jwt.ParseWithClaims(令牌字符串, 用来装内容的空Claims, 密钥回调函数)：
	token, err := jwt.ParseWithClaims(
		// 第一个参数：待解析的令牌字符串。
		tokenString,
		// 第二个参数：&Claims{} 告诉库"解析出的内容请填进这个 Claims 结构体"。本身是空的，等待传进来
		&Claims{},
		// 第三个参数：一个函数，库在验签时调用它来获取密钥。
		func(token *jwt.Token) (interface{}, error) {
			// token.Method == nil：令牌没带签名方法；
			// token.Method.Alg() != jwt.SigningMethodHS256.Alg()：签名算法不是 HS256。
			// 技术点（防算法篡改攻击）：必须检查令牌自称的算法是不是我们预期的那种，
			// 否则攻击者可能伪造一张用别的算法签的令牌骗过程序。
			if token.Method == nil || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				// 算法不符：返回错误，拒绝解析。
				return nil, errors.New("unexpected signing method")
			}
			// 算法正确：返回密钥供库验签。jwtSecret()：调用我们自己的函数拿到签名密钥；
			// 把密钥交还给库，库拿到后就能去做真正的 HMAC 验签。
			return jwtSecret(), nil
		},
	)
	if err != nil {
		// 解析出错（签名错误、过期等）：返回 nil 和错误。
		return nil, err
	}

	// claims 变量把令牌里的 Claims 取出来并做类型断言。
	// token.Claims.(*Claims)：语法 值.(目标类型) 叫"类型断言"，
	// 尝试把接口类型的值转换成 *Claims；ok 表示是否转换成功。
	// Go 此刻不确认它到底是不是 Claims。你要明确告诉它："我确信里面是 *Claims，帮我取出来。
	claims, ok := token.Claims.(*Claims)
	// !ok 转换失败，或 !token.Valid 库判定令牌无效。
	if !ok || !token.Valid {
		// 返回 nil 和库提供的"声明无效"错误。
		return nil, jwt.ErrTokenInvalidClaims
	}

	// 一切正常：返回解析出的声明 claims，错误为 nil。
	return claims, nil
}
