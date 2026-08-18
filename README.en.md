<p align="center">
  <img src="icon.png" width="128" alt="ClipSync Logo"/>
</p>

<h1 align="center">ClipSync-Server</h1>

<p align="center">
  <b>Relay server for the ClipSync three-endpoint sync system</b><br/>
  <a href="README.md">简体中文</a> ·
  <a href="README.en.md">English</a> ·
  <a href="README.ja.md">日本語</a>
</p>

---

ClipSync-Server is the core relay service of ClipSync's self-hosted cross-device messaging sync system, built with **Go + gorilla/websocket + MySQL + Redis**. It is responsible for forwarding **SMS verification codes** received on the phone and copied **text/images** to the PC in real time, and vice versa.

It does not rely on any third-party push service — all traffic goes through your own WebSocket channel. End-to-end encryption is optional, so privacy stays under your control. It listens on port **28001** by default.

---

## ✨ Key Features

| Module | Description |
|--------|-------------|
| 🔄 **Real-time WebSocket forwarding** | Routes messages by `userID` group, supporting four delivery semantics: `notify_pc` / `notify_mobile` / `notify_all` / `clipboard`; `pc` and `mobile` roles send to each other without loops |
| 👥 **Online device management** | The in-memory Hub authoritatively maintains real WebSocket connections; a Redis Hash (`clipsync:online:<userID>`, TTL 90s) records online status with a 30s heartbeat renewal. Connections are cleaned up automatically on disconnect, and no ghost records remain even if the process is killed |
| 📡 **Real-time Presence push** | When a device comes online or goes offline, `presence` messages are proactively pushed to all connections in the same group, so clients can refresh the online-device UI (platform / IP / capability bits / custom name) in real time |
| 👤 **User system** | Registration / login, scrypt password hashing (N=32768, r=8, p=1), random tokens (32 bytes, only the SHA-256 hash is stored), token TTL (default 720 hours / 30 days), per-IP login rate limiting |
| 📱 **Device table management** | The `devices` table persistently records every device that has appeared under an account (role / platform / custom name / last IP); records are auto-created on first handshake. Administrators can **disable a device**, after which its handshake is immediately rejected |
| 👟 **Force offline** | Supports kicking all devices of a user or a single device by ID; password reset / user ban / user deletion triggers automatic kicks. 5 kick reasons (password reset / user banned / user deleted / device kicked / device disabled) |
| 🛡️ **Admin API** | `GET /server-admin/users/{id}/devices` (online status based on the in-memory Hub, supplemented by Redis), device enable/disable / rename, cross-user paginated device search, and the unified `POST /server-admin/kick` action endpoint; Bearer Token authentication using constant-time comparison |
| 📨 **Redis Pub/Sub integration** | When sharing Redis with ClipSync-Admin, control commands are delivered through the `clipsync:admin:kick_user` channel, with HTTP API as a fallback — double insurance |
| 🧹 **SMS payload cleaning** | Automatically strips `【+86xxx】` / `[N条]` prefixes, extracts the 11-digit sender phone number into `sender`, and trims whitespace, so downstream clients don't have to deal with mobile-side injections |
| 🔐 **End-to-end encryption gate** | When `e2ee.require=true`, the server refuses to forward any plaintext message and disables the `/push` plaintext endpoint; for ciphertext, the server only relays and cannot see the content |
| 📝 **Daily log rotation** | General logs `logs/clipsync.log` plus message push logs `logs/message.log`, archived daily into `logs/clipsync/` and `logs/message/` subdirectories, with configurable retention days |
| 🐳 **Docker-native** | Multi-stage build → distroless nonroot image (no shell, non-root, minimal attack surface), supports host networking to connect directly to the host's MySQL/Redis, with volumes for persistent config and logs |

---

## 🚀 Quick Start

### Option 1: Docker Compose (recommended)

The bundled `docker-compose.yml` uses **host networking** to connect directly to an existing MySQL / Redis on the host — it does not spin up extra containers:

