package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Device 一条设备记录。同一账号下 device_id 唯一；首次出现时由登录/握手写入。
type Device struct {
	UserID     int64
	DeviceID   string
	Role       string
	Platform   string
	Name       string
	Disabled   bool
	LastSeenAt time.Time
	CreatedAt time.Time
}

// ErrDeviceDisabled 设备被管理员禁用，拒绝登录或继续使用。
var ErrDeviceDisabled = errors.New("设备已被禁用")

// ErrDeviceNotFound 设备不存在
var ErrDeviceNotFound = errors.New("设备不存在")

// migrateDevices 建 devices 表。跟 deploy/mysql/init.sql 保持一致。
func (s *MySQLStore) migrateDevices(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS devices (
			user_id      BIGINT       NOT NULL,
			device_id    VARCHAR(128) NOT NULL,
			role         VARCHAR(16)  NOT NULL DEFAULT '',
			platform     VARCHAR(32)  NOT NULL DEFAULT '',
			name         VARCHAR(64)  NOT NULL DEFAULT '',
			disabled     TINYINT(1)   NOT NULL DEFAULT 0,
			last_seen_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, device_id),
			KEY idx_devices_user (user_id),
			CONSTRAINT fk_devices_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 兼容旧库：name 列是后续新增的，旧实例升级时补上（已存在则忽略错误）
		`ALTER TABLE devices ADD COLUMN name VARCHAR(64) NOT NULL DEFAULT '' AFTER platform`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// 旧库已存在该列时会报 "Duplicate column name"，无害跳过
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("初始化 devices 表失败: %w", err)
		}
	}
	return nil
}

// UpsertDevice 登录/握手时登记一台设备。role/platform 以最新一次为准。
// disabled 保持库里的现值，不会因为再次上线而自动解禁。
func (s *MySQLStore) UpsertDevice(ctx context.Context, d *Device) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (user_id, device_id, role, platform, disabled, last_seen_at, created_at)
		VALUES (?, ?, ?, ?, 0, NOW(), NOW())
		ON DUPLICATE KEY UPDATE
			role = VALUES(role),
			platform = VALUES(platform),
			last_seen_at = NOW()`,
		d.UserID, d.DeviceID, d.Role, d.Platform)
	if err != nil {
		return fmt.Errorf("登记设备失败: %w", err)
	}
	return nil
}

// IsDeviceDisabled 查询设备是否被禁用。不存在视为未禁用（首次登录会自动建档）。
func (s *MySQLStore) IsDeviceDisabled(ctx context.Context, userID int64, deviceID string) (bool, error) {
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT disabled FROM devices WHERE user_id = ? AND device_id = ?`,
		userID, deviceID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询设备状态失败: %w", err)
	}
	return disabled != 0, nil
}

// ListDevices 列出某账号下所有登记过的设备（含离线）。
func (s *MySQLStore) ListDevices(ctx context.Context, userID int64) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, device_id, role, platform, name, disabled, last_seen_at, created_at
		  FROM devices WHERE user_id = ?
		 ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("查询设备列表失败: %w", err)
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var d Device
		var disabled int
		if err := rows.Scan(&d.UserID, &d.DeviceID, &d.Role, &d.Platform,
			&d.Name, &disabled, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取设备行失败: %w", err)
		}
		d.Disabled = disabled != 0
		out = append(out, &d)
	}
	return out, rows.Err()
}

// GetDeviceName 查询某设备的自定义名称，不存在返回空串。
func (s *MySQLStore) GetDeviceName(ctx context.Context, userID int64, deviceID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM devices WHERE user_id = ? AND device_id = ?`,
		userID, deviceID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("查询设备名称失败: %w", err)
	}
	return name, nil
}

// UpdateDeviceName 更新某账号下一台设备的自定义名称。
func (s *MySQLStore) UpdateDeviceName(ctx context.Context, userID int64, deviceID, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE user_id = ? AND device_id = ?`,
		name, userID, deviceID)
	if err != nil {
		return fmt.Errorf("更新设备名称失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// UpdateDeviceStatus 启用/禁用指定账号下的一台设备。
func (s *MySQLStore) UpdateDeviceStatus(ctx context.Context, userID int64, deviceID string, disabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET disabled = ? WHERE user_id = ? AND device_id = ?`,
		disabled, userID, deviceID)
	if err != nil {
		return fmt.Errorf("更新设备状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}
