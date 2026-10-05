// package config：配置包。
// 作用：定义程序的全部配置长什么样，负责读取 yaml 配置文件，
// 并允许用"环境变量"覆盖文件里的配置（Docker 部署时就是靠环境变量传地址密码的）。
package config

import (
	// errors 错误处理包：这里用 errors.Is 判断"文件不存在"错误。
	"errors"
	// fmt 格式化包：这里用 fmt.Errorf 包装错误（给错误补充说明文字）。
	"fmt"
	// os 操作系统包：读文件、读环境变量都靠它。
	"os"
	// strconv 字符串转换包：这里用 strconv.Atoi 把字符串转成整数。
	"strconv"
	// strings 字符串处理包：解析审核员 ID 列表时按逗号切分。
	"strings"

	// gopkg.in/yaml.v3 是第三方 YAML 解析库，把 yaml 文件内容解析进 Go 结构体。
	"gopkg.in/yaml.v3"
)

// Config 是"整个程序的总配置"，yaml 文件里的所有配置项都装在它里面。
//
// 技术点：yaml:"xxx" 标签和之前学的 json 标签用法一样，
// 只是这个标签是给 YAML 解析库看的，表示这个字段对应 yaml 文件里的哪个配置项。
type Config struct {
	// Server 是服务器相关配置（监听端口），对应 yaml 里的 server 段。
	Server ServerConfig `yaml:"server"`
	// Database 是 MySQL 数据库配置（地址、账号密码、库名）。
	Database DatabaseConfig `yaml:"database"`
	// Redis 是 Redis 缓存配置。
	Redis RedisConfig `yaml:"redis"`
	// RabbitMQ 是 RabbitMQ 消息队列配置。
	RabbitMQ RabbitMQConfig `yaml:"rabbitmq"`
	// ObservabilityConfig 是可观测性配置（pprof 性能分析）。
	ObservabilityConfig ObservabilityConfig `yaml:"observability"`
	// Agent 是内容安全 Agent 配置（模型、地址、阈值等），对应 yaml 里的 agent 段。
	Agent AgentConfig `yaml:"agent"`
}

// AgentConfig 是内容安全 Agent 的配置。
//
// 安全要点：API Key【不写在配置文件里】，这里只记录"去哪个环境变量取 Key"
// （APIKeyEnv），真正的 Key 在程序运行时从环境变量读，避免泄露到代码/仓库。
type AgentConfig struct {
	// Enabled 是否启用 Agent；false 时 Worker 走原来的"建空工单"逻辑，不需要 Key。
	Enabled bool `yaml:"enabled"`
	// Provider 供应商标识（openai_compatible）。
	Provider string `yaml:"provider"`
	// Model 模型名。
	Model string `yaml:"model"`
	// BaseURL 兼容 OpenAI 接口的基础地址（末尾不带 /chat/completions）。
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv 存放 API Key 的环境变量名，默认 AGENT_API_KEY。
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxRounds Function Calling 最大轮次（D3 先预留）。
	MaxRounds int `yaml:"max_rounds"`
	// TimeoutSeconds 单次 Agent 运行硬超时（秒）。
	TimeoutSeconds int `yaml:"run_timeout_seconds"`
	// Thresholds 各处置动作的置信度门槛。
	Thresholds ThresholdConfig `yaml:"thresholds"`
	// ReviewerIDs 审核员账号编号列表：只有这些账号能访问人工审核台。
	// 这是最简的审核员身份方案（agent.md §10）；给账号加 role 字段属于二期。
	ReviewerIDs []uint `yaml:"reviewer_ids"`
}

// ThresholdConfig 是处置动作的置信度门槛。
type ThresholdConfig struct {
	// Remove 删除/下架的置信度门槛。
	Remove float64 `yaml:"remove"`
	// Warn 警告的置信度门槛。
	Warn float64 `yaml:"warn"`
}

// ServerConfig 是服务器配置：程序监听哪个端口。
type ServerConfig struct {
	// Port 是后端 API 的监听端口（yaml 里 server.port，本地默认 8080）。
	Port int `yaml:"port"`
}

