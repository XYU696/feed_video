// package main：特殊包名。Go 程序的入口必须是 package main，
// 并且里面要有 func main()，程序启动时就从 main 函数第一行开始执行。
//
// 本文件是 API 服务（给前端提供 HTTP 接口的那个进程）的入口，
// 它像"总装车间"：按顺序把配置、数据库、Redis、RabbitMQ、pprof、路由全部装好，最后启动 Web 服务。
package main

import (
	// context：给 Redis ping 设置超时。
	"context"
	// config：读取配置文件。
	"feedsystem_video_go/internal/config"
	// db：连接 MySQL、自动建表。
	"feedsystem_video_go/internal/db"
	// apphttp：路由装配（http 是 Go 标准库保留名，所以起别名 apphttp）。
	apphttp "feedsystem_video_go/internal/http"
	// rabbitmq：连接 RabbitMQ。
	rabbitmq "feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：连接 Redis。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// observability：启动 pprof 性能分析服务。
	"feedsystem_video_go/internal/observability"
	// log：打印日志；log.Fatalf 打印后直接退出整个程序。
	"log"
	// os：读环境变量。
	"os"
	// strconv：端口号 int 转字符串。
	"strconv"
	// time：ping 超时时长。
	"time"

	// godotenv：第三方库，读取项目根目录 .env 文件里的环境变量。
	"github.com/joho/godotenv"
)

