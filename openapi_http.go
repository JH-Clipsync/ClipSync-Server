package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseIntStrict 严格解析整数（不接受空白/小数/带尾字符）。
func parseIntStrict(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// sqlNullTime 把时间包成 sql.NullTime。
func sqlNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}

// nullableTimeRFC3339 把可空时间格式化成 RFC3339 字符串，NULL 返回空串。
func nullableTimeRFC3339(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(time.RFC3339)
}

// openAPILimiter 开放接口按 IP 限流（独立于登录限流），main 启动时初始化。
var openAPILimiter *loginLimiter

const (
	apiKeyPrefix  = "cs_live_"
	apiKeyRandLen = 24 // 随机字节数；hex 后 48 字符
)

// generateAPIKey 生成明文密钥 cs_live_<48hex> 与其 SHA-256 哈希、辨识前缀。
func generateAPIKey() (plaintext, hash, short string, err error) {
	buf := make([]byte, apiKeyRandLen)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plaintext = apiKeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	// 辨识前缀：cs_live_ + 前 6 位，如 cs_live_ab12cd…
	short = plaintext[:len(apiKeyPrefix)+6] + "…"
	return plaintext, hash, short, nil
}

// normalizePhoneQuery 归一化号码：去空白/连字符/括号等分隔符，字母转小写，只保留字母数字。
func normalizePhoneQuery(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	return b.String()
}

// phoneEqual 判断两个号码是否同一号：归一化后相等，或去掉 86 国家码前缀后相等
// （兼容 138… 与 86138…/＋86138… 写法；发件短号不含 86 前缀时走第一条直接比较）。
func phoneEqual(actual, query string) bool {
	a := normalizePhoneQuery(actual)
	q := normalizePhoneQuery(query)
	if a == "" || q == "" {
		return false
	}
	if a == q {
		return true
	}
	stripCN := func(x string) string {
		if strings.HasPrefix(x, "86") && len(x) == 13 {
			return x[2:]
		}
		return x
	}
	return stripCN(a) == stripCN(q)
}

// bearerOrHeader 从 Authorization: Bearer *** X-API-Key 取密钥。
func extractAPIKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// ==================== 对外开放接口 ====================

