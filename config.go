package main

import (
	"embed"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultConfigFS 把 config.default.yaml 编译进二进制。
// 所有默认值都写在那份 YAML 里，Go 代码里不再有硬编码的配置值——
// 想知道某项默认是什么，直接看 config.default.yaml 即可。
//
//go:embed config.default.yaml
var defaultConfigFS embed.FS

// DefaultConfigYAML 返回内置默认配置的原文，供 `--print-default-config` 输出。
func DefaultConfigYAML() []byte {
	data, err := defaultConfigFS.ReadFile("config.default.yaml")
	if err != nil {
		// go:embed 保证文件存在，读不到只可能是构建被破坏
		panic(fmt.Sprintf("内置默认配置缺失: %v", err))
	}
	return data
}

// Config 服务端配置。从 YAML 文件加载，未设置的字段走默认值；
// 部分字段还支持通过环境变量覆盖（环境变量优先级最高）。
type Config struct {
	// Server HTTP 监听配置
	Server ServerConfig `yaml:"server"`

	// Logs 日志相关
	Logs LogsConfig `yaml:"logs"`

	// WebSocket 调优
	WebSocket WebSocketConfig `yaml:"websocket"`

	// MessageProtocol 协议级参数（保留扩展位）
	MessageProtocol MessageProtocolConfig `yaml:"message_protocol"`

	// MySQL 用户 / 会话持久化
	MySQL MySQLConfig `yaml:"mysql"`

	// Redis token 缓存与在线态
	Redis RedisConfig `yaml:"redis"`

	// Auth 认证策略（token 有效期、是否开放注册等）
	Auth AuthConfig `yaml:"auth"`

	// E2EE 端到端加密策略
	E2EE E2EEConfig `yaml:"e2ee"`
}

// ServerConfig HTTP 服务监听
type ServerConfig struct {
	// Addr 监听地址，例如 ":28001" 或 "0.0.0.0:9000"
	Addr string `yaml:"addr"`

	// ReadTimeout HTTP 读超时
	ReadTimeout time.Duration `yaml:"read_timeout"`

	// WriteTimeout HTTP 写超时
	WriteTimeout time.Duration `yaml:"write_timeout"`

	// ShutdownTimeout 优雅退出等待时间
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// TrustProxy 是否信任反代头（X-Forwarded-For / X-Real-IP），启用后才能正确拿到真实 IP
	TrustProxy bool `yaml:"trust_proxy"`

	// AdminToken 管理端通过 HTTP 调用 /server-admin/* 接口（kick、设备禁用等）时携带的
	// Bearer Token。留空则 /server-admin/* 接口一律返回 401，避免未授权访问。
	AdminToken string `yaml:"admin_token"`
}

// LogsConfig 日志相关
type LogsConfig struct {
	// Dir 日志根目录
	Dir string `yaml:"dir"`

	// Level 日志级别：debug / info / warn / error
	Level string `yaml:"level"`

	// MaxAgeDays 归档日志保留天数（0 表示不限）
	MaxAgeDays int `yaml:"max_age_days"`

	// Stdout 是否同时输出到 stdout（容器里一般 true）
	Stdout bool `yaml:"stdout"`
}

// WebSocketConfig WebSocket 调优
type WebSocketConfig struct {
	// ReadLimit 单条消息最大字节（剪贴板图片可能很大，默认 10MB）
	ReadLimit int64 `yaml:"read_limit"`

	// ReadDeadline 读超时（秒）
	ReadDeadlineSec int `yaml:"read_deadline_sec"`

	// WriteDeadline 写超时（秒）
	WriteDeadlineSec int `yaml:"write_deadline_sec"`

	// PingInterval 心跳间隔（秒）
	PingIntervalSec int `yaml:"ping_interval_sec"`

	// SendQueueSize 每个客户端的发送队列长度
	SendQueueSize int `yaml:"send_queue_size"`
}

// MessageProtocolConfig 协议级参数
type MessageProtocolConfig struct {
	// CheckOrigin 是否允许跨域 WebSocket（true = 允许任意 Origin，调试用；生产建议改成白名单）
	CheckOrigin bool `yaml:"check_origin"`

	// MaxPayloadPreview 推送消息预览最大字符数
	MaxPayloadPreview int `yaml:"max_payload_preview"`
}

// MySQLConfig 用户 / 会话存储
type MySQLConfig struct {
	// Host / Port / User / Password / Database 组成 DSN
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`

	// Params DSN 附加参数，默认带上 parseTime / charset
	Params string `yaml:"params"`

	MaxOpenConns       int `yaml:"max_open_conns"`
	MaxIdleConns       int `yaml:"max_idle_conns"`
	ConnMaxLifetimeSec int `yaml:"conn_max_lifetime_sec"`
}

// DSN 拼出 go-sql-driver/mysql 的连接串
func (c MySQLConfig) DSN() string {
	params := c.Params
	if params == "" {
		params = "charset=utf8mb4&parseTime=true&loc=Local"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		c.User, c.Password, c.Host, c.Port, c.Database, params)
}

// RedisConfig token 缓存 / 在线态
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`

	// KeyPrefix 所有 key 的统一前缀，方便和别的业务共用实例
	KeyPrefix string `yaml:"key_prefix"`

	// OnlineTTLSec 在线登记的 TTL（秒）。心跳会定期续期，进程被 kill 后自然过期。
	OnlineTTLSec int `yaml:"online_ttl_sec"`
}

// AuthConfig 认证策略
type AuthConfig struct {
	// TokenTTLHours token 有效期（小时）
	TokenTTLHours int `yaml:"token_ttl_hours"`

	// AllowRegister 是否开放 /auth/register
	AllowRegister bool `yaml:"allow_register"`

	// MinPasswordLen 注册时的最小密码长度
	MinPasswordLen int `yaml:"min_password_len"`

	// BootstrapUser / BootstrapPassword 启动时自动创建的初始账号（已存在则跳过）。
	// 留空表示不创建。方便 docker 首次部署直接可用。
	BootstrapUser     string `yaml:"bootstrap_user"`
	BootstrapPassword string `yaml:"bootstrap_password"`

	// LoginRateLimitPerMin 单 IP 每分钟允许的登录尝试次数（0 = 不限）
	LoginRateLimitPerMin int `yaml:"login_rate_limit_per_min"`
}

// E2EEConfig 端到端加密策略
type E2EEConfig struct {
	// Require 为 true 时，服务端拒绝转发未加密（明文 payload）的消息。
	// 客户端全部升级完成后建议打开。
	Require bool `yaml:"require"`
}

// DefaultConfig 解析内置的 config.default.yaml，返回填好默认值的 Config。
// 默认值只有那一处来源，改默认行为就改那份 YAML。
func DefaultConfig() *Config {
	cfg := &Config{}
	if err := yaml.Unmarshal(DefaultConfigYAML(), cfg); err != nil {
		// 默认配置是随二进制一起编译的，解析失败属于构建期错误，
		// 由单测（TestDefaultConfigParses）兜住，不该出现在运行时。
		panic(fmt.Sprintf("内置默认配置解析失败: %v", err))
	}
	return cfg
}

// LoadConfig 从 YAML 文件加载配置，缺失字段用默认值补齐，最后再被环境变量覆盖。
// 配置文件路径查找顺序：
//  1. --config 命令行参数
//  2. CLIPSYNC_CONFIG 环境变量
//  3. ./config.yaml
//  4. /etc/clipsync/config.yaml
//
// 文件不存在不报错，直接用默认值（方便 docker 启动时只通过环境变量配置）。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		path = resolveConfigPath()
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
			}
			fmt.Printf("📄 已加载配置: %s\n", path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		// 文件不存在：保持默认值即可
	}

	applyEnvOverrides(cfg)
	return cfg, nil
}