// DatabaseConfig 是 MySQL 数据库的连接配置。
type DatabaseConfig struct {
	// Host 是数据库地址（本机 localhost 或 Docker 里的服务名 mysql）。
	Host string `yaml:"host"`
	// Port 是数据库端口（MySQL 默认 3306）。
	Port int `yaml:"port"`
	// User 是登录数据库的用户名。
	User string `yaml:"user"`
	// Password 是数据库密码。
	Password string `yaml:"password"`
	// DBName 是要连接的数据库名字（本项目 feedsystem）。
	DBName string `yaml:"dbname"`
}

// RedisConfig 是 Redis 的连接配置。
type RedisConfig struct {
	// Host 是 Redis 地址。
	Host string `yaml:"host"`
	// Port 是 Redis 端口（默认 6379）。
	Port int `yaml:"port"`
	// Password 是 Redis 密码。
	Password string `yaml:"password"`
	// DB 是使用 Redis 的第几号库（Redis 默认有 16 个库，编号 0~15，一般用 0 号）。
	DB int `yaml:"db"`
}

// RabbitMQConfig 是 RabbitMQ 的连接配置。
type RabbitMQConfig struct {
	// Host 是 RabbitMQ 地址。
	Host string `yaml:"host"`
	// Port 是 RabbitMQ 端口（AMQP 默认 5672）。
	Port int `yaml:"port"`
	// Username 是登录用户名。
	Username string `yaml:"username"`
	// Password 是登录密码。
	Password string `yaml:"password"`
}

// ObservabilityConfig 是可观测性总配置，目前里面只有 pprof。
type ObservabilityConfig struct {
	// Pprof 是 pprof 性能分析服务的配置。
	Pprof PprofConfig `yaml:"pprof"`
}

// PprofConfig 是 pprof（Go 自带的性能分析工具）的配置。
type PprofConfig struct {
	// Enabled 表示是否开启 pprof。
	Enabled bool `yaml:"enabled"`
	// ApiAddr 是 API 进程 pprof 的监听地址（本地默认 localhost:6060）。
	ApiAddr string `yaml:"api_addr"`
	// WorkerAddr 是 Worker 进程 pprof 的监听地址（本地默认 localhost:6061）。
	WorkerAddr string `yaml:"worker_addr"`
}

// Load 函数的作用：读取并解析指定的 yaml 配置文件，返回填好的总配置 Config。
//
// 参数 filename：配置文件的路径（比如 configs/config.yaml）；
// 返回值：Config（解析出的配置）和 error（读文件/解析失败时的错误，成功时为 nil）。
func Load(filename string) (Config, error) {
	// data 变量保存从文件里读出的原始内容（字节切片 []byte，可以理解为"二进制文本"）。
	// os.ReadFile(filename) 一次性把整个文件读进内存；err 接收读取错误（比如文件不存在）。
	data, err := os.ReadFile(filename)
	if err != nil {
		// return Config{} 返回一个【空的 Config】（所有字段都是零值），并返回包装后的错误。
		//
		// 重点技术点——fmt.Errorf 的 %w：
		//   fmt.Errorf("failed to read config file: %w", err)
		//   %w 的意思是"把原来的错误 err 包进新错误里"（w = wrap 包装），
		//   新错误文字是补充说明，里面还裹着原始错误，之后用 errors.Is 仍能认出原始错误。
		//   对比：用 %v 只是把错误文字拼进去（认不出原始错误），用 %w 才能保留错误链。
		return Config{}, fmt.Errorf("failed to read config file: %w", err)
	}

	// var cfg Config 声明一个空的总配置变量，等 yaml 解析时往里填。
	var cfg Config
	// yaml.Unmarshal(data, &cfg)：把文件字节 data 按 yaml 规则解析，填进 cfg。
	//
	// 重点技术点——为什么第二个参数要写 &cfg（取地址）？
	// 因为 Go 函数传参默认是"复制一份"传进去。如果直接传 cfg，函数改的是复制品，外面的 cfg 不会有变化；
	// &cfg 表示"把 cfg 的内存地址传进去"（& 是取地址符），解析库拿到地址就能直接修改我们这个 cfg 本身。
	// 简单记：想让函数"往变量里填东西"，通常要传 &变量。
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// 解析失败：返回空配置和包装错误（%s 把文件名拼进提示，%w 包原始错误）。
		return Config{}, fmt.Errorf("parse config %s: %w", filename, err)
	}

	// ApplyEnvOverrides(&cfg)：用环境变量覆盖配置（同样传地址，函数才能改掉 cfg）。
	// 作用：yaml 文件里写的是默认值，如果系统设置了环境变量（Docker 里常这么干），就以环境变量为准。
	ApplyEnvOverrides(&cfg)
	// 解析和覆盖都成功，返回填好的配置 cfg，错误为 nil。
	return cfg, nil
}