// openAPISMSItem 单条短信返回项。payload 内仍是端到端密文信封；
// sender/receiver 是信封外明文号码元数据，仅用于服务端按号码筛选。
type openAPISMSItem struct {
	ID         string          `json:"id"`
	FromDevice string          `json:"from_device"`
	Type       string          `json:"type"`
	TS         int64           `json:"ts"`
	ReceivedAt int64           `json:"received_at"`
	Sender     string          `json:"sender,omitempty"`
	Receiver   string          `json:"receiver,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// openAPIQuerySMS GET /openapi/v1/sms
// 鉴权：ApiKey（X-API-Key 或 Bearer）。返回该用户最近短信的「密文」列表，服务端不解密。
func openAPIQuerySMS(w http.ResponseWriter, r *http.Request) {
	if globalConfig == nil || !globalConfig.PublicAPI.Enabled {
		writeErr(w, http.StatusServiceUnavailable, "验证码查询接口未开启")
		return
	}
	ip := extractIP(r)
	if openAPILimiter != nil && !openAPILimiter.allow(ip) {
		logWarn("⚠ 开放接口限流：ip=%s 超过每分钟上限", ip)
		writeErr(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
		return
	}

	plain := extractAPIKey(r)
	if plain == "" || globalStore == nil || globalCache == nil {
		writeErr(w, http.StatusUnauthorized, "缺少 API Key")
		return
	}
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	key, err := globalStore.GetAPIKeyByHash(ctx, hash)
	if err != nil || key.Disabled {
		writeErr(w, http.StatusUnauthorized, "API Key 无效")
		return
	}
	if key.ExpiresAt.Valid && time.Now().After(key.ExpiresAt.Time) {
		writeErr(w, http.StatusUnauthorized, "API Key 已过期")
		return
	}

	// 密钥比较用恒定时间，降低时序侧信道
	if subtle.ConstantTimeCompare([]byte(key.KeyHash), []byte(hash)) != 1 {
		writeErr(w, http.StatusUnauthorized, "API Key 无效")
		return
	}

	ttl := globalConfig.PublicAPI.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	limit := globalConfig.PublicAPI.Limit
	if limit <= 0 {
		limit = 20
	}
	// 允许调用方用 ?limit= 缩小范围（不超过配置上限）
	if q := strings.TrimSpace(r.URL.Query().Get("limit")); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n < limit {
			limit = n
		}
	}
	receiverFilter := normalizePhoneQuery(r.URL.Query().Get("receiver"))
	if receiverFilter == "" {
		// 兼容旧语义：phone 表示要查的本机收端卡号
		receiverFilter = normalizePhoneQuery(r.URL.Query().Get("phone"))
	}
	senderFilter := normalizePhoneQuery(r.URL.Query().Get("sender"))

	// 号码过滤在服务端完成，先取该用户 TTL 窗口内的完整短缓存，再过滤截断。
	// 不能只取 limit 条后过滤，否则最新 N 条不匹配时会漏掉稍早的目标短信。
	fetchLimit := globalConfig.PublicAPI.MaxItems
	if fetchLimit < limit {
		fetchLimit = limit
	}
	if fetchLimit <= 0 {
		fetchLimit = 20
	}

	raw, err := globalCache.RecentSMSCipher(ctx, key.UserID, ttl, fetchLimit)
	if err != nil {
		logError("开放接口读取短信缓存失败 user=%d: %v", key.UserID, err)
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}

	items := make([]openAPISMSItem, 0, len(raw))
	for _, b := range raw {
		var c CachedSMS
		if err := json.Unmarshal(b, &c); err != nil {
			continue
		}
		// 若密钥限定了移动设备，只返回该设备的短信
		if key.DeviceID != "" && c.FromDevice != key.DeviceID {
			continue
		}
		// 按收端卡号（本机接码号）/ 发端号码在服务端明文元数据上过滤
		if receiverFilter != "" && !phoneEqual(c.Receiver, receiverFilter) {
			continue
		}
		if senderFilter != "" && !phoneEqual(c.Sender, senderFilter) {
			continue
		}
		items = append(items, openAPISMSItem{
			ID:         c.ID,
			FromDevice: c.FromDevice,
			Type:       c.Type,
			TS:         c.TS,
			ReceivedAt: c.ReceivedAt,
			Sender:     c.Sender,
			Receiver:   c.Receiver,
			Payload:    c.Payload,
		})
		if len(items) >= limit {
			break
		}
	}

	globalStore.TouchAPIKeyUsed(ctx, key.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": key.DeviceID,
		"count":     len(items),
		"items":     items,
	})
}

// ==================== ApiKey 管理（server-admin） ====================

type createAPIKeyReq struct {
	Name      string `json:"name"`
	DeviceID  string `json:"device_id"`
	ExpiresAt string `json:"expires_at"` // RFC3339；留空表示无限期
}

// adminCreateAPIKey POST /server-admin/users/{id}/api-keys
func adminCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	userID, ok := int64PathParam(w, r, "id")
	if !ok {
		return
	}
	var body createAPIKeyReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不合法")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 用户必须存在
	if _, err := globalStore.FindUserByID(ctx, userID); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}

	plaintext, hash, short, err := generateAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}

	k := &APIKey{
		UserID:    userID,
		KeyHash:   hash,
		KeyPrefix: short,
		Name:      strings.TrimSpace(body.Name),
		DeviceID:  strings.TrimSpace(body.DeviceID),
	}
	if strings.TrimSpace(body.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ExpiresAt))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "expires_at 需为 RFC3339 时间，或留空表示无限期")
			return
		}
		k.ExpiresAt = sqlNullTime(t)
	}

	id, err := globalStore.CreateAPIKey(ctx, k)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	logError("🔑 已为用户 %d 创建开放接口 ApiKey id=%d", userID, id)
	// 明文密钥仅此一次返回
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"api_key":    plaintext,
		"key_prefix": short,
		"name":       k.Name,
		"device_id":  k.DeviceID,
		"expires_at": nullableTimeRFC3339(k.ExpiresAt),
		"created_at": time.Now().Format(time.RFC3339),
		"hint":       "请立即妥善保存 api_key，它只会显示这一次。",
	})
}

// apiKeyAdminView 列表/管理视图，不含任何密钥材料。
type apiKeyAdminView struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	KeyPrefix  string `json:"key_prefix"`
	Name       string `json:"name"`
	DeviceID   string `json:"device_id"`
	ExpiresAt  string `json:"expires_at"`
	Disabled   bool   `json:"disabled"`
	Expired    bool   `json:"expired"`
	LastUsedAt string `json:"last_used_at"`
	CreatedAt  string `json:"created_at"`
}

func toAPIKeyView(k *APIKey) apiKeyAdminView {
	expired := k.ExpiresAt.Valid && time.Now().After(k.ExpiresAt.Time)
	return apiKeyAdminView{
		ID:         k.ID,
		UserID:     k.UserID,
		Username:   k.Username,
		KeyPrefix:  k.KeyPrefix,
		Name:       k.Name,
		DeviceID:   k.DeviceID,
		ExpiresAt:  nullableTimeRFC3339(k.ExpiresAt),
		Disabled:   k.Disabled,
		Expired:    expired,
		LastUsedAt: nullableTimeRFC3339(k.LastUsedAt),
		CreatedAt:  k.CreatedAt.Format(time.RFC3339),
	}
}

// adminListAPIKeys GET /server-admin/users/{id}/api-keys
func adminListAPIKeys(w http.ResponseWriter, r *http.Request) {
	userID, ok := int64PathParam(w, r, "id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := globalStore.ListAPIKeys(ctx, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiKeyAdminView, 0, len(list))
	for _, k := range list {
		out = append(out, toAPIKeyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

// adminListAllAPIKeys GET /server-admin/api-keys
func adminListAllAPIKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := globalStore.ListAPIKeys(ctx, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiKeyAdminView, 0, len(list))
	for _, k := range list {
		out = append(out, toAPIKeyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

// adminRevokeAPIKey DELETE /server-admin/api-keys/{keyID}
func adminRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("keyID")
	id, err := parseIntStrict(raw)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "密钥 ID 不合法")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := globalStore.RevokeAPIKey(ctx, id, 0); err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "密钥不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	logError("🗑 已吊销开放接口 ApiKey id=%d", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
