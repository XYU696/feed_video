// package account：账号业务包。本文件是"服务层(Service)"，
// 处在 Handler 和 Repository 中间，承载真正的业务规则：
// 密码加密与校验、签发 JWT、读写 Redis 缓存、判断用户名是否重复、编排仓储调用。
//
// 记住分层原则：Handler 不直接碰 Repository 的数据库细节，所有业务动作都先经过 Service。
package account

import (
	// context：超时控制等。
	"context"
	// errors：定义哨兵错误、errors.Is/errors.As 错误识别。
	"errors"
	// auth：本项目 JWT 签发包。
	"feedsystem_video_go/internal/auth"
	// log：缓存操作失败时打日志（不影响主流程）。
	"log"
	// strconv：用户编号和字符串互转。
	"strconv"
	// strings：去空白。
	"strings"
	// time：缓存超时时间、过期时长。
	"time"

	// rediscache：Redis 客户端，token 缓存和刷新令牌查找都靠它。
	rediscache "feedsystem_video_go/internal/middleware/redis"

	// mysql：MySQL 驱动包，用来识别 1062 号错误（唯一索引冲突）。
	"github.com/go-sql-driver/mysql"
	// bcrypt：业界标准的密码哈希算法库。
	"golang.org/x/crypto/bcrypt"
	// gorm：用它的 ErrRecordNotFound 哨兵错误。
	"gorm.io/gorm"
)

// AccountService 结构体是账号服务层，持有它需要的两个依赖：
// 仓储（操作数据库）和缓存（操作 Redis）。
type AccountService struct {
	// accountRepository 账号仓储，所有数据库操作通过它完成。
	accountRepository *AccountRepository
	// cache 是 Redis 客户端；可能为 nil（没配 Redis 时，所有缓存逻辑自动跳过）。
	cache *rediscache.Client
}

// var ( ... ) 集中定义包级别的"哨兵错误"：预先声明好的固定错误值，
// 上层用 errors.Is(err, ErrXxx) 判断具体是哪种业务错误。
var (
	// ErrUsernameTaken：注册/改名时用户名已被占用。
	ErrUsernameTaken = errors.New("username already exists")
	// ErrNewUsernameRequired：改名时没传新用户名。
	ErrNewUsernameRequired = errors.New("new_username is required")
)

// NewAccountService 是构造函数：创建账号服务。
//
// 参数：accountRepository 账号仓储、cache Redis 客户端；
// 返回值 *AccountService：初始化好的服务。
func NewAccountService(accountRepository *AccountRepository, cache *rediscache.Client) *AccountService {
	// 用结构体字面量注入两个依赖并返回指针（"依赖注入"：需要什么由外面传进来，而不是自己 new）。
	return &AccountService{accountRepository: accountRepository, cache: cache}
}

// CreateAccount 方法：注册账号的业务逻辑——先把明文密码哈希，再交给仓储入库。
//
// 参数：account 注册信息（里面 Password 暂时是明文）；
// 返回值 error：哈希失败或入库失败的错误。
func (as *AccountService) CreateAccount(ctx context.Context, account *Account) error {
	// passwordHash 接收 bcrypt 哈希结果（[]byte 字节切片）；err 接收错误。
	// bcrypt.GenerateFromPassword(明文, bcrypt.DefaultCost)：
	//   - 把密码用 bcrypt 算法加盐哈希，每次结果都不同（盐随机）；
	//   - DefaultCost=10 是计算强度，越大约安全也越慢，用来抵抗暴力破解。
	// []byte(account.Password)：string 转字节切片（bcrypt 只吃 []byte）。
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(account.Password), bcrypt.DefaultCost)
	if err != nil {
		// 哈希失败（极少见）：返回错误。
		return err
	}
	// account.Password = string(passwordHash)：把哈希值转回字符串，覆盖掉明文，
	// 保证落库的绝不是明文密码。
	account.Password = string(passwordHash)
	// 调仓储插入账号；err 接收错误。
	if err := as.accountRepository.CreateAccount(ctx, account); err != nil {
		// 入库失败（比如用户名唯一索引冲突）：返回错误，具体识别在 Handler 或调用方做。
		return err
	}
	// 成功：返回 nil。
	return nil
}