// ApplyEnvOverrides 函数的作用：检查一系列环境变量，
// 只要某个环境变量被设置了（非空），就用它覆盖配置结构体里对应的字段。
//
// 参数 cfg：*Config 指针，指向要被修改的总配置（必须用指针，否则改的是副本）；
// 没有返回值。
func ApplyEnvOverrides(cfg *Config) {
	// if cfg == nil：先判断指针是不是空（防御性写法）。
	if cfg == nil {
		// return 直接结束函数：配置都不存在，没东西可改。
		return
	}
	// if v := os.Getenv("SERVER_PORT"); v != "" { ... }
	//
	// 技术点——if 的"初始化语句"：if 后面可以先写一句赋值（v := ...），再写分号和真正的判断条件，
	// 变量 v 只在这个 if 块内有效。os.Getenv("SERVER_PORT") 读取名为 SERVER_PORT 的环境变量，没设置时返回 ""。
	if v := os.Getenv("SERVER_PORT"); v != "" {
		// strconv.Atoi(v) 把字符串 v 转成整数（ASCII to int），port 接收整数、err 接收错误。
		if port, err := strconv.Atoi(v); err == nil {
			// 转换成功（err 为 nil）：用环境变量里的端口覆盖配置里的端口。
			cfg.Server.Port = port
		}
	}
	// 读取 MYSQL_HOST 环境变量，非空就覆盖数据库地址（字符串不用转换，直接赋值）。
	if v := os.Getenv("MYSQL_HOST"); v != "" {
		// 覆盖 MySQL 主机地址。
		cfg.Database.Host = v
	}
	// MYSQL_PORT 环境变量覆盖数据库端口（字符串要先 Atoi 转整数）。
	if v := os.Getenv("MYSQL_PORT"); v != "" {
		// 转成整数 port，成功才覆盖。
		if port, err := strconv.Atoi(v); err == nil {
			// 覆盖数据库端口。
			cfg.Database.Port = port
		}
	}
	// MYSQL_USER 覆盖数据库用户名。
	if v := os.Getenv("MYSQL_USER"); v != "" {
		// 覆盖数据库登录用户名。
		cfg.Database.User = v
	}
	// MYSQL_ROOT_PASSWORD 覆盖数据库密码（Docker Compose 里 MySQL 的 root 密码变量名）。
	if v := os.Getenv("MYSQL_ROOT_PASSWORD"); v != "" {
		// 覆盖数据库密码。
		cfg.Database.Password = v
	}
	// MYSQL_PASSWORD 也覆盖数据库密码（普通用户场景的变量名，后设置的生效）。
	if v := os.Getenv("MYSQL_PASSWORD"); v != "" {
		// 再次覆盖数据库密码。
		cfg.Database.Password = v
	}
	// MYSQL_DATABASE 覆盖数据库名。
	if v := os.Getenv("MYSQL_DATABASE"); v != "" {
		// 覆盖要连接的数据库名字。
		cfg.Database.DBName = v
	}
	// REDIS_HOST 覆盖 Redis 地址。
	if v := os.Getenv("REDIS_HOST"); v != "" {
		// 覆盖 Redis 主机地址。
		cfg.Redis.Host = v
	}
	// REDIS_PORT 覆盖 Redis 端口。
	if v := os.Getenv("REDIS_PORT"); v != "" {
		// 字符串转整数成功才覆盖。
		if port, err := strconv.Atoi(v); err == nil {
			// 覆盖 Redis 端口。
			cfg.Redis.Port = port
		}
	}
	// REDIS_PASSWORD 覆盖 Redis 密码。
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		// 覆盖 Redis 密码。
		cfg.Redis.Password = v
	}
	// REDIS_DB 覆盖使用 Redis 第几号库。
	if v := os.Getenv("REDIS_DB"); v != "" {
		// 转成整数。注意这里局部变量也叫 db，它只在这个 if 块内有效，不会和别的 db 冲突。
		if db, err := strconv.Atoi(v); err == nil {
			// 覆盖 Redis 库编号。
			cfg.Redis.DB = db
		}
	}
	// RABBITMQ_HOST 覆盖 RabbitMQ 地址。
	if v := os.Getenv("RABBITMQ_HOST"); v != "" {
		// 覆盖 MQ 主机地址。
		cfg.RabbitMQ.Host = v
	}
	// RABBITMQ_PORT 覆盖 RabbitMQ 端口。
	if v := os.Getenv("RABBITMQ_PORT"); v != "" {
		// 字符串转整数成功才覆盖。
		if port, err := strconv.Atoi(v); err == nil {
			// 覆盖 MQ 端口。
			cfg.RabbitMQ.Port = port
		}
	}
	// RABBITMQ_USER 覆盖 MQ 用户名。
	if v := os.Getenv("RABBITMQ_USER"); v != "" {
		// 覆盖 MQ 登录用户名。
		cfg.RabbitMQ.Username = v
	}
	// RABBITMQ_PASS 覆盖 MQ 密码。
	if v := os.Getenv("RABBITMQ_PASS"); v != "" {
		// 覆盖 MQ 登录密码。
		cfg.RabbitMQ.Password = v
	}

	// AGENT_ENABLED 覆盖是否启用 Agent（"true"/"false"）。
	if v := os.Getenv("AGENT_ENABLED"); v != "" {
		// 只有 true 时打开（其余取值按 false）。
		cfg.Agent.Enabled = v == "true"
	}
	// AGENT_MODEL 覆盖模型名。
	if v := os.Getenv("AGENT_MODEL"); v != "" {
		// 覆盖。
		cfg.Agent.Model = v
	}
	// AGENT_BASE_URL 覆盖接口基础地址。
	if v := os.Getenv("AGENT_BASE_URL"); v != "" {
		// 覆盖。
		cfg.Agent.BaseURL = v
	}
	// MODERATION_REVIEWER_IDS 覆盖审核员账号列表，逗号分隔，如 "1,7,9"。
	if v := os.Getenv("MODERATION_REVIEWER_IDS"); v != "" {
		// ids 收集解析结果。
		var ids []uint
		// strings.Split 按逗号切成多段；range 遍历。
		for _, part := range strings.Split(v, ",") {
			// TrimSpace 去掉每段首尾空白。
			part = strings.TrimSpace(part)
			// 空段跳过（如结尾多了逗号）。
			if part == "" {
				// 跳过。
				continue
			}
			// strconv.ParseUint 转成无符号整数；位宽 64。
			id, err := strconv.ParseUint(part, 10, 64)
			// 某一段不是数字：整段配置视为无效，忽略这次覆盖（保留原值）。
			if err != nil {
				// 跳出（不更新 ReviewerIDs）。
				goto reviewerDone
			}
			// 收入结果。
			ids = append(ids, uint(id))
		}
		// 全部解析成功：覆盖。
		cfg.Agent.ReviewerIDs = ids
	}
