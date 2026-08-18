package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// renameDeviceRequest 改名请求体。
type renameDeviceRequest struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

// requireUserToken 校验登录 token（Authorization: Bearer <token>），
// 通过后把 userID 写入请求上下文交给后续 handler。
func requireUserToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "未授权：缺少 token", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		userID, _, err := authGate.AuthenticateToken(ctx, token)
		cancel()
		if err != nil {
			http.Error(w, "认证失败：token 无效或已过期", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey{}, userID)))
	}
}

type userIDKey struct{}

func userIDFromCtx(r *http.Request) int64 {
	v, _ := r.Context().Value(userIDKey{}).(int64)
	return v
}

// handleRenameDevice POST /device/name
// 给当前账号下的一台设备设置自定义名称，并实时广播给所有在线端。
func handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	var req renameDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体格式错误", http.StatusBadRequest)
		return
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.Name = strings.TrimSpace(req.Name)
	if req.DeviceID == "" {
		http.Error(w, "缺少 device_id", http.StatusBadRequest)
		return
	}
	if len([]rune(req.Name)) > 32 {
		http.Error(w, "设备名称不能超过 32 个字符", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := authService.UpdateDeviceName(ctx, userID, req.DeviceID, req.Name)
	cancel()
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			http.Error(w, "设备不存在", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 更新内存中在线连接的名称并广播，让所有端实时刷新
	hub.renameDevice(userID, req.DeviceID, req.Name)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device_id": req.DeviceID, "name": req.Name})
}
