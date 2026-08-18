package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ===== 认证业务 =====

var (
	// ErrInvalidCredentials 用户名或密码错误（不区分，避免探测用户名是否存在）
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	// ErrUserDisabled 账号被停用
	ErrUserDisabled = errors.New("账号已停用")
	// ErrWeakPassword 密码不满足强度要求
	ErrWeakPassword = errors.New("密码长度至少 8 位")
	// ErrBadUsername 用户名不合法
	ErrBadUsername = errors.New("用户名需 3-32 位，仅允许字母/数字/下划线/连字符")
	// ErrRegisterClosed 注册通道已关闭
	ErrRegisterClosed = errors.New("服务端未开放注册")
)

// AuthService 把 MySQL（持久）和 Redis（缓存 + 在线态）拼成一套认证逻辑。
type AuthService struct {
	db    *MySQLStore
	cache *RedisStore
	cfg   AuthConfig
}

func NewAuthService(db *MySQLStore, cache *RedisStore, cfg AuthConfig) *AuthService {
	return &AuthService{db: db, cache: cache, cfg: cfg}
}

// AuthGate 是 WebSocket / push 这两条数据通道真正依赖的能力：
// 校验 token + 维护在线登记。抽成接口是为了让 handler 单测能注入替身，
// 不必真的拉起 MySQL 和 Redis。*AuthService 是生产实现。
type AuthGate interface {
	AuthenticateToken(ctx context.Context, token string) (int64, string, error)
	MarkOnline(ctx context.Context, userID int64, deviceID, role string, ttl time.Duration) error
	TouchOnline(ctx context.Context, userID int64, ttl time.Duration) error
	MarkOffline(ctx context.Context, userID int64, deviceID string) error
	OnlineDevices(ctx context.Context, userID int64) (map[string]string, error)
}

// 以下四个方法把在线登记透传给 Redis，让 *AuthService 满足 AuthGate。

func (a *AuthService) MarkOnline(ctx context.Context, userID int64, deviceID, role string, ttl time.Duration) error {
	return a.cache.MarkOnline(ctx, userID, deviceID, role, ttl)
}

func (a *AuthService) TouchOnline(ctx context.Context, userID int64, ttl time.Duration) error {
	return a.cache.TouchOnline(ctx, userID, ttl)
}

func (a *AuthService) MarkOffline(ctx context.Context, userID int64, deviceID string) error {
	return a.cache.MarkOffline(ctx, userID, deviceID)
}

func (a *AuthService) OnlineDevices(ctx context.Context, userID int64) (map[string]string, error) {
	return a.cache.OnlineDevices(ctx, userID)
}

// EnsureDeviceAllowed 在 WS 握手阶段调用：首次见到的设备自动建档，
// 已被管理员禁用的设备直接返回 ErrDeviceDisabled，拒绝升级连接。
//
// role/platform 由客户端 query 上报，旧客户端没带 platform 时由 main 里
// 的 platformFromDeviceID 兜底推断后再传进来。
func (a *AuthService) EnsureDeviceAllowed(ctx context.Context, userID int64, deviceID, role, platform string) error {
	if err := a.db.UpsertDevice(ctx, &Device{
		UserID:   userID,
		DeviceID: deviceID,
		Role:     role,
		Platform: platform,
	}); err != nil {
		// 建档失败不阻断连接：设备表只是管理能力，不能因此影响正常同步
		logWarn("⚠ 设备建档失败 user=%d device=%s: %v", userID, shortID(deviceID), err)
	}
	disabled, err := a.db.IsDeviceDisabled(ctx, userID, deviceID)
	if err != nil {
		logWarn("⚠ 查询设备禁用状态失败 user=%d device=%s: %v", userID, shortID(deviceID), err)
		return nil
	}
	if disabled {
		return ErrDeviceDisabled
	}
	return nil
}

// ListDevices 列出用户所有登记过的设备（含离线）。供管理端接口使用。
func (a *AuthService) ListDevices(ctx context.Context, userID int64) ([]*Device, error) {
	return a.db.ListDevices(ctx, userID)
}

// SetDeviceStatus 启用/禁用指定账号下的一台设备。禁用一台设备只影响后续登录/握手，
// 已经在线的连接由上层另外调用 Hub.kickDevice 立即踢掉。
func (a *AuthService) SetDeviceStatus(ctx context.Context, userID int64, deviceID string, disabled bool) error {
	return a.db.UpdateDeviceStatus(ctx, userID, deviceID, disabled)
}

// GetDeviceName 查询某设备的自定义名称，不存在返回空串。
func (a *AuthService) GetDeviceName(ctx context.Context, userID int64, deviceID string) (string, error) {
	return a.db.GetDeviceName(ctx, userID, deviceID)
}

// UpdateDeviceName 更新某账号下一台设备的自定义名称。
func (a *AuthService) UpdateDeviceName(ctx context.Context, userID int64, deviceID, name string) error {
	return a.db.UpdateDeviceName(ctx, userID, deviceID, name)
}