reviewerDone:
	// 注意：API Key 只在运行时按 APIKeyEnv 指定的名字从环境变量读，
	// 这里不把它放进 Config，避免 Key 随配置结构被打印/泄露。
}

// LoadLocalDev 函数的作用：加载配置的"本地开发友好版"——
// 正常读配置文件；如果配置文件【不存在】，不报错退出，而是退回使用一份内置的默认配置，让新手更容易跑起来。
//
// 参数 filename：配置文件路径；
// 返回值有三个：
//   - Config：解析出的配置（或默认配置）；
//   - bool：true 表示"文件没找到，用的是默认配置"（调用方可以据此打印提示）；
//   - error：真正的错误（文件存在但解析失败等）。
//
// bool用来表示是否使用了默认配置，true表示使用了默认配置
func LoadLocalDev(filename string) (Config, bool, error) {
	// cfg, err := Load(filename)：先尝试正常读取配置文件。
	cfg, err := Load(filename)
	// if err == nil：没有任何错误，文件读取解析成功。
	if err == nil {
		// 返回配置，false 表示"用的不是默认配置"，错误为 nil。
		return cfg, false, nil
	}
	// errors.Is(err, os.ErrNotExist)：判断错误是不是"文件不存在"（错误可能被 %w 包装过，所以用 errors.Is）。
	if errors.Is(err, os.ErrNotExist) {
		// 文件不存在：返回 DefaultLocalConfig() 生成的默认配置，true 表示"用了默认配置"，错误为 nil。
		return DefaultLocalConfig(), true, nil
	}
	// 其他错误（文件能读到但解析失败等）：返回空配置、false，并把错误交出去。
	return Config{}, false, err
}

