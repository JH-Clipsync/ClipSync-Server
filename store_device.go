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
	LastIP     string
	Disabled   bool
	LastSeenAt time.Time
	CreatedAt  time.Time
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
			last_ip      VARCHAR(64)  NOT NULL DEFAULT '',
			disabled     TINYINT(1)   NOT NULL DEFAULT 0,
			last_seen_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, device_id),
			KEY idx_devices_user (user_id),
			KEY idx_devices_last_seen (last_seen_at),
			CONSTRAINT fk_devices_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 兼容旧库：name / last_ip 列是后续新增的，旧实例升级时补上（已存在则忽略错误）
		`ALTER TABLE devices ADD COLUMN name VARCHAR(64) NOT NULL DEFAULT '' AFTER platform`,
		`ALTER TABLE devices ADD COLUMN last_ip VARCHAR(64) NOT NULL DEFAULT '' AFTER name`,
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

// UpsertDevice 登录/握手时登记一台设备。
//
// 字段更新策略：
//   - role/platform/last_ip/last_seen_at：以最新一次握手为准；
//   - name：仅首次建档（INSERT）时写入客户端上报值；已存在的记录不会被握手阶段
//     上报的 name 覆盖，避免把管理员/用户通过重命名接口设置的名称冲掉。
//     改名只能走显式的 UpdateDeviceName 接口；
//   - disabled：保持库里的现值，不会因为再次上线而自动解禁。
func (s *MySQLStore) UpsertDevice(ctx context.Context, d *Device) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (user_id, device_id, role, platform, name, last_ip, disabled, last_seen_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, NOW(), NOW())
		ON DUPLICATE KEY UPDATE
			role = VALUES(role),
			platform = VALUES(platform),
			last_ip = VALUES(last_ip),
			last_seen_at = NOW()`,
		d.UserID, d.DeviceID, d.Role, d.Platform, d.Name, d.LastIP)
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
		SELECT user_id, device_id, role, platform, name, last_ip, disabled, last_seen_at, created_at
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
			&d.Name, &d.LastIP, &disabled, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取设备行失败: %w", err)
		}
		d.Disabled = disabled != 0
		out = append(out, &d)
	}
	return out, rows.Err()
}

// DeviceFilter 设备列表搜索条件。零值表示不限制。
type DeviceFilter struct {
	Keyword  string // 模糊匹配 username / device_id / name / last_ip
	UserID   int64  // > 0 时只查该用户
	Disabled *bool  // 禁用状态过滤
	Offset   int
	Limit    int
}

// DeviceRow 全量设备列表的一行：带上 username，方便管理端直接展示。
type DeviceRow struct {
	Device
	Username string
}

// ListAllDevices 跨用户分页查询设备，按 last_seen_at 倒序。
// keyword 会同时模糊匹配 users.username / devices.device_id / devices.name / devices.last_ip。
func (s *MySQLStore) ListAllDevices(ctx context.Context, f DeviceFilter) ([]*DeviceRow, int64, error) {
	var args []any
	where := "WHERE 1=1"
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + kw + "%"
		where += " AND (u.username LIKE ? OR d.device_id LIKE ? OR d.name LIKE ? OR d.last_ip LIKE ?)"
		args = append(args, like, like, like, like)
	}
	if f.Disabled != nil {
		where += " AND d.disabled = ?"
		if *f.Disabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if f.UserID > 0 {
		where += " AND d.user_id = ?"
		args = append(args, f.UserID)
	}

	var total int64
	countQ := "SELECT COUNT(*) FROM devices d JOIN users u ON u.id = d.user_id " + where
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计设备总数失败: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	args = append(args, limit, f.Offset)
	q := `SELECT d.user_id, d.device_id, d.role, d.platform, d.name, d.last_ip,
	             d.disabled, d.last_seen_at, d.created_at, u.username
	        FROM devices d JOIN users u ON u.id = d.user_id ` + where + `
	       ORDER BY d.last_seen_at DESC
	       LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询设备列表失败: %w", err)
	}
	defer rows.Close()
	var out []*DeviceRow
	for rows.Next() {
		var r DeviceRow
		var disabled int
		if err := rows.Scan(&r.UserID, &r.DeviceID, &r.Role, &r.Platform, &r.Name, &r.LastIP,
			&disabled, &r.LastSeenAt, &r.CreatedAt, &r.Username); err != nil {
			return nil, 0, fmt.Errorf("读取设备行失败: %w", err)
		}
		r.Disabled = disabled != 0
		out = append(out, &r)
	}
	return out, total, rows.Err()
}

// CountDevices 统计某用户登记过的设备总数（含离线）。
func (s *MySQLStore) CountDevices(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE user_id = ?`, userID).Scan(&n)
	return n, err
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
// 注意：MySQL 默认在「新值与旧值相同」时 RowsAffected 返回 0，不能据此判定设备不存在，
// 因此 UPDATE 影响 0 行时再 SELECT 一次确认真实情况。
func (s *MySQLStore) UpdateDeviceName(ctx context.Context, userID int64, deviceID, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE user_id = ? AND device_id = ?`,
		name, userID, deviceID)
	if err != nil {
		return fmt.Errorf("更新设备名称失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		exists, err := s.deviceExists(ctx, userID, deviceID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrDeviceNotFound
		}
	}
	return nil
}

// UpdateDeviceStatus 启用/禁用指定账号下的一台设备。
// 同样存在「值未变时 RowsAffected=0」的问题，需要二次确认设备是否存在。
func (s *MySQLStore) UpdateDeviceStatus(ctx context.Context, userID int64, deviceID string, disabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET disabled = ? WHERE user_id = ? AND device_id = ?`,
		disabled, userID, deviceID)
	if err != nil {
		return fmt.Errorf("更新设备状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		exists, err := s.deviceExists(ctx, userID, deviceID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrDeviceNotFound
		}
	}
	return nil
}

// deviceExists 判断指定 user_id + device_id 的设备是否存在。
func (s *MySQLStore) deviceExists(ctx context.Context, userID int64, deviceID string) (bool, error) {
	var cnt int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM devices WHERE user_id = ? AND device_id = ?`,
		userID, deviceID).Scan(&cnt); err != nil {
		return false, fmt.Errorf("查询设备是否存在失败: %w", err)
	}
	return cnt > 0, nil
}