// Rename 方法：修改用户名的完整业务逻辑。
//
// 流程：校验非空 → 用新名字重签 JWT → 事务里同时改名字和 token
// → 识别"用户名重复"错误 → 把新 token 写进 Redis 缓存。
//
// 参数：accountID 当前账号编号、newUsername 想要的新用户名；
// 返回值：string 重新签发的 JWT（前端要替换保存）、error 错误。
func (as *AccountService) Rename(ctx context.Context, accountID uint, newUsername string) (string, error) {
	// 新用户名为空。
	if newUsername == "" {
		// 返回空 token 和"必须提供新用户名"哨兵错误。
		return "", ErrNewUsernameRequired
	}

	// token 用新用户名重新生成 JWT；err 接收错误。
	// JWT 的载荷里含用户名，所以名字一变必须重签，否则 token 和库里名字对不上。
	token, err := auth.GenerateToken(accountID, newUsername)
	if err != nil {
		// 签发失败：返回。
		return "", err
	}

	// 调仓储在事务里改名字+token；err 接收错误。
	if err := as.accountRepository.RenameWithToken(ctx, accountID, newUsername, token); err != nil {
		// mysqlErr 声明 MySQL 驱动的错误类型指针，准备做错误类型识别。
		var mysqlErr *mysql.MySQLError
		// errors.As(err, &mysqlErr)：把错误链里的 *mysql.MySQLError 提取出来（类似类型版的 errors.Is）。
		// mysqlErr.Number == 1062：1062 是 MySQL"唯一键冲突"错误码——accounts.username 有唯一索引，
		// 新名字跟别人撞了就会触发。
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			// 翻译成业务错误 ErrUsernameTaken（上层返回 409 Conflict）。
			return "", ErrUsernameTaken
		}
		// errors.Is(err, gorm.ErrRecordNotFound)：账号不存在。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 原样返回（上层返回 404）。
			return "", err
		}
		// 其他数据库错误：原样返回。
		return "", err
	}
	// 仓储更新成功后，如果配了 Redis。
	if as.cache != nil {
		// cacheCtx 是只给缓存操作用的上下文，50 毫秒超时；cancel 是取消函数。
		// 为什么这么短？缓存是"锦上添花"，绝不能让 Redis 慢拖慢改名主流程。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer cancel()：函数退出时取消，及时释放资源。
		defer cancel()

		// SetBytes 把新 token 存进 key "account:{id}"，TTL 24 小时；err 接收错误。
		// as.cache.Key("account:%d", accountID)：带前缀拼 key（%d 被编号替换）。
		// []byte(token)：字符串转字节。
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", accountID), []byte(token), 24*time.Hour); err != nil {
			// 缓存写失败只记日志，不返回错误（数据库已经成功，不能因缓存让用户改名失败）。
			log.Printf("failed to set cache: %v", err)
		}
	}
	// 返回新 token，错误为 nil。
	return token, nil
}

// ChangePassword 方法：修改密码的完整业务逻辑。
//
// 流程：按用户名查到账号 → 用 bcrypt 校验旧密码 → 哈希新密码 → 更新
// → 强制登出（让所有旧 token 失效，需重新登录）。
//
// 参数：username 用户名、oldPassword 旧密码、newPassword 新密码；
// 返回值 error：任何一步失败的错误。
func (as *AccountService) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	// account 按用户名查到账号；err 接收错误。
	account, err := as.FindByUsername(ctx, username)
	if err != nil {
		// 用户不存在等：返回错误。
		return err
	}
	// bcrypt.CompareHashAndPassword(库里存的哈希, 用户输入的旧密码)：
	// 内部会重新算哈希并比对，密码正确返回 nil，错误返回非 nil（它天生防时序攻击）。
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(oldPassword)); err != nil {
		// 旧密码不对：返回错误。
		return err
	}
	// passwordHash 哈希新密码；err 接收错误。
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		// 哈希失败：返回。
		return err
	}
	// 调仓储更新密码（存哈希字符串）；err 接收错误。
	if err := as.accountRepository.ChangePassword(ctx, account.ID, string(passwordHash)); err != nil {
		// 更新失败：返回。
		return err
	}
	// as.Logout(...)：改完密码强制登出，清空库里和缓存里的 token，
	// 这样别的设备上的旧登录态也全部失效（安全设计）。
	if err := as.Logout(ctx, account.ID); err != nil {
		// 登出失败：返回。
		return err
	}
	// 全部成功：返回 nil。
	return nil
}

