package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore 承担两件事：
//  1. token 校验缓存：token_hash → user_id，避免每次 WS 握手都打 MySQL
//  2. 在线设备登记：user_id → {device_id: role}，用于判断"当前用户是否已有客户端登录"
//
// 在线登记用 Hash + TTL 续期，进程被 kill 也会自然过期，不会留下幽灵在线记录。
type RedisStore struct {
	rdb    *redis.Client
	prefix string
}

// OpenRedis 建连并 Ping。
func OpenRedis(cfg RedisConfig) (*RedisStore, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	prefix := cfg.KeyPrefix
	if prefix == "" {
		prefix = "clipsync:"
	}
	return &RedisStore{rdb: rdb, prefix: prefix}, nil
}

func (s *RedisStore) Close() error { return s.rdb.Close() }

func (s *RedisStore) tokenKey(tokenHash string) string { return s.prefix + "token:" + tokenHash }
func (s *RedisStore) userTokenKey(userID int64) string {
	return s.prefix + "user_token:" + strconv.FormatInt(userID, 10)
}
func (s *RedisStore) onlineKey(userID int64) string {
	return s.prefix + "online:" + strconv.FormatInt(userID, 10)
}
func (s *RedisStore) plainTokenKey(userID int64) string {
	return s.prefix + "plain_token:" + strconv.FormatInt(userID, 10)
}

// ===== token 缓存 =====

// CacheToken 写入 token_hash → user_id，同时记录 user_id → token_hash 反向索引。
// ttl 取会话剩余有效期。
func (s *RedisStore) CacheToken(ctx context.Context, tokenHash string, userID int64, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.tokenKey(tokenHash), userID, ttl)
	pipe.Set(ctx, s.userTokenKey(userID), tokenHash, ttl)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("缓存 token 失败: %w", err)
	}
	return nil
}

// LookupToken 查 token 对应的 user_id。未命中返回 (0, false, nil)。
func (s *RedisStore) LookupToken(ctx context.Context, tokenHash string) (int64, bool, error) {
	v, err := s.rdb.Get(ctx, s.tokenKey(tokenHash)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询 token 缓存失败: %w", err)
	}
	return v, true, nil
}

// DropToken 清掉某个 token 的缓存（登出 / 强制下线）。
func (s *RedisStore) DropToken(ctx context.Context, tokenHash string, userID int64) error {
	if err := s.rdb.Del(ctx, s.tokenKey(tokenHash), s.userTokenKey(userID)).Err(); err != nil {
		return fmt.Errorf("清理 token 缓存失败: %w", err)
	}
	return nil
}

// ===== 明文 token 暂存 =====
//
// 「已有客户端登录时返回同一个 token」这条规则要求服务端能把原 token 再交给新设备，
// 而 MySQL 里只存哈希（拖库也拿不到）。折中做法：明文只放 Redis，带会话同长的 TTL，
// 掉了就重新签发（老设备下次握手失败后自动重登）。

// StorePlainToken 暂存明文 token。
func (s *RedisStore) StorePlainToken(ctx context.Context, userID int64, token string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	if err := s.rdb.Set(ctx, s.plainTokenKey(userID), token, ttl).Err(); err != nil {
		return fmt.Errorf("暂存 token 失败: %w", err)
	}
	return nil
}

// LoadPlainToken 取回明文 token；不存在返回 ("", false, nil)。
func (s *RedisStore) LoadPlainToken(ctx context.Context, userID int64) (string, bool, error) {
	v, err := s.rdb.Get(ctx, s.plainTokenKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取暂存 token 失败: %w", err)
	}
	return v, true, nil
}

// DropPlainToken 清掉暂存的明文 token。
func (s *RedisStore) DropPlainToken(ctx context.Context, userID int64) error {
	return s.rdb.Del(ctx, s.plainTokenKey(userID)).Err()
}

// ===== 在线设备登记 =====

// MarkOnline 登记一台在线设备，并刷新整个 Hash 的 TTL。
func (s *RedisStore) MarkOnline(ctx context.Context, userID int64, deviceID, role string, ttl time.Duration) error {
	key := s.onlineKey(userID)
	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, key, deviceID, role)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("登记在线设备失败: %w", err)
	}
	return nil
}

// TouchOnline 心跳续期：设备还在线时定期调用，防止 Hash 过期。
func (s *RedisStore) TouchOnline(ctx context.Context, userID int64, ttl time.Duration) error {
	return s.rdb.Expire(ctx, s.onlineKey(userID), ttl).Err()
}

// MarkOffline 摘掉一台设备；Hash 空了就直接删 key。
func (s *RedisStore) MarkOffline(ctx context.Context, userID int64, deviceID string) error {
	key := s.onlineKey(userID)
	if err := s.rdb.HDel(ctx, key, deviceID).Err(); err != nil {
		return fmt.Errorf("移除在线设备失败: %w", err)
	}
	n, err := s.rdb.HLen(ctx, key).Result()
	if err == nil && n == 0 {
		s.rdb.Del(ctx, key)
	}
	return nil
}

