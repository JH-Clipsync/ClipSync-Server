package main

import (
	"context"
	"crypto/subtle"
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
	Username   string `json:"username,omitempty"`
	DeviceID   string `json:"device_id"`
	Role       string `json:"role"`
	Platform   string `json:"platform"`
	Name       string `json:"name"`
	LastIP     string `json:"last_ip"`
	Disabled   bool   `json:"disabled"`
	Online     bool   `json:"online"`
	LastSeenAt string `json:"last_seen_at"`
	CreatedAt  string `json:"created_at"`
}

func requireAdminToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if globalConfig.Server.AdminToken == "" {
			http.Error(w, "服务端未配置 admin_token，管理接口不可用", http.StatusServiceUnavailable)
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(tok), []byte(globalConfig.Server.AdminToken)) != 1 {
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ==================== 管理端 HTTP 接口（供 ClipSync-Admin HTTP 兜底调用） ====================

type adminCreateUserReq struct {
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Password string `json:"password"`
}

// adminCreateUser POST /server-admin/users
// 管理端创建用户：不走 auth.allow_register 开关，密码用 Server 端 bcrypt 哈希。
func adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body adminCreateUserReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不合法", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := authService.AdminCreateUser(ctx, body.Username, body.Nickname, body.Password)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			http.Error(w, "用户名已存在", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"nickname": user.Nickname,
	})
}

// writeJSONAdmin 与 auth_http.go 中统一 JSON 响应同名，这里直接复用。
// 为了避免重复声明，本文件不再实现 writeJSON。

// adminListDevices GET /server-admin/users/{id}/devices
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
	// 在线状态以内存 hub 为准（真实 WebSocket 连接），Redis 仅作补充
	memOnline := hub.onlineDeviceIDs(userID)
	redisOnline, _ := authService.OnlineDevices(ctx, userID)
	out := make([]adminDeviceResponse, 0, len(list))
	for _, d := range list {
		_, isOnline := memOnline[d.DeviceID]
		if !isOnline && redisOnline != nil {
			if _, ok := redisOnline[d.DeviceID]; ok {
				isOnline = true
			}
		}
		out = append(out, adminDeviceResponse{
			UserID:     d.UserID,
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			Name:       d.Name,
			LastIP:     d.LastIP,
			Disabled:   d.Disabled,
			Online:     isOnline,
			LastSeenAt: d.LastSeenAt.Format(time.RFC3339),
			CreatedAt:  d.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// adminListAllDevices GET /server-admin/devices
// 跨用户分页查询设备，支持按 username/device_id/name/last_ip 模糊搜索、按禁用状态过滤。
// 查询参数：keyword、disabled(true/false)、page、page_size
func adminListAllDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	f := DeviceFilter{
		Keyword: q.Get("keyword"),
		Offset:  (page - 1) * pageSize,
		Limit:   pageSize,
	}
	if uid, _ := strconv.ParseInt(q.Get("user_id"), 10, 64); uid > 0 {
		f.UserID = uid
	}
	switch strings.ToLower(q.Get("disabled")) {
	case "true":
		t := true
		f.Disabled = &t
	case "false":
		f2 := false
		f.Disabled = &f2
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, total, err := authService.ListAllDevices(ctx, f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	onlineSet := hub.allOnlineDeviceIDs()
	out := make([]adminDeviceResponse, 0, len(rows))
	for _, d := range rows {
		_, online := onlineSet[strconv.FormatInt(d.UserID, 10)+":"+d.DeviceID]
		out = append(out, adminDeviceResponse{
			UserID:     d.UserID,
			Username:   d.Username,
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			Name:       d.Name,
			LastIP:     d.LastIP,
			Disabled:   d.Disabled,
			Online:     online,
			LastSeenAt: d.LastSeenAt.Format(time.RFC3339),
			CreatedAt:  d.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"list":  out,
		"total": total,
		"page":  page,
	})
}

// adminRenameDevice PUT /server-admin/users/{id}/devices/{deviceID}/name
// body: {"name": "新设备名"}
func adminRenameDevice(w http.ResponseWriter, r *http.Request) {
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
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不合法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		http.Error(w, "设备名称不能为空", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := authService.UpdateDeviceName(ctx, userID, deviceID, body.Name); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			http.Error(w, "设备不存在", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	hub.renameDevice(userID, deviceID, body.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminSetDeviceStatus PUT /server-admin/users/{id}/devices/{deviceID}/status
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
	// 禁用成功后主动踢这台设备下线；解禁不动当前连接。
	// 即使踢连接失败，设备下次重连握手时也会因 disabled=true 被拒。
	if body.Disabled {
		hub.kickDevice(userID, deviceID, KickReasonDeviceBanned)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminKick POST /server-admin/kick
// 统一入口，支持 Admin 通过 HTTP 兜底下发的所有管理动作：
//   - kick_user / 空 action：踢用户所有设备下线
//   - kick_device：踢单台设备下线
//   - disable_device：禁用设备（先改库，成功后踢下线）
//   - enable_device：解禁设备（只改库，不踢连接）
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

	switch cmd.Action {
	case AdminActionDisableDevice:
		if cmd.DeviceID == "" {
			http.Error(w, "缺少 device_id", http.StatusBadRequest)
			return
		}
		if err := authService.SetDeviceStatus(ctx, cmd.UserID, cmd.DeviceID, true); err != nil {
			if errors.Is(err, ErrDeviceNotFound) {
				http.Error(w, "设备不存在", http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		hub.kickDevice(cmd.UserID, cmd.DeviceID, KickReasonDeviceBanned)
	case AdminActionEnableDevice:
		if cmd.DeviceID == "" {
			http.Error(w, "缺少 device_id", http.StatusBadRequest)
			return
		}
		if err := authService.SetDeviceStatus(ctx, cmd.UserID, cmd.DeviceID, false); err != nil {
			if errors.Is(err, ErrDeviceNotFound) {
				http.Error(w, "设备不存在", http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	case AdminActionKickDevice:
		if cmd.DeviceID == "" {
			http.Error(w, "缺少 device_id", http.StatusBadRequest)
			return
		}
		hub.kickDevice(cmd.UserID, cmd.DeviceID, cmd.Reason)
	default:
		// 兼容旧版 body（不带 action）或 kick_user：踢整个账号
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
