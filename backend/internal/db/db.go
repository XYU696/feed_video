// package db：数据库包。
// 作用：负责连接 MySQL、根据结构体自动建表（AutoMigrate）、以及关闭数据库连接。
package db

import (
	// 下面这些内部包的导入，是为了在 AutoMigrate 里拿到各张表的结构体（必须先"认识"这些类型才能建表）。
	// account：用户表。
	"feedsystem_video_go/internal/account"
	// config：配置包，NewDB 的参数要用 config.DatabaseConfig 类型。
	"feedsystem_video_go/internal/config"
	// message：私信表。
	"feedsystem_video_go/internal/message"
	// moderation：举报表。
	"feedsystem_video_go/internal/moderation"
	// social：关注关系表。
	"feedsystem_video_go/internal/social"
	// video：视频、点赞、评论、话题、发件箱等表。
	"feedsystem_video_go/internal/video"
	// worker：通知表（Notification 定义在 worker 包里，所以建表也要导入它）。
	"feedsystem_video_go/internal/worker"
	// fmt：用来拼接 DSN 数据库连接字符串。
	"fmt"

	// gorm.io/driver/mysql：GORM 的 MySQL 驱动（负责真正和 MySQL 对话）。
	"gorm.io/driver/mysql"
	// gorm.io/gorm：GORM 核心包，gorm.Open、gorm.DB 等都来自它。
	"gorm.io/gorm"
)

// NewDB 函数的作用：根据数据库配置，建立并返回一个 MySQL 数据库连接。
//
// 参数 dbcfg：MySQL 的连接配置（地址、端口、账号密码、库名）；
// 返回值：
//   - *gorm.DB：数据库连接对象（指针），之后所有数据库操作都靠它；
//   - error：连接失败时的错误，成功为 nil。
func NewDB(dbcfg config.DatabaseConfig) (*gorm.DB, error) {
	// dsn 变量是"数据库连接字符串"(Data Source Name)，
	// 它用一行文字告诉驱动：用什么账号密码、连哪台机器哪个库、还要加什么选项。
	//
	// fmt.Sprintf 按格式模板拼出字符串（类似 Python 的 f-string / % 格式化，但用 %s、%d 这种占位符）：
	//   %s 放字符串、%d 放整数，按顺序对应后面的参数。
	// 拼出来的字符串形如：root:123456@tcp(localhost:3306)/feedsystem?charset=utf8mb4&parseTime=True&loc=Local
	// 各部分含义：
	//   %s:%s              用户名:密码；
	//   @tcp(%s:%d)        用 TCP 连接 主机地址:端口；
	//   /%s                要进入的数据库名；
	//   charset=utf8mb4    字符集用 utf8mb4（支持中文，还支持 emoji 等 4 字节字符）；
	//   parseTime=True     让驱动自动把数据库的时间字段转成 Go 的 time.Time；
	//   loc=Local          时间按本地时区解释。
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		// 这一行依次提供：用户名、密码、主机、端口、库名。
		dbcfg.User, dbcfg.Password, dbcfg.Host, dbcfg.Port, dbcfg.DBName)

	// db 变量接收打开后的数据库连接对象；err 接收连接错误。
	// gorm.Open(mysql.Open(dsn), &gorm.Config{})：
	//   - mysql.Open(dsn) 用上面的连接字符串创建一个 MySQL 驱动；
	//   - &gorm.Config{} 传入 GORM 的配置（空结构体表示全部用默认设置）；
	//   - 合起来就是"打开一个由 GORM 管理的 MySQL 连接"。
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		// 连接失败：返回 nil（没有可用连接）和具体错误，让调用方决定怎么办（比如重试或退出）。
		return nil, err
	}

	// 连接成功：返回数据库对象 db，错误为 nil。
	return db, nil
}

// AutoMigrate 函数的作用：让 GORM 对照 Go 结构体，自动在 MySQL 里创建/更新对应的表。
//
// 技术点：AutoMigrate（自动迁移）会检查表是否存在、字段是否齐全，
// 缺表就建表、缺列就加列，省去手写 CREATE TABLE 的麻烦。
// 注意：它一般只"增"不"减"——你删了结构体字段，它不会自动删数据库列（防止误删数据）。
//
// 参数 db：数据库连接对象；
// 返回值 error：建表过程中的错误，成功为 nil。
func AutoMigrate(db *gorm.DB) error {
	// return 直接把 db.AutoMigrate(...) 的结果（错误）返回出去。
	// db.AutoMigrate 接收"若干个结构体指针"，每传一个 &结构体{} 就为它建一张表：
	return db.AutoMigrate(
		// &account.Account{} 用户表 accounts。
		// &video.Video{} 视频表 videos。
		// &video.Like{} 点赞表 likes。
		// &video.Comment{} 评论表 comments。
		&account.Account{}, &video.Video{}, &video.Like{}, &video.Comment{},
		// &social.Social{} 关注关系表 socials。
		// &video.OutboxMsg{} 发件箱表 outbox_msgs。
		// &video.Tag{} 话题表 tags。
		// &video.VideoTag{} 视频-话题关联表 video_tags。
		&social.Social{}, &video.OutboxMsg{}, &video.Tag{}, &video.VideoTag{},
		// &message.Message{} 私信表 messages。
		// &worker.Notification{} 通知表 notifications。
		&message.Message{}, &worker.Notification{},
		// &moderation.Report{} 举报表 reports。
		// &moderation.Case{} 审核工单表 moderation_cases。
		// &moderation.AuditLog{} 审计流水表 moderation_audit（D10）。
		&moderation.Report{}, &moderation.Case{}, &moderation.AuditLog{},
	)
}

// CloseDB 函数的作用：关闭数据库连接（程序退出前调用，释放资源）。
//
// 参数 db：要关闭的数据库连接对象；
// 返回值 error：关闭时的错误，成功为 nil。
func CloseDB(db *gorm.DB) error {
	// sqlDB 变量接收底层标准库的数据库对象；err 接收错误。
	// db.DB() 从 GORM 对象里取出它内部包着的"通用数据库连接"（*sql.DB），
	// 因为真正的关闭方法在这个底层对象上。
	sqlDB, err := db.DB()
	if err != nil {
		// 取底层对象失败：直接把错误返回。
		return err
	}
	// sqlDB.Close() 关闭连接，并把可能的错误返回给调用方。
	return sqlDB.Close()
}