// main 函数：程序入口，无参数无返回值。
// 整体顺序：.env → 配置 → MySQL（必需）→ Redis（可选）→ RabbitMQ（可选）→ pprof → 启动 HTTP 服务。
func main() {
	// 加载 .env（本地开发）
	// godotenv.Load()：读取当前目录 .env 并把里面的键值导入环境变量；err 接收错误。
	if err := godotenv.Load(); err != nil {
		// 文件不存在时不致命：log.Println 只打印一行提示，程序继续（线上靠真实环境变量，不用 .env）。
		log.Println(".env not found; continuing")
	}

	// 加载配置
	// configPath 从环境变量 CONFIG_PATH 取配置文件路径。
	configPath := os.Getenv("CONFIG_PATH")
	// 没设置就用默认路径 configs/config.yaml。
	if configPath == "" {
		// 给 configPath 赋默认值。
		configPath = "configs/config.yaml"
	}
	// 打印将要加载的配置路径。
	log.Printf("Loading config from %s", configPath)
	// cfg 接收完整配置对象；usedDefault 表示"文件不存在、是否用了内置默认配置"；err 接收错误。
	cfg, usedDefault, err := config.LoadLocalDev(configPath)
	if err != nil {
		// 配置出了真正的错误（文件存在但内容解析不了）：log.Fatalf 打印并【退出程序】。
		// 没有配置服务没法启动，所以数据库/配置类故障是致命的。
		log.Fatalf("Failed to load config: %v", err)
	}
	// 用了默认配置（配置文件没找到）。
	if usedDefault {
		// 提示在用内置默认值。
		log.Printf("Config File %s not found, using default local config", configPath)
	} else {
		// 正常读到了配置文件。
		log.Printf("Config loaded from file: %s", configPath)
	}

	// 连接数据库
	// sqlDB 接收 GORM 数据库连接；err 接收错误。db.NewDB(cfg.Database) 用配置里的数据库信息建立连接。
	sqlDB, err := db.NewDB(cfg.Database)
	if err != nil {
		// MySQL 连不上：致命，退出（数据库是核心依赖，不能没有）。
		log.Fatalf("Failed to connect database: %v", err)
	}
	// db.AutoMigrate(sqlDB)：自动检查/创建所有表（10 张表，见 db.go 注释）。
	if err := db.AutoMigrate(sqlDB); err != nil {
		// 建表失败：致命退出。
		log.Fatalf("Failed to auto migrate database: %v", err)
	}
	// defer db.CloseDB(sqlDB)：注册"延迟调用"——main 函数退出前才执行，关闭数据库连接。
	// 技术点：defer 会把调用压栈，函数结束时（正常或出错）自动执行，常用于资源释放，顺序是"后进先出"。
	defer db.CloseDB(sqlDB)

	// 连接 Redis (可选，用于缓存)
	// cache 接收 Redis 客户端；err 接收错误。NewFromEnv(&cfg.Redis) 用配置创建客户端。
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		// 配置层面有问题（地址缺失等）：不退出，只提示并把 cache 置为 nil。
		log.Printf("Redis config error (cache disabled): %v", err)
		// cache = nil：后面所有代码都靠判断 nil 来决定"跳过缓存"。
		cache = nil
	} else {
		// pingCtx 是 300 毫秒超时的上下文（只用 background 无父请求，因为现在还没收到任何请求）；cancel 取消函数。
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		// defer cancel() 释放上下文。
		defer cancel()
		// cache.Ping(pingCtx)：真正发命令测连通性；err 接收错误。
		if err := cache.Ping(pingCtx); err != nil {
			// Redis 创建成功但实际连不通（没启动/网络不通）：
			log.Printf("Redis not available (cache disabled): %v", err)
			// _ = cache.Close()：先关掉这个无用客户端（_ 显式忽略返回错误）。
			_ = cache.Close()
			// 置 nil：服务照样能跑，只是没缓存。
			cache = nil
		} else {
			// 连通正常：注册退出时关闭连接。
			defer cache.Close()
			// 打印缓存已启用。
			log.Printf("Redis connected (cache enabled)")
		}
	}

	// 连接 RabbitMQ (可选，用于消息队列)
	// rmq 接收 RabbitMQ 连接；err 接收错误。NewRabbitMQ(&cfg.RabbitMQ) 建立 AMQP 连接。
	rmq, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		// 连不上：不退出，提示并置 nil。
		log.Printf("RabbitMQ config error (disabled): %v", err)
		// rmq = nil：后面所有 MQ 逻辑都判 nil 跳过。
		rmq = nil
	} else {
		// 注册退出时关闭连接。
		defer rmq.Close()
		// 打印已连接。
		log.Printf("RabbitMQ connected")
	}
	// Pprof
	// pprofServer 接收性能分析服务器；err 接收错误。
	pprofServer, err := observability.NewPprofServer(
		// 进程名 "API"（日志里区分用）。
		"API",
		// cfg.ObservabilityConfig.Pprof.Enabled：是否开启 pprof。
		cfg.ObservabilityConfig.Pprof.Enabled,
		// cfg.ObservabilityConfig.Pprof.ApiAddr：API 进程 pprof 监听地址。
		cfg.ObservabilityConfig.Pprof.ApiAddr,
	)
	if err != nil {
		// pprof 启动失败不致命（只是少了调试手段），打印即可。
		log.Printf("Failed to start API pprof server: %v", err)
	}
	// 服务器确实启动了（非 nil）。
	if pprofServer != nil {
		// 注册退出时优雅关闭。
		defer pprofServer.Close()
	}

	// 设置路由
	// r 接收装配好的 Gin 引擎。SetRouter(...) 把所有组件、路由、后台任务全部组装好；
	// 最后一个参数是审核员账号列表（D7 人工审核台）。
	r := apphttp.SetRouter(sqlDB, cache, rmq, cfg.Agent.ReviewerIDs)
	// 打印服务端口。
	log.Printf("Server is running on port %d", cfg.Server.Port)
	// r.Run(":" + 端口字符串)：启动 HTTP 服务开始监听（这个调用会阻塞在这里直到服务停止）；
	// strconv.Itoa 把 int 端口转成字符串，":" + 它 拼成 ":8080" 形式；err 接收错误。
	if err := r.Run(":" + strconv.Itoa(cfg.Server.Port)); err != nil {
		// 端口被占用等启动失败：致命退出。
		log.Fatalf("Failed to run server: %v", err)
	}
}
