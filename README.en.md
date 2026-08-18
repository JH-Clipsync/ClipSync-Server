<p align="center">
  <img src="icon.png" width="128" alt="ClipSync Logo"/>
</p>

<h1 align="center">ClipSync-Server</h1>

<p align="center">
  <b>Relay server for the ClipSync cross-device sync family</b><br/>
  <a href="README.md">简体中文</a> ·
  <a href="README.en.md">English</a> ·
  <a href="README.ja.md">日本語</a>
</p>

---

ClipSync-Server is the core relay of the self-hosted ClipSync messaging system, built with **Go + gorilla/websocket + MySQL + Redis**. It forwards **SMS verification codes** and **text/image clipboard** content received on the phone to PC clients in real time, and vice versa.

No third-party push services are involved — all traffic flows through your own WebSocket endpoint, with optional end-to-end encryption. Your data stays under your control.

---

## ✨ Features

| Area | What it does |
|------|--------------|
| 🔄 **Real-time WebSocket relay** | Routes by `userID`. Four delivery semantics: `notify_pc` / `notify_mobile` / `notify_all` / `clipboard`. `pc` and `mobile` roles exchange messages without cross-talk |
| 👥 **Online device tracking** | The in-memory Hub is the source of truth for live WebSocket connections. A Redis Hash (`clipsync:online:<userID>`, 90s TTL) records presence; entries expire naturally even if the process is killed |
| 📡 **Live presence push** | On device connect/disconnect, the server broadcasts a `presence` message to all peer connections so clients can refresh their online-device UI (platform, IP, capabilities, custom name) in real time |
| 👤 **User system** | Register/login, scrypt password hashing (N=32768, r=8, p=1), JWT-style tokens with configurable TTL (default 30 days), per-IP login rate limiting |
| 📱 **Device registry** | The `devices` table persists every device seen on an account (role, platform, custom name, last IP). Admins can **ban devices**, after which the handshake is rejected |
| 👟 **Force disconnect** | Kick all devices of a user or a single device; auto-triggered on password reset / user ban / user deletion. Five kick reasons: password_reset, user_disabled, user_deleted, device_kicked, device_banned |
| 🛡️ **Admin HTTP API** | `GET /server-admin/users/{id}/devices` (online status sourced from the in-memory Hub), device enable/disable/rename, cross-user paginated device search, unified `POST /server-admin/kick` entry point; Bearer Token auth with constant-time comparison |
| 📨 **Redis Pub/Sub integration** | When sharing Redis with ClipSync-Admin, control commands are delivered over channel `clipsync:admin:kick_user`; the HTTP API serves as a fallback for double safety |
| 🧹 **SMS payload sanitization** | Strips `【+86xxx】` / `[N items]` prefixes, extracts the 11-digit sender phone number into `sender`, trims whitespace — downstream clients no longer need to handle carrier-injected noise |
| 🔐 **E2EE gate** | With `e2ee.require=true` the server refuses to relay any plaintext message and disables the `/push` plaintext endpoint; ciphertext is forwarded as-is |
| 📝 **Daily-rotating logs** | General logs in `logs/clipsync.log`, message relay audit in `logs/message.log`, archived daily into `logs/clipsync/` and `logs/message/`; configurable retention |
| 🐳 **Docker-native** | Multi-stage build producing a distroless nonroot image (~20MB), host networking for direct access to host MySQL/Redis, volumes for config and logs |

---

## 🏗️ Tech Stack