// OnlineCount 当前用户在线设备数。登录接口用它判断"是否已有客户端登录"。
func (s *RedisStore) OnlineCount(ctx context.Context, userID int64) (int64, error) {
	n, err := s.rdb.HLen(ctx, s.onlineKey(userID)).Result()
	if err != nil {
		return 0, fmt.Errorf("查询在线设备数失败: %w", err)
	}
	return n, nil
}

// OnlineDevices 返回 device_id → role，用于 /session 接口展示。
func (s *RedisStore) OnlineDevices(ctx context.Context, userID int64) (map[string]string, error) {
	m, err := s.rdb.HGetAll(ctx, s.onlineKey(userID)).Result()
	if err != nil {
		return nil, fmt.Errorf("查询在线设备失败: %w", err)
	}
	return m, nil
}

// ===== 管理端事件通道 =====
//
// 管理端（ClipSync-Admin）和服务端共享同一个 Redis 实例时，
// 用 Pub/Sub 解耦跨服务通知。管理端是发布方，服务端是订阅方。
//
// 频道：{prefix}admin:kick_user
// 消息体支持两种格式（订阅方都能解析）：
//  1. 纯数字 userID（向后兼容）：等价于 {"user_id":id,"action":"kick_user"}
//  2. JSON：AdminCommand
//     - action=kick_user：踢该用户所有设备
//     - action=kick_device：踢该用户指定 device_id 的设备
//     - action=disable_device：禁用设备并踢它
//     - action=enable_device：解禁设备（不影响在线连接）

// AdminAction 管理端下发的控制动作
type AdminAction string

const (
	AdminActionKickUser       AdminAction = "kick_user"
	AdminActionKickDevice     AdminAction = "kick_device"
	AdminActionDisableDevice  AdminAction = "disable_device"
	AdminActionEnableDevice   AdminAction = "enable_device"
)

// AdminCommand 管理端通过 Redis/HTTP 发给 Server 的指令。
type AdminCommand struct {
	Action   AdminAction `json:"action"`
	UserID   int64       `json:"user_id"`
	DeviceID string      `json:"device_id,omitempty"`
	Reason   string      `json:"reason,omitempty"`
}

// KickUserChannel 返回管理端强制下线的 Pub/Sub 频道名。
func (s *RedisStore) KickUserChannel() string {
	return s.prefix + "admin:kick_user"
}

// publishCommand 向管理端频道发布一条指令。
func (s *RedisStore) publishCommand(ctx context.Context, cmd AdminCommand) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("编码管理端指令失败: %w", err)
	}
	return s.rdb.Publish(ctx, s.KickUserChannel(), data).Err()
}

// PublishKickUser 向管理端频道发布"踢该用户所有设备"事件。
// 由 ClipSync-Admin 调用；ClipSync-Server 自己在改密码时也会发。
func (s *RedisStore) PublishKickUser(ctx context.Context, userID int64, reason string) error {
	return s.publishCommand(ctx, AdminCommand{
		Action: AdminActionKickUser,
		UserID: userID,
		Reason: reason,
	})
}

// PublishKickDevice 向管理端频道发布"踢指定设备"事件。
func (s *RedisStore) PublishKickDevice(ctx context.Context, userID int64, deviceID, reason string) error {
	return s.publishCommand(ctx, AdminCommand{
		Action:   AdminActionKickDevice,
		UserID:   userID,
		DeviceID: deviceID,
		Reason:   reason,
	})
}

// PublishDeviceStatus 向管理端频道发布"启用/禁用设备"事件。
// 禁用时附带踢下线动作。
func (s *RedisStore) PublishDeviceStatus(ctx context.Context, userID int64, deviceID string, disabled bool, reason string) error {
	action := AdminActionEnableDevice
	if disabled {
		action = AdminActionDisableDevice
	}
	return s.publishCommand(ctx, AdminCommand{
		Action:   action,
		UserID:   userID,
		DeviceID: deviceID,
		Reason:   reason,
	})
}

// SubscribeAdminCommands 订阅管理端控制事件，阻塞运行。
// 每收到一条指令就调用 onCommand；ctx 取消时退出。
// 重连由调用方负责，本方法内部不做无限重试。
//
// 为了向后兼容，纯数字 userID 的消息会被翻译成 kick_user 指令。
func (s *RedisStore) SubscribeAdminCommands(ctx context.Context, onCommand func(AdminCommand)) error {
	sub := s.rdb.Subscribe(ctx, s.KickUserChannel())
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			cmd, ok := parseAdminCommand(msg.Payload)
			if !ok {
				continue
			}
			onCommand(cmd)
		}
	}
}

// parseAdminCommand 解析管理端消息，支持 JSON 指令和纯数字 userID。
func parseAdminCommand(payload string) (AdminCommand, bool) {
	var cmd AdminCommand
	if err := json.Unmarshal([]byte(payload), &cmd); err == nil && cmd.UserID > 0 && cmd.Action != "" {
		return cmd, true
	}
	// 兼容旧格式：纯数字 userID
	if userID, err := strconv.ParseInt(strings.TrimSpace(payload), 10, 64); err == nil && userID > 0 {
		return AdminCommand{Action: AdminActionKickUser, UserID: userID, Reason: KickReasonPasswordReset}, true
	}
	return AdminCommand{}, false
}
