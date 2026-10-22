package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrAPIKeyNotFound 接口密钥不存在或已吊销。
var ErrAPIKeyNotFound = errors.New("API Key 不存在")

// APIKey 一行开放接口密钥记录。明文密钥只在创建时返回一次，库里只存 SHA-256。
type APIKey struct {
	ID         int64
	UserID     int64
	Username   string // 列表时 JOIN users 带出，便于后台展示
	KeyHash    string
	KeyPrefix  string // 明文前缀，仅供后台辨识（如 cs_live_ab12…）
	Name       string // 备注/用途
	DeviceID   string // 可选：限定某台移动设备（对应中继消息的 from）
	ExpiresAt  sql.NullTime
	Disabled   bool
	LastUsedAt sql.NullTime
	CreatedAt  time.Time
}

// migrateAPIKeys 建开放接口 ApiKey 表。幂等。
func (s *MySQLStore) migrateAPIKeys(ctx context.Context) error {
	stmt := `CREATE TABLE IF NOT EXISTS api_keys (
		id          BIGINT       NOT NULL AUTO_INCREMENT,
		user_id     BIGINT       NOT NULL,
		key_hash    CHAR(64)     NOT NULL,
		key_prefix  VARCHAR(24)  NOT NULL DEFAULT '',
		name        VARCHAR(64)  NOT NULL DEFAULT '',
		device_id   VARCHAR(64)  NOT NULL DEFAULT '',
		expires_at  DATETIME     NULL,
		disabled    TINYINT(1)   NOT NULL DEFAULT 0,
		last_used_at DATETIME    NULL,
		created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		UNIQUE KEY uk_apikeys_hash (key_hash),
		KEY idx_apikeys_user (user_id),
		CONSTRAINT fk_apikeys_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("初始化 api_keys 表失败: %w", err)
	}
	return nil
}

// CreateAPIKey 写入一条新密钥。
func (s *MySQLStore) CreateAPIKey(ctx context.Context, k *APIKey) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys
		   (user_id, key_hash, key_prefix, name, device_id, expires_at, disabled)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		k.UserID, k.KeyHash, k.KeyPrefix, k.Name, k.DeviceID, k.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("创建 API Key 失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetAPIKeyByHash 按哈希查一条启用中的密钥（供开放接口鉴权）。
func (s *MySQLStore) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, key_hash, key_prefix, name, device_id,
		        expires_at, disabled, last_used_at, created_at
		   FROM api_keys WHERE key_hash = ? LIMIT 1`, hash)
	return scanAPIKey(row)
}

// ListAPIKeys 列出密钥；userID>0 时只看该用户。JOIN users 带出 username。
func (s *MySQLStore) ListAPIKeys(ctx context.Context, userID int64) ([]*APIKey, error) {
	q := `SELECT k.id, k.user_id, COALESCE(u.username,''), k.key_hash, k.key_prefix, k.name,
	             k.device_id, k.expires_at, k.disabled, k.last_used_at, k.created_at
	        FROM api_keys k LEFT JOIN users u ON u.id = k.user_id`
	args := []any{}
	if userID > 0 {
		q += ` WHERE k.user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY k.id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 API Key 列表失败: %w", err)
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKeyWithUsername(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TouchAPIKeyUsed 更新最近使用时间（best-effort，错误不阻断请求）。
func (s *MySQLStore) TouchAPIKeyUsed(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = NOW() WHERE id = ?`, id)
}

// RevokeAPIKey 吊销（删除）一条密钥；userID>0 时限定归属，防止越权删别人的。
func (s *MySQLStore) RevokeAPIKey(ctx context.Context, id, userID int64) error {
	q := `DELETE FROM api_keys WHERE id = ?`
	args := []any{id}
	if userID > 0 {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("吊销 API Key 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanAPIKey(row scannable) (*APIKey, error) {
	var k APIKey
	var disabled int
	if err := row.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name,
		&k.DeviceID, &k.ExpiresAt, &disabled, &k.LastUsedAt, &k.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("读取 API Key 失败: %w", err)
	}
	k.Disabled = disabled != 0
	return &k, nil
}

// scanAPIKeyWithUsername 列表查询用，字段顺序含 JOIN 出的 username（第 3 列）。
func scanAPIKeyWithUsername(row scannable) (*APIKey, error) {
	var k APIKey
	var disabled int
	if err := row.Scan(&k.ID, &k.UserID, &k.Username, &k.KeyHash, &k.KeyPrefix, &k.Name,
		&k.DeviceID, &k.ExpiresAt, &disabled, &k.LastUsedAt, &k.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("读取 API Key 失败: %w", err)
	}
	k.Disabled = disabled != 0
	return &k, nil
}