// FindByID 方法：按编号查账号（Service 层对仓储的薄封装，
// 其他 Service 方法复用它，将来可在这一层加缓存而调用方无感）。
//
// 参数 id：账号编号；
// 返回值：*Account 账号、error 错误。
func (as *AccountService) FindByID(ctx context.Context, id uint) (*Account, error) {
	// 调仓储查询；if 初始化语句里同时拿 account、err。
	if account, err := as.accountRepository.FindByID(ctx, id); err != nil {
		// 出错：返回 nil。
		return nil, err
	} else {
		// 成功：返回账号。
		return account, nil
	}
}

// FindByUsername 方法：按用户名查账号（同样是对仓储的薄封装）。
//
// 参数 username：用户名；
// 返回值：*Account 账号、error 错误。
func (as *AccountService) FindByUsername(ctx context.Context, username string) (*Account, error) {
	// 调仓储按用户名查。
	if account, err := as.accountRepository.FindByUsername(ctx, username); err != nil {
		// 出错返回。
		return nil, err
	} else {
		// 成功返回账号。
		return account, nil
	}
}

// Login 方法：登录的完整业务逻辑。
//
// 流程：查账号 → bcrypt 校验密码 → 签发 access token(15分钟) 和 refresh token(7天)
// → 落库 → 写 3 个 Redis 缓存键。
//
// 参数：username 用户名、password 明文密码；
// 返回值：string accessToken、string refreshToken、error 错误。
func (as *AccountService) Login(ctx context.Context, username, password string) (string, string, error) {
	// account 先按用户名查出账号（确认用户存在）；err 接收错误。
	account, err := as.FindByUsername(ctx, username)
	if err != nil {
		// 用户不存在：返回两个空串和错误。
		return "", "", err
	}
	// bcrypt 校验密码；err 接收错误。
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(password)); err != nil {
		// 密码错误：返回。
		return "", "", err
	}
	// accessToken 生成访问令牌（15 分钟有效，接口鉴权用）；err 接收错误。
	accessToken, err := auth.GenerateToken(account.ID, account.Username)
	if err != nil {
		// 签发失败：返回。
		return "", "", err
	}
	// refreshToken 生成刷新令牌（7 天有效，随机十六进制串，用来换新 access token）；err 接收错误。
	refreshToken, err := auth.GenerateRefreshToken(account.ID)
	if err != nil {
		// 签发失败：返回。
		return "", "", err
	}
	// 调仓储把两个 token 落库；err 接收错误。
	if err := as.accountRepository.Login(ctx, account.ID, accessToken, refreshToken); err != nil {
		// 落库失败：返回。
		return "", "", err
	}
	// 配了 Redis 就写缓存。
	if as.cache != nil {
		// cacheCtx 缓存专用上下文，50ms 超时；cancel 取消函数。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 退出时取消。
		defer cancel()

		// 缓存键 1：account:{id} → accessToken，TTL 24h（鉴权中间件可快速取 token 比对）。
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(accessToken), 24*time.Hour); err != nil {
			// 失败只记日志。
			log.Printf("failed to set cache: %v", err)
		}
		// 缓存键 2：account:{id}:refresh → refreshToken，TTL 7 天。
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d:refresh", account.ID), []byte(refreshToken), 7*24*time.Hour); err != nil {
			// 失败只记日志。
			log.Printf("failed to set refresh cache: %v", err)
		}
		// 缓存键 3（反向索引）：refresh:{refreshToken} → 账号编号字符串，TTL 7 天。
		// 刷新接口只带 refresh token，靠这个键 O(1) 反查出是哪个用户，不用扫表。
		// strconv.FormatUint 把账号编号转成十进制字符串。
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken), []byte(strconv.FormatUint(uint64(account.ID), 10)), 7*24*time.Hour); err != nil {
			// 失败只记日志。
			log.Printf("failed to set refresh lookup: %v", err)
		}
	}
	// 返回两个令牌，错误 nil。
	return accessToken, refreshToken, nil
}