```bash
# 1. Prepare config
mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# Edit config/config.yaml and fill in mysql.password / redis.password / admin_token

# 2. Prepare .env (determines the initial account password, etc.)
cp .env.example .env
vim .env   # At minimum, change BOOTSTRAP_PASSWORD

# 3. Start
docker compose up -d
docker compose logs -f clipsync
```

After successful startup:

- The service listens on `:28001` (host networking binds directly to the host port)
- The initial account is specified by `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` in `.env`
- The config file is mounted at `./config/config.yaml`, and logs are written to `./logs/`

> If you want Compose to bring up MySQL + Redis + Server all at once, you can refer to `deploy/config.compose.yaml` and extend the service definitions yourself.

### Option 2: Docker one-liner

```bash
docker run -d --name clipsync-server \
  --network host \
  --restart unless-stopped \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  -e TZ=Asia/Shanghai \
  -e CLIPSYNC_TRUST_PROXY=true \
  ghcr.io/jh-clipsync/clipsync-server:latest
```

### Option 3: Binary + systemd

Download the tarball for your platform from [Releases](https://github.com/JH-Clipsync/ClipSync-Server/releases) (`linux-amd64` / `linux-arm64` / `darwin-arm64`), extract it, and run:

```bash
sudo ./install.sh
```

The script will:

1. Install to `/opt/clipsync-server/`
2. Register and start `clipsync-server.service` (systemd)
3. Enable auto-start on boot and automatic restart on failure
4. Write logs to `/opt/clipsync-server/logs/clipsync.log`

Common commands:

```bash
sudo systemctl status clipsync-server
sudo systemctl restart clipsync-server
tail -f /opt/clipsync-server/logs/clipsync.log
```

### Option 4: Build from source

Requires Go 1.23+:

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

# Run directly
go run .

# Or build a static binary
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=1.0.0" -o clipsync-server .
./clipsync-server --config config.yaml
```

Check the version / export the full default config:

```bash
./clipsync-server --version
./clipsync-server --print-default-config > config.yaml
```

---

## ⚙️ Configuration

The config uses `go:embed` to compile [config.default.yaml](config.default.yaml) into the binary as the single source of default values. Configuration priority:

```
built-in defaults  <  config.yaml  <  environment variables
```

Config file lookup order:

1. `--config` command-line flag
2. `CLIPSYNC_CONFIG` environment variable
3. `./config.yaml`
4. `/etc/clipsync/config.yaml`

If the file does not exist, no error is raised and defaults are used. Run `clipsync-server --print-default-config` to see the full list of fields.

### Main configuration sections

| Section | Key | Description |
|---------|-----|-------------|
| `server` | `addr: ":28001"` | Listen address |
| | `trust_proxy: false` | Set to `true` behind a reverse proxy so the real IP is taken from `X-Forwarded-For` |
| | `admin_token: ""` | Bearer Token for the `/server-admin/*` endpoints; if left empty, all admin endpoints return 503. Recommend generating with `openssl rand -hex 32` |
| `logs` | `dir` / `level` / `stdout` / `max_age_days` | Log directory, level (debug/info/warn/error), whether to also print to stdout, archive retention days (0 = forever) |
| `websocket` | `read_limit: 10MB` | Maximum size of a single message — clipboard images can be large |
| | `ping_interval_sec: 30` | Server ping interval |
| | `read_deadline_sec: 60` | Read timeout, which determines the window for cleaning up dead connections |
| | `send_queue_size: 32` | Per-client send queue length |
| `mysql` | `host/port/user/password/database` | User / session / device persistence; tables are auto-created on startup |
| | `max_open_conns / max_idle_conns` | Connection pool tuning |
| `redis` | `addr / password / db` | Token cache + online device registry + Pub/Sub |
| | `key_prefix: "clipsync:"` | Unified prefix for all keys |
| | `online_ttl_sec: 90` | Online registry TTL; during a connection it is renewed every TTL/3 (~30s) via heartbeat |
| `auth` | `token_ttl_hours: 720` | Token validity period (30 days) |
| | `allow_register: false` | Whether to expose `POST /auth/register`; disabled by default, accounts are created by admins |
| | `min_password_len: 8` | Minimum password length |
| | `bootstrap_user / bootstrap_password` | Initial account auto-created on startup (skipped if it already exists) |
| | `login_rate_limit_per_min: 10` | Per-IP login attempt limit per minute to prevent brute force (0 = unlimited) |
| `e2ee` | `require: false` | When `true`, refuses to forward any plaintext message and disables the `/push` plaintext endpoint |
| `message_protocol` | `check_origin: true` | Whether to allow WebSocket handshakes from any Origin (in production, recommend switching to a whitelist) |
| | `max_payload_preview: 40` | Number of characters to truncate the payload preview in logs |

### Environment variable overrides

A few key runtime parameters support environment variable overrides:

| Environment variable | Corresponding field |
|----------------------|---------------------|
| `CLIPSYNC_ADDR` | `server.addr` |
| `CLIPSYNC_LOG_DIR` | `logs.dir` |
| `CLIPSYNC_LOG_LEVEL` | `logs.level` |
| `CLIPSYNC_TRUST_PROXY` | `server.trust_proxy` |
| `CLIPSYNC_WS_READ_LIMIT` | `websocket.read_limit` |
| `CLIPSYNC_TOKEN_TTL_HOURS` | `auth.token_ttl_hours` |
| `CLIPSYNC_ALLOW_REGISTER` | `auth.allow_register` |
| `CLIPSYNC_BOOTSTRAP_USER` | `auth.bootstrap_user` |
| `CLIPSYNC_BOOTSTRAP_PASSWORD` | `auth.bootstrap_password` |
| `CLIPSYNC_E2EE_REQUIRE` | `e2ee.require` |

> MySQL / Redis connection info intentionally does not support environment variable overrides — just edit the config file and restart, to avoid "I changed the config but a leftover env var silently overrode it."

---

## 📡 API Reference

### Client endpoints (authenticated with user token)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/ws` | WebSocket upgrade endpoint; query params `token` / `device` / `role` (pc/mobile, legacy value phone also accepted) / `platform` / `caps` / `name` |
| POST | `/auth/register` | Registration (controlled by the `auth.allow_register` switch) |
| POST | `/auth/login` | Login, returns a token; if a client is already online, the same token is reused and `reused=true` is returned |
| POST | `/auth/logout` | Invalidates the current token |
| GET | `/auth/session` | Queries the current session status and online devices |
| POST | `/auth/change-password` | Changes the password; the old token is immediately invalidated and all devices are kicked, a new token is returned |
| POST | `/push` | Convenience HTTP push endpoint (for curl debugging; disabled when `e2ee.require=true`) |
| POST | `/device/name` | Lets a user rename their own device (broadcasts presence in real time) |
| GET | `/health` | Health check, returns `ok` |

### Admin endpoints (authenticated with `admin_token`)

All `/server-admin/*` endpoints require the header:

```
Authorization: Bearer <server.admin_token>
```

| Method | Path | Description |
|--------|------|-------------|
| POST | `/server-admin/users` | Admin creates a user (not restricted by `allow_register`) |
| GET | `/server-admin/users/{id}/devices` | Lists all devices of a user; **online status is based on the in-memory Hub, supplemented by Redis** |
| GET | `/server-admin/devices` | Cross-user paginated device search, supports `keyword` / `disabled` / `user_id` / `page` / `page_size` |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/status` | Enables / disables a device; a disabled device is immediately kicked |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/name` | Renames a device and broadcasts presence to online connections |
| POST | `/server-admin/kick` | Unified action endpoint; body supports `kick_user` / `kick_device` / `disable_device` / `enable_device` |

### WebSocket message types

| `type` | Delivery scope | Typical use case |
|--------|----------------|------------------|
| `notify_pc` | All `pc` roles in the same group | Syncing SMS verification codes to the computer |
| `notify_mobile` | All `mobile` roles in the same group | Pushing notifications from the computer to the phone |
| `notify_all` | All devices in the same group (except self) | General broadcast |
| `clipboard` | All devices in the same group (except self) | Clipboard text / images; the receiver decides whether to auto-write based on its settings |
| `presence` | Server-to-client only | Notification that the online device list has changed |
| `server_kick` | Server-to-client only | Forced offline, includes a `reason` field |

### Redis Pub/Sub channel

- Channel name: `{redis.key_prefix}admin:kick_user` (default `clipsync:admin:kick_user`)
- Message body is JSON: `{"action":"kick_user|kick_device|disable_device|enable_device","user_id":1,"device_id":"...","reason":"..."}`
- Compatible with plain numeric `userID` (equivalent to `kick_user`)
- Admin is the publisher; Server subscribes and auto-reconnects (exponential backoff, max 30s)

---

## 🏗️ Project Architecture

```
┌──────────────┐  WebSocket   ┌──────────────────────────────┐
│  PC/Mobile   │ ──────────▶  │      ClipSync-Server         │
│   Clients    │ ◀──────────  │  ┌────────────────────────┐  │
└──────────────┘   presence   │  │  Hub (in-memory)       │  │
                              │  │  userID -> []*Client   │  │
                              │  └──────────┬─────────────┘  │
                              │             │                │
                              │  ┌──────────▼─────────────┐  │
                              │  │  AuthService           │  │
                              │  │  ┌──────┐  ┌────────┐  │  │
                              │  │  │MySQL │  │ Redis  │  │  │
                              │  │  └──────┘  └────────┘  │  │
                              │  └────────────────────────┘  │
                              └──────────┬───────────────────┘
                                         │ Redis Pub/Sub + HTTP
                                         ▼
                              ┌──────────────────────────────┐
                              │       ClipSync-Admin         │
                              └──────────────────────────────┘
```

### Data model

- **users**: accounts; only the scrypt hash of the password is stored; `disabled` controls bans
- **sessions**: a user has at most one active session (`user_id` is the primary key); all devices share the same token; only the SHA-256 hash of the token is stored
- **devices**: composite primary key `(user_id, device_id)`, records role / platform / custom name / last IP / disabled status / last online time

### Code structure

| File | Responsibility |
|------|----------------|
| [main.go](main.go) | Hub / Client / WebSocket routing / presence broadcast / heartbeat / HTTP route entry / graceful shutdown |
| [config.go](config.go) / [config.default.yaml](config.default.yaml) | Config struct, loading order, environment variable overrides |
| [auth_service.go](auth_service.go) / [auth_http.go](auth_http.go) / [auth_crypto.go](auth_crypto.go) | Login / registration / session / scrypt + token hashing / rate limiting |
| [store_mysql.go](store_mysql.go) | User / session persistence and auto table creation |
| [store_device.go](store_device.go) | Device table CRUD, paginated search, disabled status |
| [store_redis.go](store_redis.go) | Token cache, online registry, Pub/Sub admin channel |
| [admin_http.go](admin_http.go) | `/server-admin/*` endpoint implementation |
| [e2ee.go](e2ee.go) | End-to-end encryption policy gate |
| [logger.go](logger.go) | Daily rotation + archive retention + dual general/message logs |
| [device_name_http.go](device_name_http.go) | User self-service device rename |

---

## 🔐 Security Notes

- **Password storage**: scrypt (N=32768, r=8, p=1, 32-byte derived key, 16-byte random salt), format `scrypt$N$r$p$salt$dk`; parameters are stored with the hash for smooth future upgrades
- **Token storage**: 32-byte random token; only the SHA-256 hash is stored in MySQL / Redis; the plaintext token is only cached in Redis with a TTL for "same account, multiple devices reuse the same session"
- **Login rate limiting**: per-IP sliding window, default 10 attempts/minute; wrong username or password returns the same error to prevent username enumeration
- **Admin API auth**: if `admin_token` is empty, returns 503; comparison uses `subtle.ConstantTimeCompare` to prevent timing attacks
- **Reverse proxy trust**: when `trust_proxy=false`, `X-Forwarded-For` is ignored to avoid being fooled by forged headers
- **WebSocket Origin**: in production, recommend setting `message_protocol.check_origin` to `false` and validating against a whitelist
- **End-to-end encryption**: after all three endpoints have been upgraded to versions that support encryption, set `e2ee.require` to `true` and the server will reject any plaintext message
- **Container security**: distroless nonroot image, no shell, no package manager, runs as uid 65532 (non-root)
- **Transport security**: in production, be sure to put Nginx / Caddy in front to terminate TLS (see examples below)

### Nginx reverse proxy example

```nginx
location = /clipsync/ws {
    proxy_pass http://127.0.0.1:28001/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}

location /clipsync/ {
    proxy_pass http://127.0.0.1:28001/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Remember to set `server.trust_proxy` to `true` in `config.yaml`.

### Caddy reverse proxy example

The repo provides [Caddyfile.example](Caddyfile.example):

```
clipsync.example.com {
    reverse_proxy 127.0.0.1:28001
}
```

Caddy will automatically apply for and renew Let's Encrypt certificates.

---

## 🐛 Troubleshooting

| Symptom | Where to look |
|---------|---------------|
| Client handshake returns 401 | Token is invalid or expired; check whether `clipsync:token:*` exists in Redis; verify the server time is accurate |
| Client handshake returns 403 "Device disabled by administrator" | `devices.disabled=1`; unblock the device in the admin panel |
| Client disconnects immediately after connecting | Check whether `websocket.read_deadline_sec` is too small; whether the reverse proxy is swallowing Ping/Pong frames; whether Nginx has `proxy_read_timeout` set |
| Online status is inaccurate | The in-memory Hub is the authoritative source; Redis online TTL defaults to 90s and is renewed every 30s. If Redis is flushed, it will recover automatically after reconnect; after an abnormal process exit, wait 90s for the key to expire naturally |
| Admin API returns 503 | `server.admin_token` is not configured; configure it and restart |
| Admin API returns 401 | The `Authorization: Bearer <token>` header does not match the server's `admin_token` |
| Login says "too many attempts" | The per-IP rate limit was triggered; wait 1 minute or increase `auth.login_rate_limit_per_min` |
| Behind a reverse proxy, all IPs in logs are 127.0.0.1 | Set `server.trust_proxy` to `true` and ensure the reverse proxy sets `X-Forwarded-For` |
| Clipboard images are not received | Check `websocket.read_limit` (default 10MB) and the reverse proxy's `client_max_body_size` |
| Docker container cannot connect to MySQL / Redis | With host networking, use `127.0.0.1:<host port>`; confirm MySQL allows connections from `127.0.0.1` and that the port is not blocked by a firewall |
| Log files are not generated | Check the permissions of `logs.dir`; in the distroless nonroot image, uid 65532 needs write access to the log directory; the compose file already uses `user: "0:0"` as a fallback |

### Log locations

- General logs: `logs/clipsync.log` (today) + `logs/clipsync/clipsync-YYYY-MM-DD.log` (archive)
- Message push logs: `logs/message.log` (today) + `logs/message/message-YYYY-MM-DD.log` (archive)

The message log is a separate file that records each message's received / sent / dropped status, business category (SMS / clipboard / notification), content format (text / image), and push scope, making it easy to audit and troubleshoot.

---

## 🤝 Related Projects

- [ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin): Admin backend (Go + Gin + GORM)
- [ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web): Admin frontend (Vue 3 + Element Plus)
- [ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows): Windows client
- [ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac): macOS client
- [ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android): Android client