// DefaultLocalConfig 函数的作用：生成一份写死在代码里的"本地开发默认配置"
// （本机地址 + 开发用的简单密码），配置文件缺失时用它兜底。
//
// 返回值 Config：填好默认值的总配置。
func DefaultLocalConfig() Config {
	// cfg 变量用"结构体字面量"方式一次性创建：类型名 { 字段: 值, ... }，类似 Python 里传关键字参数构造对象。
	cfg := Config{
		// Server 设置默认端口 8080。
		Server: ServerConfig{
			// Port 默认 8080。
			Port: 8080,
		},
		// Database 设置本机 MySQL 的默认连接信息。
		Database: DatabaseConfig{
			// Host 本机。
			Host: "localhost",
			// Port MySQL 默认端口 3306。
			Port: 3306,
			// User 默认 root 账号。
			User: "root",
			// Password 开发环境简单密码。
			Password: "123456",
			// DBName 默认数据库名。
			DBName: "feedsystem",
		},
		// Redis 设置本机 Redis 默认连接信息。
		Redis: RedisConfig{
			// Host 本机。
			Host: "localhost",
			// Port Redis 默认端口 6379。
			Port: 6379,
			// Password 开发环境密码。
			Password: "123456",
			// DB 默认用 0 号库。
			DB: 0,
		},
		// RabbitMQ 设置本机 MQ 默认连接信息。
		RabbitMQ: RabbitMQConfig{
			// Host 本机。
			Host: "localhost",
			// Port AMQP 默认端口 5672。
			Port: 5672,
			// Username 默认管理员名。
			Username: "admin",
			// Password 默认密码。
			Password: "password123",
		},
		// ObservabilityConfig 默认开启 pprof。
		ObservabilityConfig: ObservabilityConfig{
			// Pprof 默认配置。
			Pprof: PprofConfig{
				// Enabled 开启 pprof。
				Enabled: true,
				// ApiAddr API 性能分析地址。
				ApiAddr: "localhost:6060",
				// WorkerAddr Worker 性能分析地址。
				WorkerAddr: "localhost:6061",
			},
		},
		// Agent 默认【不启用】：没有 Key 也能正常跑，想看 AI 判定再打开。
		Agent: AgentConfig{
			// Enabled 默认关。
			Enabled: false,
			// Provider OpenAI 兼容接口。
			Provider: "openai_compatible",
			// Model 默认模型名（按需替换成自己账号支持的模型）。
			Model: "gpt-4o-mini",
			// BaseURL OpenAI 官方地址；用第三方兼容服务时改成对应地址。
			BaseURL: "https://api.openai.com/v1",
			// APIKeyEnv 从 AGENT_API_KEY 取 Key。
			APIKeyEnv: "AGENT_API_KEY",
			// MaxRounds 默认 4 轮。
			MaxRounds: 4,
			// TimeoutSeconds 硬超时 20 秒。
			TimeoutSeconds: 20,
			// Thresholds 默认门槛。
			Thresholds: ThresholdConfig{
				// Remove 0.85。
				Remove: 0.85,
				// Warn 0.7。
				Warn: 0.7,
			},
			// ReviewerIDs 默认把 1 号账号当审核员（本地首个注册的账号通常是 1）。
			ReviewerIDs: []uint{1},
		},
	}
	// ApplyEnvOverrides(&cfg)：即使是默认配置，也允许环境变量覆盖（保持两种入口行为一致）。
	ApplyEnvOverrides(&cfg)
	// return 返回最终的默认配置。
	return cfg
}