// Logout 方法：退出登录的完整业务逻辑——删缓存 + 清空库里的 token。
//
// 参数 accountID：账号编号；
// 返回值 error：错误。
func (as *AccountService) Logout(ctx context.Context, accountID uint) error {
	// account 先查到账号（要拿到现有 RefreshToken 才知道删哪个反向键）；err 接收错误。
	account, err := as.FindByID(ctx, accountID)
	if err != nil {
		// 查询失败：返回。
		return err
	}
	// account.Token == ""：库里已经没有 token，说明本来就是登出状态。
	if account.Token == "" {
		// 幂等处理：直接返回 nil，不重复操作也不报错（登多次结果一样）。
		return nil
	}
	// 配了 Redis 就逐个删缓存键。
	if as.cache != nil {
		// cacheCtx 50ms 超时；cancel 取消函数。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 退出时取消。
		defer cancel()

		// 删除缓存键 account:{id}（access token）；err 接收错误。
		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d", account.ID)); err != nil {
			// 失败只记日志。
			log.Printf("failed to del cache: %v", err)
		}
		// 删除缓存键 account:{id}:refresh；err 接收错误。
		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d:refresh", account.ID)); err != nil {
			// 失败只记日志。
			log.Printf("failed to del refresh cache: %v", err)
		}
		// 库里 RefreshToken 非空时，才存在对应的反向索引键。
		if account.RefreshToken != "" {
			// 删除反向键 refresh:{refreshToken}；err 接收错误。
			if err := as.cache.Del(cacheCtx, as.cache.Key("refresh:%s", account.RefreshToken)); err != nil {
				// 失败只记日志。
				log.Printf("failed to del refresh lookup: %v", err)
			}
		}
	}
	// 最后调仓储清空数据库里的两个 token，并把结果返回。
	return as.accountRepository.Logout(ctx, account.ID)
}

// UpdateAvatar 方法：更新头像 URL 的薄封装（直接委托仓储）。
//
// 参数：accountID 账号编号、avatarURL 头像地址；
// 返回值 error：错误。
func (as *AccountService) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	// 调仓储更新头像并返回错误。
	return as.accountRepository.UpdateAvatar(ctx, accountID, avatarURL)
}

// FindAll 方法：查全部账号（薄封装）。
//
// 无业务参数；
// 返回值：[]*Account 账号指针切片、error 错误。
func (as *AccountService) FindAll(ctx context.Context) ([]*Account, error) {
	// 调仓储查全部并返回。
	return as.accountRepository.FindAll(ctx)
}

// UpdateProfile 方法：编辑个人资料的业务逻辑。
//
// 动态组装要更新的列：bio（简介）和 avatar_url 只更新用户实际提交的字段，
// 防止把没传的字段覆盖成空值。
//
// 参数：accountID 账号编号、req 编辑请求（指针）；
// 返回值 error：错误。
func (as *AccountService) UpdateProfile(ctx context.Context, accountID uint, req *UpdateProfileRequest) error {
	// updates 创建空 map，准备按情况填要更新的列（列名→新值）。
	updates := map[string]interface{}{}
	// req.Bio != ""：用户传了简介。
	if req.Bio != "" {
		// TrimSpace 去首尾空白后放入 "bio" 列。
		updates["bio"] = strings.TrimSpace(req.Bio)
	}
	// req.AvatarURL != ""：用户传了头像地址。
	if req.AvatarURL != "" {
		// 去空白后放入 "avatar_url" 列。
		updates["avatar_url"] = strings.TrimSpace(req.AvatarURL)
	}
	// len(updates) == 0：两个字段都没传，没有任何可更新的东西。
	if len(updates) == 0 {
		// 返回错误提示"没有要更新的内容"（len 是 map 当前键值对数量）。
		return errors.New("nothing to update")
	}
	// 调仓储按 map 更新并返回错误。
	return as.accountRepository.UpdateFields(ctx, accountID, updates)
}

