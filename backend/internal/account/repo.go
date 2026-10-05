// package account：账号业务包。本文件是"仓储层(Repository)"，只干一件事：
// 把对 accounts 表的所有数据库操作封装成方法。
//
// 先理解经典的三层分层架构（本项目每个业务模块都是这个结构）：
//
//	Handler（处理 HTTP 请求/响应）
//	  → Service（业务逻辑：校验、加密、缓存、事务编排）
//	    → Repository（只跟数据库打交道，写 SQL/GORM）
//
// 好处：各层职责单一，SQL 全集中在 repo，换 Service 逻辑不影响数据库代码，方便测试。
// 类比 Python（Django）：Repository 很像 Model.objects 那层数据访问对象(DAO)。
package account

import (
	// context：贯穿请求的上下文对象（可携带超时、取消信号），每个方法第一个参数都是它。
	"context"

	// gorm：ORM 库，db *gorm.DB 就是数据库连接句柄，用它拼查询。
	"gorm.io/gorm"
)

// AccountRepository 结构体是账号仓储，内部只持有一个数据库连接。
//
// 技术点：字段 db 小写开头 = 包私有（只能在 account 包内用），
// 外部必须通过下面的 NewAccountRepository 构造函数创建——这叫"封装"。
type AccountRepository struct {
	// db 是 GORM 数据库连接，所有 SQL 都通过它执行。
	db *gorm.DB
}

// NewAccountRepository 是构造函数：创建一个账号仓储并返回它的指针。
//
// 技术点：Go 没有 Python 的 __init__，约定俗成用 NewXxx 函数当构造器；
// 返回 *AccountRepository（指针）而不是 Account（值），避免复制整个对象。
//
// 参数 db：数据库连接；
// 返回值 *AccountRepository：初始化好的仓储。
func NewAccountRepository(db *gorm.DB) *AccountRepository {
	// &AccountRepository{db: db}：用结构体字面量初始化，& 取地址返回指针。
	return &AccountRepository{db: db}
}

// CreateAccount 方法：向 accounts 表插入一条新账号记录。
//
// 接收者 ar *AccountRepository：类似 Python 的 self，ar 就是当前仓储对象；
// 参数 ctx 上下文、account 要插入的账号指针（插入后 GORM 会把自增 ID 回填进这个结构体）；
// 返回值 error：插入失败的错误，成功为 nil。
func (ar *AccountRepository) CreateAccount(ctx context.Context, account *Account) error {
	// ar.db.WithContext(ctx)：把上下文绑给本次查询（请求取消/超时能传递到数据库）；
	// .Create(account)：生成并执行 INSERT，把 account 写进表；
	// .Error：GORM 把执行错误放在结果的 Error 字段上；err 接收它。
	if err := ar.db.WithContext(ctx).Create(account).Error; err != nil {
		// 出错：把错误返回给上层。
		return err
	}
	// 成功：返回 nil。
	return nil
}

// Rename 方法：更新指定账号的用户名为新名字。
//
// 参数：id 账号编号、newUsername 新用户名；
// 返回值 error：更新错误；如果没有任何行被更新，返回"记录不存在"错误。
func (ar *AccountRepository) Rename(ctx context.Context, id uint, newUsername string) error {
	// result 变量接收 GORM 执行结果（里面有 Error 和 RowsAffected）。
	// .Model(&Account{})：指定操作 accounts 表；
	// .Where("id = ?", id)：条件——? 是占位符，GORM 会把 id 安全地填进去（防 SQL 注入）；
	// .Update("username", newUsername)：执行 UPDATE，只改 username 这一列。
	result := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Update("username", newUsername)
	// 数据库执行本身出错。
	if result.Error != nil {
		// 返回执行错误。
		return result.Error
	}
	// result.RowsAffected：实际被修改的行数；== 0 说明这个 id 根本不存在。
	if result.RowsAffected == 0 {
		// 返回 GORM 预定义的哨兵错误 gorm.ErrRecordNotFound（上层可用 errors.Is 识别成 404）。
		return gorm.ErrRecordNotFound
	}
	// 成功：返回 nil。
	return nil
}

// RenameWithToken 方法：在【一个数据库事务】里同时改用户名和该用户的 token。
//
// 为什么需要事务？改用户名后 JWT 里的用户名也变了，必须"用户名 + token"一起改成功，
// 否则会出现"名字改了但 token 还是旧的"的中间状态。
// 事务(Transaction)保证：里面所有操作要么全部成功提交，要么全部回滚（类比 Python 的 with transaction.atomic()）。
//
// 参数：id 账号编号、newUsername 新用户名、token 用新名字重新签发的 JWT；
// 返回值 error：事务中任何一步出错都会回滚并返回该错误。
func (ar *AccountRepository) RenameWithToken(ctx context.Context, id uint, newUsername string, token string) error {
	// .Transaction(func(tx *gorm.DB) error { ... })：开启事务，回调里的 tx 是【事务专用连接】，
	// 回调返回 nil 就提交，返回任何 error 就回滚。
	return ar.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// result 用 tx（事务连接）执行改名 UPDATE。
		result := tx.Model(&Account{}).Where("id = ?", id).Update("username", newUsername)
		// 执行出错。
		if result.Error != nil {
			// 返回错误 → 整个事务回滚。
			return result.Error
		}
		// 没有行被更新：账号不存在。
		if result.RowsAffected == 0 {
			// 返回"记录不存在"→ 事务回滚。
			return gorm.ErrRecordNotFound
		}
		// 用同一个 tx 更新 token 列；.Error 取错误，err 接收。
		if err := tx.Model(&Account{}).Where("id = ?", id).Update("token", token).Error; err != nil {
			// 更新 token 出错：返回错误 → 回滚（改名也一并撤销）。
			return err
		}
		// 两步都成功：返回 nil → 事务提交。
		return nil
	})
}