// InvalidateSessions 清掉一个用户的 MySQL 会话与 Redis token 缓存，
// 让该用户所有老 token 立即失效。管理端踢整个账号下线时调用。
func (a *AuthService) InvalidateSessions(ctx context.Context, userID int64) {
	if th, err := a.cache.rdb.Get(ctx, a.cache.userTokenKey(userID)).Result(); err == nil && th != "" {
		_ = a.cache.DropToken(ctx, th, userID)
	}
	_ = a.cache.DropPlainToken(ctx, userID)
	_ = a.db.DeleteSession(ctx, userID)
}

// LoginResult 登录接口的返回信息。
type LoginResult struct {
	Token     string
	UserID    int64
	Username  string
	ExpiresAt time.Time
	// Reused 为 true 表示"该用户已有客户端登录"，本次直接复用现有 token；
	// false 表示当前没有客户端在线，重新签发了一个。
	Reused bool
	// OnlineDevices 复用场景下的在线设备数，方便客户端提示"已有 N 台设备在线"
	OnlineDevices int64
}

// ValidateUsername 用户名规则：3-32 位，字母/数字/下划线/连字符。
func ValidateUsername(name string) error {
	if len(name) < 3 || len(name) > 32 {
		return ErrBadUsername
	}
	for _, r := range name {
		if r > unicode.MaxASCII {
			return ErrBadUsername
		}
		isOK := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !isOK {
			return ErrBadUsername
		}
	}
	return nil
}

