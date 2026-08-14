package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDefaultConfigParses 守住内置默认配置：
// config.default.yaml 一旦写错缩进或类型，这里立刻炸，而不是等到线上启动。
func TestDefaultConfigParses(t *testing.T) {
	cfg := DefaultConfig()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"server.addr", cfg.Server.Addr, ":28001"},
		{"server.read_timeout", cfg.Server.ReadTimeout, 15 * time.Second},
		{"server.shutdown_timeout", cfg.Server.ShutdownTimeout, 10 * time.Second},
		{"logs.dir", cfg.Logs.Dir, "logs"},
		{"logs.level", cfg.Logs.Level, "info"},
		{"logs.stdout", cfg.Logs.Stdout, true},
		{"websocket.read_limit", cfg.WebSocket.ReadLimit, int64(10485760)},
		{"websocket.send_queue_size", cfg.WebSocket.SendQueueSize, 32},
		{"message_protocol.check_origin", cfg.MessageProtocol.CheckOrigin, true},
		{"message_protocol.max_payload_preview", cfg.MessageProtocol.MaxPayloadPreview, 40},
		{"mysql.host", cfg.MySQL.Host, "127.0.0.1"},
		{"mysql.port", cfg.MySQL.Port, 3306},
		{"mysql.database", cfg.MySQL.Database, "clipsync"},
		{"mysql.max_open_conns", cfg.MySQL.MaxOpenConns, 20},
		{"redis.addr", cfg.Redis.Addr, "127.0.0.1:6379"},
		{"redis.key_prefix", cfg.Redis.KeyPrefix, "clipsync:"},
		{"redis.online_ttl_sec", cfg.Redis.OnlineTTLSec, 90},
        {"auth.token_ttl_hours", cfg.Auth.TokenTTLHours, 720},
        {"auth.allow_register", cfg.Auth.AllowRegister, false},
        {"auth.min_password_len", cfg.Auth.MinPasswordLen, 8},
		{"auth.login_rate_limit_per_min", cfg.Auth.LoginRateLimitPerMin, 10},
		{"e2ee.require", cfg.E2EE.Require, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("默认配置 %s = %v，期望 %v", c.name, c.got, c.want)
		}
	}
}

// TestLoadConfigOverridesDefaults 验证用户 config.yaml 只需写想改的字段，
// 其余仍从内置默认值补齐（部分覆盖，不是整体替换）。
func TestLoadConfigOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
mysql:
  host: "db.internal"
  port: 13306
  password: "s3cret"
redis:
  addr: "cache.internal:16379"
e2ee:
  require: true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}

	// 显式写了的字段应被覆盖
	if cfg.MySQL.Host != "db.internal" || cfg.MySQL.Port != 13306 || cfg.MySQL.Password != "s3cret" {
		t.Errorf("mysql 覆盖失败: %+v", cfg.MySQL)
	}
	if cfg.Redis.Addr != "cache.internal:16379" {
		t.Errorf("redis.addr 覆盖失败: %q", cfg.Redis.Addr)
	}
	if !cfg.E2EE.Require {
		t.Error("e2ee.require 应被覆盖为 true")
	}

	// 没写的字段应保留默认值
	if cfg.MySQL.User != "clipsync" {
		t.Errorf("mysql.user 应保留默认值 clipsync，实际 %q", cfg.MySQL.User)
	}
	if cfg.MySQL.MaxOpenConns != 20 {
		t.Errorf("mysql.max_open_conns 应保留默认 20，实际 %d", cfg.MySQL.MaxOpenConns)
	}
	if cfg.Redis.KeyPrefix != "clipsync:" {
		t.Errorf("redis.key_prefix 应保留默认值，实际 %q", cfg.Redis.KeyPrefix)
	}
	if cfg.Server.Addr != ":28001" {
		t.Errorf("server.addr 应保留默认 :28001，实际 %q", cfg.Server.Addr)
	}
}

// TestMiddlewareIgnoresEnv 中间件连接信息只认配置文件：
// 即使环境里残留 CLIPSYNC_MYSQL_HOST 之类，也不该悄悄盖掉文件里的值。
func TestMiddlewareIgnoresEnv(t *testing.T) {
	t.Setenv("CLIPSYNC_MYSQL_HOST", "should-be-ignored")
	t.Setenv("CLIPSYNC_MYSQL_PORT", "19999")
	t.Setenv("CLIPSYNC_MYSQL_PASSWORD", "env-password")
	t.Setenv("CLIPSYNC_REDIS_ADDR", "ignored:6379")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
mysql:
  host: "from-file"
  port: 3307
  password: "file-password"
redis:
  addr: "from-file:6379"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.MySQL.Host != "from-file" || cfg.MySQL.Port != 3307 || cfg.MySQL.Password != "file-password" {
		t.Errorf("mysql 连接信息被环境变量污染: %+v", cfg.MySQL)
	}
	if cfg.Redis.Addr != "from-file:6379" {
		t.Errorf("redis.addr 被环境变量污染: %q", cfg.Redis.Addr)
	}
}
