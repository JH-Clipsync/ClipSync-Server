package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
