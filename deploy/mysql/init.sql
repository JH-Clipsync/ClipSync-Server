-- ClipSync 初始化脚本
-- 挂到 mysql 容器的 /docker-entrypoint-initdb.d/ 下，首次启动时自动执行。
-- 服务端启动时也会跑一份等价的 CREATE TABLE IF NOT EXISTS（见 store_mysql.go 的 migrate），
-- 所以连已有实例、没跑过这个脚本也不会缺表。

CREATE DATABASE IF NOT EXISTS clipsync
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_unicode_ci;

USE clipsync;

-- 用户表：密码只存 scrypt 哈希（格式 scrypt$N$r$p$salt$dk）
CREATE TABLE IF NOT EXISTS users (
  id            BIGINT       NOT NULL AUTO_INCREMENT,
  username      VARCHAR(64)  NOT NULL,
  nickname      VARCHAR(64)  NOT NULL DEFAULT '',
  password_hash VARCHAR(255) NOT NULL,
  disabled      TINYINT(1)   NOT NULL DEFAULT 0,
  created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 会话表：一个用户最多一条活跃会话（user_id 主键），
-- 该用户的所有设备共享同一个 token，正好对应"同组转发"的路由模型。
-- token 同样只存 SHA-256 哈希；明文只在 Redis 里带 TTL 暂存。
CREATE TABLE IF NOT EXISTS sessions (
  user_id    BIGINT   NOT NULL,
  token_hash CHAR(64) NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at DATETIME NOT NULL,
  PRIMARY KEY (user_id),
  UNIQUE KEY uk_sessions_token (token_hash),
  KEY idx_sessions_expires (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 设备表：记录每个账号下出现过的设备，支持按设备禁用/解禁/踢下线。
-- 同一账号下 device_id 唯一；首次握手自动建档，role/platform 以最新一次为准。
CREATE TABLE IF NOT EXISTS devices (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
