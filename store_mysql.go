package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ===== 数据模型 =====

// User 一个账号。密码只存 scrypt 哈希。
type User struct {
	ID           int64
	Username     string
	Nickname     string
	PasswordHash string
	Disabled     bool
	CreatedAt    time.Time
}

// Session 一次成功登录换到的 token。
//
// 设计要点：**同一用户复用同一条会话**。
//   - 该用户当前没有任何客户端登录 → 新生成 token
//   - 已有客户端登录 → 直接把现有 token 返回给新客户端
//
// 因此这里对 user_id 建唯一索引，一个用户最多一条活跃会话，
// 所有设备共享同一个 token（也正好满足"同 token 分组转发"的既有路由模型）。
type Session struct {
	UserID    int64
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrUserExists 注册时用户名已被占用
var ErrUserExists = errors.New("用户名已存在")

// ErrUserNotFound 用户不存在
var ErrUserNotFound = errors.New("用户不存在")

// MySQLStore 负责用户与会话的持久化。
type MySQLStore struct {
	db *sql.DB
}

// OpenMySQL 建连并做一次 Ping，顺带建表（幂等）。
func OpenMySQL(cfg MySQLConfig) (*MySQLStore, error) {
	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetimeSec) * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}

	s := &MySQLStore{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *MySQLStore) Close() error { return s.db.Close() }

// migrate 建表。跟 deploy/mysql/init.sql 内容一致，
// 保证不挂 init 脚本（比如连的是已有实例）时也能自建。
func (s *MySQLStore) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            BIGINT       NOT NULL AUTO_INCREMENT,
			username      VARCHAR(64)  NOT NULL,
			nickname      VARCHAR(64)  NOT NULL DEFAULT '',
			password_hash VARCHAR(255) NOT NULL,
			disabled      TINYINT(1)   NOT NULL DEFAULT 0,
			created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_users_username (username)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 兼容旧库：若 users 表已存在但没有 nickname 列，自动补上。
		`ALTER TABLE users ADD COLUMN nickname VARCHAR(64) NOT NULL DEFAULT '' AFTER username`,
		`CREATE TABLE IF NOT EXISTS sessions (
			user_id    BIGINT      NOT NULL,
			token_hash CHAR(64)    NOT NULL,
			created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME    NOT NULL,
			PRIMARY KEY (user_id),
			UNIQUE KEY uk_sessions_token (token_hash),
			KEY idx_sessions_expires (expires_at),
			CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// ADD COLUMN 在新库（CREATE TABLE 已带该列）上会报 1060 重复列名，忽略
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == 1060 {
				continue
			}
			return fmt.Errorf("初始化表结构失败: %w", err)
		}
	}
	// devices 表单独建，方便后续扩展
	if err := s.migrateDevices(ctx); err != nil {
		return err
	}
	return nil
}

// CreateUser 注册。用户名唯一冲突时返回 ErrUserExists。
func (s *MySQLStore) CreateUser(ctx context.Context, username, nickname, passwordHash string) (*User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, nickname, password_hash) VALUES (?, ?, ?)`,
		username, nickname, passwordHash)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return nil, ErrUserExists
		}
		return nil, fmt.Errorf("创建用户失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Nickname: nickname, PasswordHash: passwordHash, CreatedAt: time.Now()}, nil
}

// FindUserByName 按用户名查，找不到返回 ErrUserNotFound。
func (s *MySQLStore) FindUserByName(ctx context.Context, username string) (*User, error) {
	var u User
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, nickname, password_hash, disabled, created_at FROM users WHERE username = ?`,
		username).Scan(&u.ID, &u.Username, &u.Nickname, &u.PasswordHash, &disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	u.Disabled = disabled != 0
	return &u, nil
}

// FindUserByID 按 ID 查，找不到返回 ErrUserNotFound。
func (s *MySQLStore) FindUserByID(ctx context.Context, userID int64) (*User, error) {
	var u User
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, nickname, password_hash, disabled, created_at FROM users WHERE id = ?`,
		userID).Scan(&u.ID, &u.Username, &u.Nickname, &u.PasswordHash, &disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	u.Disabled = disabled != 0
	return &u, nil
}

// GetSession 取用户当前会话；没有或已过期都返回 nil, nil。
func (s *MySQLStore) GetSession(ctx context.Context, userID int64) (*Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, token_hash, created_at, expires_at FROM sessions WHERE user_id = ?`,
		userID).Scan(&sess.UserID, &sess.TokenHash, &sess.CreatedAt, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询会话失败: %w", err)
	}
	if !sess.ExpiresAt.After(time.Now()) {
		return nil, nil // 过期视为不存在，交给调用方重新签发
	}
	return &sess, nil
}

// UpsertSession 写入/替换用户的活跃会话（一个用户一条）。
func (s *MySQLStore) UpsertSession(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE token_hash = VALUES(token_hash),
		                         created_at = VALUES(created_at),
		                         expires_at = VALUES(expires_at)`,
		sess.UserID, sess.TokenHash, sess.CreatedAt, sess.ExpiresAt)
	if err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

// FindUserByTokenHash 用 token 哈希反查用户（WebSocket 鉴权走这条路，Redis 未命中时回源）。
func (s *MySQLStore) FindUserByTokenHash(ctx context.Context, tokenHash string) (*User, error) {
	var u User
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.nickname, u.password_hash, u.disabled, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > NOW()`,
		tokenHash).Scan(&u.ID, &u.Username, &u.Nickname, &u.PasswordHash, &disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("按 token 查询用户失败: %w", err)
	}
	u.Disabled = disabled != 0
	return &u, nil
}

// UpdatePassword 修改用户密码哈希。
func (s *MySQLStore) UpdatePassword(ctx context.Context, userID int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	if err != nil {
		return fmt.Errorf("修改密码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// DeleteSession 登出：删掉用户的活跃会话。
func (s *MySQLStore) DeleteSession(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("删除会话失败: %w", err)
	}
	return nil
}

// PurgeExpiredSessions 清理过期会话，返回删除条数。由后台定时任务调用。
func (s *MySQLStore) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, fmt.Errorf("清理过期会话失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