// RefreshAccessToken 方法：用 refresh token 换发新的 access token。
//
// access token 只有 15 分钟，过期后前端拿 refresh token（7 天）调本方法换新的，
// 用户就能免登录保持在线。
//
// 两条路径：
//  1. 快路径（Redis 可用）：用反向键 refresh:{token} 直接拿到账号编号；
//  2. 兜底路径（缓存没有/Redis 挂了）：查全部账号逐个比对 refresh token。
//
// 参数 refreshToken：客户端持有的刷新令牌；
// 返回值：string 新 access token、uint 账号编号、string 用户名、error 错误。
func (as *AccountService) RefreshAccessToken(ctx context.Context, refreshToken string) (string, uint, string, error) {
	// 刷新令牌为空。
	if refreshToken == "" {
		// 返回空结果和错误。
		return "", 0, "", errors.New("refresh token is empty")
	}
	// 快路径：配了 Redis。
	if as.cache != nil {
		// cacheCtx 50ms 超时；cancel 取消函数。
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		// defer 退出时取消。
		defer cancel()
		// b 接收反向键里存的账号编号字节；err 接收错误。GetBytes 读 key refresh:{refreshToken}。
		b, err := as.cache.GetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken))
		// err == nil：缓存命中。
		if err == nil {
			// idStr 把字节转成字符串（账号编号）。
			idStr := string(b)
			// id 把十进制字符串解析成 uint64；parseErr 接收错误。
			id, parseErr := strconv.ParseUint(idStr, 10, 64)
			// 解析成功。
			if parseErr == nil {
				// account 按编号查账号（要校验库里 refresh token 是否仍匹配、拿用户名）；err 接收错误。
				account, err := as.FindByID(ctx, uint(id))
				// 查询成功、账号非空，并且【库里当前的 refresh token 确实等于传入的】。
				// 这一步很关键：防止用上一个已作废的 refresh token（比如改密码后）换新。
				if err == nil && account != nil && account.RefreshToken == refreshToken {
					// newToken 用账号信息签发新 access token；err 接收错误。
					newToken, err := auth.GenerateToken(account.ID, account.Username)
					if err != nil {
						// 签发失败：返回。
						return "", 0, "", err
					}
					// 调仓储把新 token 更新到库；err 接收错误。
					if err := as.accountRepository.UpdateToken(ctx, account.ID, newToken); err != nil {
						// 更新失败：返回。
						return "", 0, "", err
					}
					// 同步更新缓存里的 access token（TTL 24h）；失败只记日志。
					if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(newToken), 24*time.Hour); err != nil {
						// 记日志。
						log.Printf("failed to set cache: %v", err)
					}
					// 成功：返回新 token、编号、用户名。
					return newToken, account.ID, account.Username, nil
				}
			}
		}
	}
	// 兜底路径：缓存没命中或没配 Redis——查全部账号。
	// accounts 接收全部账号；err 接收错误。
	accounts, err := as.FindAll(ctx)
	if err != nil {
		// 查询失败：返回。
		return "", 0, "", err
	}
	// for range 逐个遍历账号；_ 占住下标位置（不需要下标），acc 是当前账号。
	for _, acc := range accounts {
		// 当前账号的 refresh token 等于传入令牌。
		if acc.RefreshToken == refreshToken {
			// newToken 签发新 access token；err 接收错误。
			newToken, err := auth.GenerateToken(acc.ID, acc.Username)
			if err != nil {
				// 签发失败：返回。
				return "", 0, "", err
			}
			// 调仓储更新 token；err 接收错误。
			if err := as.accountRepository.UpdateToken(ctx, acc.ID, newToken); err != nil {
				// 更新失败：返回。
				return "", 0, "", err
			}
			// 成功：返回新 token 和账号信息。
			return newToken, acc.ID, acc.Username, nil
		}
	}
	// 遍历完都没匹配：refresh token 无效（或已被登出作废）。
	return "", 0, "", errors.New("invalid refresh token")
}