// resolveConfigPath 按优先级找配置文件
func resolveConfigPath() string {
	if v := os.Getenv("CLIPSYNC_CONFIG"); v != "" {
		return v
	}
	for _, p := range []string{"config.yaml", "/etc/clipsync/config.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// applyEnvOverrides 用环境变量覆盖关键字段
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CLIPSYNC_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("CLIPSYNC_LOG_DIR"); v != "" {
		cfg.Logs.Dir = v
	}
	if v := os.Getenv("CLIPSYNC_LOG_LEVEL"); v != "" {
		cfg.Logs.Level = strings.ToLower(v)
	}
	if v := os.Getenv("CLIPSYNC_TRUST_PROXY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Server.TrustProxy = b
		}
	}
	if v := os.Getenv("CLIPSYNC_WS_READ_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.WebSocket.ReadLimit = n
		}
	}

	// ===== 中间件（MySQL / Redis）=====
	//
	// 有意不提供环境变量覆盖：连接信息全部由 config.yaml 的 mysql / redis 段决定，
	// 改文件 + 重启即可生效，不必再去翻 compose 或 shell 里的 export。
	// 这样也避免了"改了配置文件却被残留环境变量悄悄盖掉"这类难查的问题。

	// ===== Auth =====
	if v := os.Getenv("CLIPSYNC_TOKEN_TTL_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Auth.TokenTTLHours = n
		}
	}
	if v := os.Getenv("CLIPSYNC_ALLOW_REGISTER"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Auth.AllowRegister = b
		}
	}
	if v := os.Getenv("CLIPSYNC_BOOTSTRAP_USER"); v != "" {
		cfg.Auth.BootstrapUser = v
	}
	if v := os.Getenv("CLIPSYNC_BOOTSTRAP_PASSWORD"); v != "" {
		cfg.Auth.BootstrapPassword = v
	}

	// ===== E2EE =====
	if v := os.Getenv("CLIPSYNC_E2EE_REQUIRE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.E2EE.Require = b
		}
	}
}
