package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// CachedSMS 旁路缓存的一条入站短信。**只含端到端密文信封**，服务端无法解密，
// 明文（含发件号码、验证码正文）由持有同步密码的调用方取回后自行解密。
type CachedSMS struct {
	ID         string          `json:"id"`                 // 客户端消息 ID
	FromDevice string          `json:"from_device"`        // 发送方设备 ID（mobile）
	Type       string          `json:"type"`               // 原始消息类型（notify_pc）
	TS         int64           `json:"ts"`                 // 客户端消息时间戳（秒）
	ReceivedAt int64           `json:"received_at"`        // 服务端旁路缓存时间（秒）
	Sender     string          `json:"sender,omitempty"`   // 短信发送号码（密文外明文元数据）
	Receiver   string          `json:"receiver,omitempty"` // 短信接收号码/本机接码卡号（密文外明文元数据）
	Payload    json.RawMessage `json:"payload"`            // 原始 payload，内含 enc 密文信封
}

func (s *RedisStore) smsCacheKey(userID int64) string {
	return s.prefix + "sms_cache:" + strconv.FormatInt(userID, 10)
}

// CacheSMSCipher 旁路缓存一条入站短信的密文。
// ZSET：score=服务端收到时间，member=整条 JSON（内含唯一 id，窗口内不会碰撞）。
// 写入后清理窗口外旧成员、限制最大条数，key 再带兜底 TTL。
func (s *RedisStore) CacheSMSCipher(ctx context.Context, userID int64, sms CachedSMS, ttl time.Duration, maxItems int) error {
	if ttl <= 0 || maxItems <= 0 {
		return nil
	}
	raw, err := json.Marshal(sms)
	if err != nil {
		return fmt.Errorf("序列化短信缓存失败: %w", err)
	}
	key := s.smsCacheKey(userID)
	now := time.Now().Unix()
	ttlSec := int64(ttl / time.Second)
	if ttlSec <= 0 {
		ttlSec = 600
	}

	pipe := s.rdb.TxPipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: string(raw)})
	pipe.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", now-ttlSec))
	pipe.ZRemRangeByRank(ctx, key, 0, int64(-maxItems-1)) // 只保留最新 maxItems 条
	pipe.Expire(ctx, key, ttl+time.Minute)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("写入短信缓存失败: %w", err)
	}
	return nil
}

// RecentSMSCipher 取某用户最近的短信密文（按服务端收到时间倒序），并顺手清理过期成员。
func (s *RedisStore) RecentSMSCipher(ctx context.Context, userID int64, ttl time.Duration, limit int) ([][]byte, error) {
	if limit <= 0 {
		limit = 20
	}
	key := s.smsCacheKey(userID)
	now := time.Now().Unix()
	ttlSec := int64(ttl / time.Second)
	if ttlSec <= 0 {
		ttlSec = 600
	}
	pipe := s.rdb.TxPipeline()
	pipe.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", now-ttlSec))
	res := pipe.ZRevRange(ctx, key, 0, int64(limit-1))
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("读取短信缓存失败: %w", err)
	}
	members := res.Val()
	out := make([][]byte, 0, len(members))
	for _, m := range members {
		out = append(out, []byte(m))
	}
	return out, nil
}
