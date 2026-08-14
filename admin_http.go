package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// adminDeviceResponse 给管理端返回的设备信息（含在线状态）。
type adminDeviceResponse struct {
	UserID     int64  `json:"user_id"`
	DeviceID   string `json:"device_id"`
	Role       string `json:"role"`
	Platform   string `json:"platform"`
	Disabled   bool   `json:"disabled"`
	Online     bool   `json:"online"`
	LastSeenAt string `json:"last_seen_at"`
	CreatedAt  string `json:"created_at"`
}

// requireAdminToken 校验 Authorization: Bearer <token>。
// 配置里没填 admin_token 时返回 503（HTTP 兜底通道未启用），
// 配了但 token 不对返回 401。正常情况下管理端指令走 Redis Pub/Sub，
// 这里的 HTTP 接口只是兜底，不配 admin_token 不影响主流程。
func requireAdminToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := ""
		if globalConfig != nil {
			want = globalConfig.Server.AdminToken
		}
		if want == "" {
			http.Error(w, "服务端未配置 admin_token，管理接口不可用", http.StatusServiceUnavailable)
			return
		}
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, "Bearer ") || got[len("Bearer "):] != want {
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// writeJSONAdmin 与 auth_http.go 中统一 JSON 响应同名，这里直接复用。
// 为了避免重复声明，本文件不再实现 writeJSON。

// adminListDevices GET /admin/users/{id}/devices
func adminListDevices(w http.ResponseWriter, r *http.Request) {
	userID, ok := int64PathParam(w, r, "id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := authService.ListDevices(ctx, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	online, _ := authService.OnlineDevices(ctx, userID)
	out := make([]adminDeviceResponse, 0, len(list))
	for _, d := range list {
		_, isOnline := online[d.DeviceID]
		out = append(out, adminDeviceResponse{
			UserID:     d.UserID,
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			Disabled:   d.Disabled,
			Online:     isOnline,
			LastSeenAt: d.LastSeenAt.Format(time.RFC3339),
			CreatedAt:  d.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// adminSetDeviceStatus PUT /admin/users/{id}/devices/{deviceID}/status
// body: {"disabled": true|false}
func adminSetDeviceStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := int64PathParam(w, r, "id")
	if !ok {
		return
	}
	deviceID := r.PathValue("deviceID")
	if deviceID == "" {
		http.Error(w, "缺少 deviceID", http.StatusBadRequest)
		return
	}
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不合法", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := authService.SetDeviceStatus(ctx, userID, deviceID, body.Disabled); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			http.Error(w, "设备不存在", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 禁用：立刻踢这台设备下线；解禁：不动当前连接（让它继续工作）
	if body.Disabled {
		hub.kickDevice(userID, deviceID, KickReasonDeviceBanned)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminKick POST /admin/kick
// 兼容两种 body：
//  1. {"user_id":123}                   踢用户所有设备
//  2. {"user_id":123,"device_id":"xx"}  只踢该设备
func adminKick(w http.ResponseWriter, r *http.Request) {
	var cmd AdminCommand
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil || cmd.UserID <= 0 {
		http.Error(w, "请求体不合法：需要 user_id", http.StatusBadRequest)
		return
	}
	if cmd.Reason == "" {
		cmd.Reason = KickReasonDeviceKicked
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	switch {
	case cmd.DeviceID != "":
		hub.kickDevice(cmd.UserID, cmd.DeviceID, cmd.Reason)
	default:
		invalidateUserSessions(ctx, cmd.UserID)
		hub.kickUser(cmd.UserID, cmd.Reason)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// invalidateUserSessions 在踢整个用户下线时清掉服务端会话/缓存，
// 避免客户端拿旧 token 立刻又连上（账号被封/密码被改时必须）。
func invalidateUserSessions(ctx context.Context, userID int64) {
	if authService != nil {
		authService.InvalidateSessions(ctx, userID)
	}
}

// int64PathParam 从路径里取一个 int64 参数，失败写 400 并返回 false。
func int64PathParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		http.Error(w, "参数 "+name+" 不合法", http.StatusBadRequest)
		return 0, false
	}
	return v, true
}