// Register 注册新用户。是否开放由 auth.allow_register 控制。
func (a *AuthService) Register(ctx context.Context, username, password string) (*User, error) {
	if !a.cfg.AllowRegister {
		return nil, ErrRegisterClosed
	}
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if len(password) < a.cfg.MinPasswordLen {
		return nil, ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	return a.db.CreateUser(ctx, username, hash)
}

// Login 用用户名 + 密码换 token，这是"手填 token"被替换掉的入口。
//
// 核心规则（对应需求）：
//   - 当前登录用户**没有客户端在线** → 新生成 token，覆盖旧会话
//   - 当前登录用户**已有客户端在线** → 直接返回现有 token，让新客户端加入同一组
//
// 在线判定走 Redis（HLen > 0）；token 本身存在 MySQL（哈希）+ Redis 缓存。
func (a *AuthService) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	user, err := a.db.FindUserByName(ctx, username)
	if errors.Is(err, ErrUserNotFound) {
		// 故意跟密码错误返回同一个错误，避免用户名枚举
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if user.Disabled {
		return nil, ErrUserDisabled
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}

	ttl := time.Duration(a.cfg.TokenTTLHours) * time.Hour

	online, err := a.cache.OnlineCount(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	// 已有客户端在线 → 复用现有会话 token
	if online > 0 {
		if sess, err := a.db.GetSession(ctx, user.ID); err != nil {
			return nil, err
		} else if sess != nil {
			// 只有明文 token 才能发给客户端，而库里只有哈希。
			// 因此明文 token 在签发时写进 Redis（key 带 TTL），这里取回来。
			if plain, ok, err := a.cache.LoadPlainToken(ctx, user.ID); err != nil {
				return nil, err
			} else if ok && TokenHash(plain) == sess.TokenHash {
				return &LoginResult{
					Token:         plain,
					UserID:        user.ID,
					Username:      user.Username,
					ExpiresAt:     sess.ExpiresAt,
					Reused:        true,
					OnlineDevices: online,
				}, nil
			}
		}
		// 拿不到明文（缓存被清/过期）：只能重新签发，老客户端会在下次握手时被拒并自动重登
		logWarn("⚠ 用户 %s 有 %d 台在线设备但明文 token 已失效，重新签发", user.Username, online)
	}

	// 无客户端在线（或明文丢失）→ 新签发
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		UserID:    user.ID,
		TokenHash: TokenHash(token),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := a.db.UpsertSession(ctx, sess); err != nil {
		return nil, err
	}
	if err := a.cache.CacheToken(ctx, sess.TokenHash, user.ID, ttl); err != nil {
		return nil, err
	}
	if err := a.cache.StorePlainToken(ctx, user.ID, token, ttl); err != nil {
		return nil, err
	}
	return &LoginResult{
		Token:         token,
		UserID:        user.ID,
		Username:      user.Username,
		ExpiresAt:     sess.ExpiresAt,
		Reused:        false,
		OnlineDevices: online,
	}, nil
}

// AuthenticateToken 校验 token 是否有效，返回所属用户 ID。
// 先查 Redis，未命中回源 MySQL 并回填缓存。
func (a *AuthService) AuthenticateToken(ctx context.Context, token string) (int64, string, error) {
	if token == "" {
		return 0, "", ErrInvalidCredentials
	}
	hash := TokenHash(token)

	if userID, ok, err := a.cache.LookupToken(ctx, hash); err != nil {
		logWarn("⚠ Redis token 查询失败，回源 MySQL: %v", err)
	} else if ok {
		return userID, "", nil
	}

	user, err := a.db.FindUserByTokenHash(ctx, hash)
	if errors.Is(err, ErrUserNotFound) {
		return 0, "", ErrInvalidCredentials
	}
	if err != nil {
		return 0, "", err
	}
	if user.Disabled {
		return 0, "", ErrUserDisabled
	}
	// 回填缓存：TTL 用会话剩余时间
	if sess, err := a.db.GetSession(ctx, user.ID); err == nil && sess != nil {
		if ttl := time.Until(sess.ExpiresAt); ttl > 0 {
			_ = a.cache.CacheToken(ctx, hash, user.ID, ttl)
		}
	}
	return user.ID, user.Username, nil
}

// Logout 主动登出：删会话 + 清缓存。之后所有用该 token 的连接都会握手失败。
func (a *AuthService) Logout(ctx context.Context, token string) error {
	hash := TokenHash(token)
	userID, ok, err := a.cache.LookupToken(ctx, hash)
	if err != nil || !ok {
		user, err2 := a.db.FindUserByTokenHash(ctx, hash)
		if err2 != nil {
			return fmt.Errorf("token 无效: %w", err2)
		}
		userID = user.ID
	}
	if err := a.db.DeleteSession(ctx, userID); err != nil {
		return err
	}
	if err := a.cache.DropToken(ctx, hash, userID); err != nil {
		return err
	}
	return a.cache.DropPlainToken(ctx, userID)
}

// ChangePassword 修改密码：验证旧密码 → 更新数据库 → 作废所有旧 token → 发 kick 踢掉在线连接。
// 返回新签发的 token，客户端拿到后替换本地缓存的旧 token。
func (a *AuthService) ChangePassword(ctx context.Context, token, oldPassword, newPassword string) (string, error) {
	if len(newPassword) < a.cfg.MinPasswordLen {
		return "", ErrWeakPassword
	}
	userID, _, err := a.AuthenticateToken(ctx, token)
	if err != nil {
		return "", ErrInvalidCredentials
	}
	user, err := a.db.FindUserByID(ctx, userID)
	if err != nil {
		return "", err
	}
	if !VerifyPassword(user.PasswordHash, oldPassword) {
		return "", ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return "", err
	}
	if err := a.db.UpdatePassword(ctx, userID, hash); err != nil {
		return "", err
	}
	// 作废旧会话 + 缓存
	oldHash := TokenHash(token)
	_ = a.db.DeleteSession(ctx, userID)
	_ = a.cache.DropToken(ctx, oldHash, userID)
	_ = a.cache.DropPlainToken(ctx, userID)
	// 踢掉所有在线连接，让它们立即断开
	_ = a.cache.PublishKickUser(ctx, userID, KickReasonPasswordReset)

	// 签发新 token 返回给调用方
	ttl := time.Duration(a.cfg.TokenTTLHours) * time.Hour
	newToken, err := NewToken()
	if err != nil {
		return "", err
	}
	sess := &Session{
		UserID:    userID,
		TokenHash: TokenHash(newToken),
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	if err := a.db.UpsertSession(ctx, sess); err != nil {
		return "", err
	}
	if err := a.cache.CacheToken(ctx, sess.TokenHash, userID, ttl); err != nil {
		return "", err
	}
	if err := a.cache.StorePlainToken(ctx, userID, newToken, ttl); err != nil {
		return "", err
	}
	return newToken, nil
}

// bootstrapUser 按配置创建初始账号：已存在则跳过，不改动已有密码。
// 目的是让 docker compose 起来就能直接登录，不用先手动注册。
func bootstrapUser(db *MySQLStore, cfg AuthConfig) {
	if cfg.BootstrapUser == "" || cfg.BootstrapPassword == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := db.FindUserByName(ctx, cfg.BootstrapUser); err == nil {
		logInfo("👤 初始账号 %s 已存在，跳过创建", cfg.BootstrapUser)
		return
	} else if !errors.Is(err, ErrUserNotFound) {
		logWarn("⚠ 检查初始账号失败: %v", err)
		return
	}

	if err := ValidateUsername(cfg.BootstrapUser); err != nil {
		logWarn("⚠ 初始账号用户名不合法: %v", err)
		return
	}
	hash, err := HashPassword(cfg.BootstrapPassword)
	if err != nil {
		logWarn("⚠ 初始账号密码哈希失败: %v", err)
		return
	}
	if _, err := db.CreateUser(ctx, cfg.BootstrapUser, hash); err != nil {
		logWarn("⚠ 创建初始账号失败: %v", err)
		return
	}
	logError("👤 已创建初始账号: %s（请尽快改密码）", cfg.BootstrapUser)
}

// purgeSessionsLoop 定期清理过期会话行。Redis 侧靠 TTL 自动过期，这里只管 MySQL。
func purgeSessionsLoop(db *MySQLStore) {
	purge := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if n, err := db.PurgeExpiredSessions(ctx); err != nil {
			logWarn("⚠ 清理过期会话失败: %v", err)
		} else if n > 0 {
			logInfo("🧹 已清理 %d 条过期会话", n)
		}
	}
	purge()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		purge()
	}
}
