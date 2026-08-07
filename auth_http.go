package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===== 登录限流 =====
//
// 极简滑动窗口：按 IP 记录最近一分钟的尝试次数。
// 单机内存足够——这是防暴力破解的第一道门，不是分布式配额系统。
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
}

func newLoginLimiter(perMin int) *loginLimiter {
	return &loginLimiter{attempts: make(map[string][]time.Time), limit: perMin}
}

// allow 返回 false 表示该 IP 已超限。
func (l *loginLimiter) allow(ip string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-time.Minute)
	kept := l.attempts[ip][:0]
	for _, t := range l.attempts[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.attempts[ip] = kept
		return false
	}
	l.attempts[ip] = append(kept, time.Now())
	return true
}

var limiter *loginLimiter

// authService 全局认证服务（登录 / 注册 / 登出走它），main 启动时初始化。
var authService *AuthService

// authGate 是 /ws 和 /push 用的鉴权入口。生产环境指向 authService，
// 单测可以替换成不依赖 MySQL/Redis 的实现。
var authGate AuthGate

// writeJSON 统一 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 统一错误响应体：{"ok":false,"error":"..."}
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// decodeCredentials 解析并粗校验请求体
func decodeCredentials(r *http.Request) (*credentials, error) {
	var c credentials
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&c); err != nil {
		return nil, errors.New("请求体不是合法 JSON")
	}
	c.Username = strings.TrimSpace(c.Username)
	if c.Username == "" || c.Password == "" {
		return nil, errors.New("username 和 password 不能为空")
	}
	return &c, nil
}

// loginHandler POST /auth/login
//
// 请求：{"username":"alice","password":"..."}
// 响应：{"ok":true,"token":"...","user_id":1,"username":"alice",
//
//	"expires_at":"...","reused":true,"online_devices":1}
//
// reused=true 表示该用户已有客户端在线，返回的是现有 token；
// reused=false 表示此前没有客户端登录，服务端新签发了 token。
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	ip := extractIP(r)
	if !limiter.allow(ip) {
		logWarn("⚠ 登录限流：ip=%s 超过每分钟上限", ip)
		writeErr(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试")
		return
	}
	c, err := decodeCredentials(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	res, err := authService.Login(ctx, c.Username, c.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			logWarn("⚠ 登录失败：user=%s ip=%s 凭据错误", c.Username, ip)
			writeErr(w, http.StatusUnauthorized, err.Error())
		case errors.Is(err, ErrUserDisabled):
			writeErr(w, http.StatusForbidden, err.Error())
		default:
			logError("✗ 登录异常: %v", err)
			writeErr(w, http.StatusInternalServerError, "服务端异常")
		}
		return
	}

	action := "新签发 token"
	if res.Reused {
		action = "复用在线 token"
	}
	logInfo("🔑 登录成功: user=%s(%d) ip=%s %s（在线 %d 台）",
		res.Username, res.UserID, ip, action, res.OnlineDevices)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"token":          res.Token,
		"user_id":        res.UserID,
		"username":       res.Username,
		"expires_at":     res.ExpiresAt.Format(time.RFC3339),
		"reused":         res.Reused,
		"online_devices": res.OnlineDevices,
		"e2ee_required":  globalConfig != nil && globalConfig.E2EE.Require,
	})
}

// registerHandler POST /auth/register
func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	c, err := decodeCredentials(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	user, err := authService.Register(ctx, c.Username, c.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrRegisterClosed):
			writeErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrUserExists):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrWeakPassword), errors.Is(err, ErrBadUsername):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			logError("✗ 注册异常: %v", err)
			writeErr(w, http.StatusInternalServerError, "服务端异常")
		}
		return
	}
	logInfo("🆕 注册成功: user=%s(%d) ip=%s", user.Username, user.ID, extractIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"user_id":  user.ID,
		"username": user.Username,
	})
}

// bearerToken 从 Authorization: Bearer xxx 或 ?token= 里取 token
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return r.URL.Query().Get("token")
}

// sessionHandler GET /auth/session
// 用 token 查询当前会话状态与在线设备，客户端启动时用它判断本地 token 是否还有效。
func sessionHandler(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "缺少 token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	userID, username, err := authService.AuthenticateToken(ctx, token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "token 无效或已过期")
		return
	}
	devices, err := authService.OnlineDevices(ctx, userID)
	if err != nil {
		logWarn("⚠ 查询在线设备失败: %v", err)
		devices = map[string]string{}
	}
	resp := map[string]any{
		"ok":             true,
		"user_id":        userID,
		"online_devices": devices,
		"e2ee_required":  globalConfig != nil && globalConfig.E2EE.Require,
	}
	if username != "" {
		resp["username"] = username
	}
	writeJSON(w, http.StatusOK, resp)
}

// logoutHandler POST /auth/logout
// 作废当前 token；所有仍在用它的连接会在下次握手时被拒。
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	token := bearerToken(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "缺少 token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := authService.Logout(ctx, token); err != nil {
		writeErr(w, http.StatusUnauthorized, "token 无效")
		return
	}
	logInfo("👋 已登出，token 作废 (%s) ip=%s", shortToken(token), extractIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// changePasswordHandler POST /auth/change-password
// 请求：{"old_password":"...","new_password":"..."}  需 Bearer token 鉴权
// 改完后旧 token 立即失效，所有在线连接被踢；返回新 token 供客户端替换。
func changePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	token := bearerToken(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "缺少 token")
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.OldPassword == "" || body.NewPassword == "" {
		writeErr(w, http.StatusBadRequest, "old_password 和 new_password 不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	newToken, err := authService.ChangePassword(ctx, token, body.OldPassword, body.NewPassword)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			writeErr(w, http.StatusUnauthorized, "旧密码不正确或 token 已失效")
		case errors.Is(err, ErrWeakPassword):
			writeErr(w, http.StatusBadRequest, "新密码太短")
		default:
			logError("✗ 修改密码异常: %v", err)
			writeErr(w, http.StatusInternalServerError, "服务端异常")
		}
		return
	}
	logInfo("🔑 密码已修改，旧 token 已作废 ip=%s", extractIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": newToken})
}