- **Language**: Go 1.23
- **WebSocket**: [gorilla/websocket](https://github.com/gorilla/websocket) v1.5
- **Database**: MySQL 8 (users / sessions / devices; auto-migrated on startup)
- **Cache**: Redis 7 (token cache + online device registry + Pub/Sub control channel)
- **Password hashing**: scrypt (`golang.org/x/crypto/scrypt`)
- **Configuration**: YAML with `go:embed`'d defaults, environment-variable overrides
- **Runtime image**: `gcr.io/distroless/base-debian12:nonroot` (no shell, non-root)

---

## 🚀 Quick Start

### Option 1: Docker Compose (recommended)

The root `docker-compose.yml` uses host networking and connects to MySQL/Redis already running on the host:

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

cp .env.example .env
# Edit .env — BOOTSTRAP_PASSWORD is required; adjust TOKEN_TTL_HOURS / ALLOW_REGISTER as needed

mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# Edit config/config.yaml with your real mysql.password / redis.password

docker compose up -d
docker compose logs -f clipsync
```

Verify:

```bash
curl http://127.0.0.1:28001/health   # should return "ok"
```

Default listen address is `:28001`; the bootstrap account comes from `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` in `.env`.

### Option 2: Pull the official image

```bash
docker run -d --name clipsync-server \
  --network host \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  -e TZ=Asia/Shanghai \
  ghcr.io/jh-clipsync/clipsync-server:latest
```

Image registry: [ghcr.io/jh-clipsync/clipsync-server](https://github.com/orgs/JH-Clipsync/packages)

### Option 3: Build from source

```bash
# Requires Go 1.23+ and reachable MySQL 8 / Redis
go build -ldflags "-X main.version=1.0.0" -o clipsync-server .
./clipsync-server --print-default-config > config.yaml
# Edit config.yaml, then run
./clipsync-server --config config.yaml
```

---

## ⚙️ Configuration

All defaults live in [config.default.yaml](config.default.yaml), compiled into the binary via `go:embed`. **There are no hardcoded defaults in Go code.**

```bash
# Dump a fully-commented YAML to use as a starting point
./clipsync-server --print-default-config > config.yaml
```

Config lookup order: `--config` flag → `CLIPSYNC_CONFIG` env var → `./config.yaml` → `/etc/clipsync/config.yaml`. A missing file is not an error; built-in defaults are used.

| Field | Description | Default |
|---|---|---|
| `server.addr` | HTTP/WS listen address | `:28001` |
| `server.read_timeout` / `write_timeout` | HTTP read/write timeouts | `15s` |
| `server.shutdown_timeout` | Graceful shutdown wait | `10s` |
| `server.trust_proxy` | Trust XFF/XRI proxy headers | `false` |
| `server.admin_token` | Bearer token for `/server-admin/*`; empty disables the endpoints | empty |
| `logs.dir` / `level` / `stdout` / `max_age_days` | Log dir / level / echo to stdout / retention days | `logs` / `info` / `true` / `0` |
| `websocket.read_limit` | Max inbound message size (clipboard images can be large) | `10485760` (10MB) |
| `websocket.ping_interval_sec` | Ping interval | `30` |
| `websocket.send_queue_size` | Per-client send queue | `32` |
| `message_protocol.max_payload_preview` | Preview length in logs | `40` |
| `mysql.*` | MySQL connection (DSN auto-assembled) | `127.0.0.1:3306/clipsync` |
| `redis.addr` / `db` / `key_prefix` / `online_ttl_sec` | Redis address / DB / key prefix / online TTL | `127.0.0.1:6379` / `0` / `clipsync:` / `90` |
| `auth.token_ttl_hours` | Token lifetime in hours | `720` (30 days) |
| `auth.allow_register` | Whether `POST /auth/register` is open | `false` |
| `auth.min_password_len` | Minimum password length on register | `8` |
| `auth.bootstrap_user/password` | Initial account auto-created on startup | empty |
| `auth.login_rate_limit_per_min` | Max login attempts per IP per minute | `10` |
| `e2ee.require` | Reject plaintext messages, disable `/push` | `false` |

Only a few fields support environment-variable overrides: `CLIPSYNC_ADDR`, `CLIPSYNC_LOG_DIR`, `CLIPSYNC_LOG_LEVEL`, `CLIPSYNC_TRUST_PROXY`, `CLIPSYNC_WS_READ_LIMIT`, `CLIPSYNC_TOKEN_TTL_HOURS`, `CLIPSYNC_ALLOW_REGISTER`, `CLIPSYNC_BOOTSTRAP_USER`, `CLIPSYNC_BOOTSTRAP_PASSWORD`, `CLIPSYNC_E2EE_REQUIRE`.

> ⚠️ **MySQL/Redis connection info is read from the config file only** — no env-var overrides — so a stale exported variable can never silently clobber your `config.yaml`. Restart is required after any change; there is no hot reload.

---

## 🔌 API Reference

### Client endpoints

| Path | Method | Description |
|---|---|---|
| `/ws?token=&device=&role=pc\|mobile&platform=&caps=&name=` | GET (WS) | Long-lived client connection; auth + device admission on handshake |
| `/auth/register` | POST | Register (gated by `allow_register`) |
| `/auth/login` | POST | Exchange username/password for a token |
| `/auth/session` | GET | Current session + online devices |
| `/auth/logout` | POST | Logout and clear session |
| `/auth/change-password` | POST | Change own password |
| `/device/name` | POST | Rename the current device |
| `/push?token=` | POST (JSON) | Convenience push endpoint (curl-friendly; disabled when `e2ee.require` is on) |
| `/health` | GET | Health check |

### Admin endpoints (`/server-admin/*`, Bearer Token)

| Path | Method | Description |
|---|---|---|
| `/server-admin/users` | POST | Admin-created user (bypasses registration switch) |
| `/server-admin/users/{id}/devices` | GET | List all devices for a user, including online status (in-memory Hub is authoritative) |
| `/server-admin/users/{id}/devices/{deviceID}/status` | PUT | Enable/disable a device (disabling also kicks it) |
| `/server-admin/users/{id}/devices/{deviceID}/name` | PUT | Rename a device (broadcasts presence) |
| `/server-admin/devices` | GET | Cross-user paginated device search (keyword/disabled/user_id) |
| `/server-admin/kick` | POST | Unified kick entry (kick_user / kick_device / disable_device / enable_device) |

### Message frame example

```json
{ "type": "notify_pc", "kind": "sms_code", "text": "[MyBank] verification code 314159" }
```

```bash
# Push an SMS code to all PC clients
curl -X POST 'http://127.0.0.1:28001/push?token=<your-token>' \
  -H 'Content-Type: application/json' \
  -d '{"type":"notify_pc","kind":"sms_code","text":"[Test] your code is 314159"}'
```

---

## 🔐 Presence Model

```
       ┌──── In-memory Hub (authoritative) ────┐
       │  userID → set<*Client>  (live conns)  │
       └──────────────────┬────────────────────┘
                          │ heartbeat every ~30s
                          ▼
       Redis Hash  clipsync:online:<userID>
                 field=deviceID  value=role
                 TTL = 90s (refreshed at TTL/3)
```

- **The in-memory Hub is the source of truth**: if a connection exists, the device is online; if not, it isn't.
- The Redis Hash is primarily used by the login flow ("does this user already have a client online?") and for cross-process admin visibility.
- On `kill -9`, Redis entries expire naturally within 90s — no ghost online devices.

---

## 🐳 Deployment Architecture

### How it works with ClipSync-Admin

```
        ┌────────────────────┐        Redis Pub/Sub
        │  ClipSync-Admin    │ ───────────────────────▶  ClipSync-Server
        │  (admin :28002)    │   clipsync:admin:kick_user  (this app :28001)
        └─────────┬──────────┘ ◀───────────────────────  └────────┬─────────┘
                  │  HTTP fallback (admin_token)                  │
                  └───────────────────────────────────────────────┘
                               shared MySQL (clipsync DB)
```

- Both services share the same MySQL database `clipsync`;
- Server owns write access to `users` / `sessions` / `devices`;
- Admin notifies Server of kick/ban actions via Redis Pub/Sub, with HTTP fallback if Redis is unavailable;
- Device online status is fetched from Server's `/server-admin/users/{id}/devices` first, falling back to local MySQL+Redis on failure.

### Reverse proxy

Always front the service with Nginx/Caddy + TLS in production. See [Caddyfile.example](Caddyfile.example) and the nginx sample in [deploy/nginx.clipsync.conf](deploy/nginx.clipsync.conf) (located in the Admin repo, but the proxy snippet applies equally here).

Important: **WebSocket long connections need a long `proxy_read_timeout`** (3600s or more), and you must set `server.trust_proxy: true` to record real client IPs.

```nginx
location = /clipsync/ws {
    proxy_pass http://127.0.0.1:28001/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 3600s;
}
```

---

## 📁 Project Structure

```
ClipSync-Server/
├── main.go                 # Routes + WebSocket Hub + message routing + graceful shutdown
├── config.go               # Config struct + YAML loading + env overrides
├── config.default.yaml     # Embedded default config (single source of defaults)
├── auth_http.go            # /auth/* register/login/session/password HTTP handlers
├── auth_service.go         # Auth logic: login, session reuse, rate limiting
├── auth_crypto.go          # scrypt password hashing + token SHA-256
├── admin_http.go           # /server-admin/* admin HTTP API
├── device_name_http.go     # Self-service device rename
├── store_mysql.go          # MySQL: users / sessions tables
├── store_device.go         # MySQL: devices (upsert/ban/rename/paginated search)
├── store_redis.go          # Redis: token cache + online Hash + Pub/Sub
├── e2ee.go                 # End-to-end encryption gate + envelope parsing
├── logger.go               # Daily-rotating logger (general + message categories)
├── Dockerfile              # Multi-stage build → distroless nonroot
├── docker-compose.yml      # Host-network deployment compose
├── Caddyfile.example       # HTTPS reverse-proxy sample
├── deploy/
│   ├── config.compose.yaml   # Starting config for all-in-Docker deployment
│   ├── config.external.yaml  # Starting config when MySQL/Redis are on the host
│   └── mysql/init.sql        # DB init script (server also auto-migrates)
└── .github/workflows/docker-image.yml  # Tag push → multi-arch image build
```

---

## 🔐 Security

| Aspect | Design |
|--------|--------|
| Password storage | scrypt (N=32768, r=8, p=1, 32-byte derived key, 16-byte random salt) |
| Token storage | MySQL stores only SHA-256 hashes; plaintext tokens live in Redis with TTL only, so a DB dump cannot be replayed |
| Transport | Front with Nginx/Caddy for `wss://`; E2EE uses AES-256-GCM performed client-side |
| Brute-force defense | Per-IP login attempt limit (default 10/min) |
| Admin API | `admin_token` compared with `crypto/subtle.ConstantTimeCompare` to prevent timing attacks |
| Client spoofing | Server force-overwrites the `from` field; `ping/pong/presence` control frames are never forwarded |
| Image hardening | distroless nonroot (uid 65532), no shell, no package manager — minimal attack surface |

---

## 🐛 Troubleshooting

| Symptom | What to check |
|---------|---------------|
| Client can't connect | Firewall allows 28001; reverse proxy forwards Upgrade headers; use `ws://IP:28001/ws` |
| Logs show proxy IP instead of real IP | Enable `server.trust_proxy: true` and restart |
| Admin endpoints return 401 | Is `server.admin_token` set? Does the client send `Authorization: Bearer <token>`? |
| Logs disappear after container restart | Is `./logs:/data/logs` mounted? |
| Config changes have no effect | Config is loaded at startup; run `docker compose restart` or `systemctl restart` |
| Redis still shows online for a disconnected device | 90s TTL expiry; the in-memory Hub is authoritative for actual routing |
| `/push` returns 403 | Plaintext push is disabled when `e2ee.require=true` |

Logs: `logs/clipsync.log` (general), `logs/message.log` (message relay audit), archived daily under `logs/clipsync/` and `logs/message/`.

---

## 🤝 Related Projects

| Project | Stack | Link |
|---------|-------|------|
| Admin backend | Go + Gin + GORM | [JH-Clipsync/ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin) |
| Admin frontend | Vue 3 + Element Plus | [JH-Clipsync/ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web) |
| Android client | Kotlin + OkHttp | [JH-Clipsync/ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android) |
| macOS client | Swift + SwiftUI | [JH-Clipsync/ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac) |
| Windows client | .NET 8 + WPF | [JH-Clipsync/ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows) |

---

## 📄 License

Personal project — feel free to study, fork, and modify.

---

**Made with ❤️ · Fully self-built across all platforms · Your data stays yours**