// ChangePassword 方法：更新指定账号的密码（存进来的已经是 bcrypt 哈希值）。
//
// 参数：id 账号编号、newPassword 新密码哈希；
// 返回值 error：更新错误。
func (ar *AccountRepository) ChangePassword(ctx context.Context, id uint, newPassword string) error {
	// 执行 UPDATE ... SET password = ? WHERE id = ?；err 接收错误。
	if err := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Update("password", newPassword).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// FindByID 方法：按主键 id 查询一个账号。
//
// 参数 id：账号编号；
// 返回值：*Account 查到的账号指针、error 查询错误（查不到时是 gorm.ErrRecordNotFound）。
func (ar *AccountRepository) FindByID(ctx context.Context, id uint) (*Account, error) {
	// account 声明一个 Account 变量，准备接收查询结果。
	var account Account
	// .First(&account, id)：按主键查第一条，结果写进 account；
	// 传 &account（地址）GORM 才能往里填数据；err 接收错误。
	if err := ar.db.WithContext(ctx).First(&account, id).Error; err != nil {
		// 出错：返回 nil 和错误（指针用 nil 表示"没有结果"）。
		return nil, err
	}
	// 成功：返回 account 的地址和 nil。
	return &account, nil
}

// FindByUsername 方法：按用户名查询一个账号（登录、改密码时用）。
//
// 参数 username：用户名；
// 返回值：*Account 账号指针、error 查询错误。
func (ar *AccountRepository) FindByUsername(ctx context.Context, username string) (*Account, error) {
	// account 准备接收结果。
	var account Account
	// .Where("username = ?", username) 加条件，.First(&account) 取一条；err 接收错误。
	if err := ar.db.WithContext(ctx).Where("username = ?", username).First(&account).Error; err != nil {
		// 出错返回 nil 和错误。
		return nil, err
	}
	// 成功返回账号指针。
	return &account, nil
}

// Login 方法：登录成功后，把新签发的 access token 和 refresh token 存进该账号记录。
//
// 技术点：token 落库是为了实现"服务端可注销"——退出登录时把 token 清空，
// 旧 token 即使没过期也会被判定无效。
//
// 参数：id 账号编号、token 访问令牌、refreshToken 刷新令牌；
// 返回值 error：更新错误。
func (ar *AccountRepository) Login(ctx context.Context, id uint, token, refreshToken string) error {
	// .Updates(map[...]{...})：用 map 一次更新多列（token、refresh_token）。
	// 注意：用 map 更新时即使值是空串也会写入（用结构体 Updates 则会跳过零值，这是 GORM 重要细节）。
	if err := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(map[string]interface{}{"token": token, "refresh_token": refreshToken}).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// Logout 方法：退出登录，把该账号的 token 和 refresh_token 都清空（使旧令牌失效）。
//
// 参数 id：账号编号；
// 返回值 error：更新错误。
func (ar *AccountRepository) Logout(ctx context.Context, id uint) error {
	// Updates 一个两列都为空串的 map，执行清空。
	if err := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(map[string]interface{}{"token": "", "refresh_token": ""}).Error; err != nil {
		// 出错返回。
		return err
	}
	// 成功返回 nil。
	return nil
}

// UpdateAvatar 方法：更新账号头像的 URL。
//
// 参数：accountID 账号编号、avatarURL 头像图片地址；
// 返回值 error：更新错误（这里直接把 GORM 调用返回，不再拆行判断）。
func (ar *AccountRepository) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	// 执行 UPDATE avatar_url = ?，并把结果的 .Error 直接 return。
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", accountID).Update("avatar_url", avatarURL).Error
}

// UpdateToken 方法：只更新账号的 access token（刷新令牌接口用：refresh token 不变，换发新 access token）。
//
// 参数：id 账号编号、token 新访问令牌；
// 返回值 error：更新错误。
func (ar *AccountRepository) UpdateToken(ctx context.Context, id uint, token string) error {
	// 执行 UPDATE token = ?，直接返回错误。
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Update("token", token).Error
}

// UpdateFields 方法：按传入的 map 动态更新若干列（编辑资料用，传什么列改什么列）。
//
// 参数：id 账号编号、updates 列名→新值的映射；
// 返回值 error：更新错误。
func (ar *AccountRepository) UpdateFields(ctx context.Context, id uint, updates map[string]interface{}) error {
	// Updates(updates) 按 map 内容更新对应列，直接返回错误。
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(updates).Error
}

// FindAll 方法：查出 accounts 表的全部账号。
//
// 没有业务参数；
// 返回值：[]*Account 账号指针切片（注意是指针切片）、error 查询错误。
//
// 说明：全表扫描只适合小表/管理用途；refresh token 兜底查找用了它（正常路径走 Redis）。
func (ar *AccountRepository) FindAll(ctx context.Context) ([]*Account, error) {
	// accounts 声明账号指针切片，准备装结果。
	var accounts []*Account
	// .Find(&accounts)：查全部记录，填进切片；err 接收错误。
	if err := ar.db.WithContext(ctx).Find(&accounts).Error; err != nil {
		// 出错返回 nil 和错误。
		return nil, err
	}
	// 成功返回整个切片。
	return accounts, nil
}
