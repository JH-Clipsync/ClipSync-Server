<p align="center">
  <img src="icon.png" width="128" alt="ClipSync 图标"/>
</p>

<h1 align="center">ClipSync-Server</h1>

<p align="center">
  <b>ClipSync 三端同步体系的中转服务端</b><br/>
  <a href="README.md">简体中文</a> ·
  <a href="README.en.md">English</a> ·
  <a href="README.ja.md">日本語</a>
</p>

---

ClipSync-Server 是 ClipSync 自建跨端消息同步体系的核心中转服务，使用 **Go + gorilla/websocket + MySQL + Redis** 开发。它负责把手机端收到的**短信验证码**、复制的**文本/图片**实时转发到 PC 端，反之亦然。

不依赖任何第三方推送服务，所有流量走你自己的 WebSocket 通道；端到端加密可选，隐私自主可控。

---

## ✨ 核心功能

| 模块 | 说明 |
|------|------|
| 🔄 **WebSocket 实时转发** | 按 `userID` 分组路由，支持 `notify_pc` / `notify_mobile` / `notify_all` / `clipboard` 四种投递语义；`pc` 与 `mobile` 角色互发 |
| 👥 **在线设备管理** | 内存 Hub 权威维护真实 WebSocket 连接，Redis Hash（`clipsync:online:<userID>`，TTL 90s）记录在线态，连接断开自动清理，进程被 kill 也不留幽灵记录 |
| 📡 **Presence 实时推送** | 设备上下线时主动向同组所有连接推送 `presence` 消息，客户端实时刷新在线设备 UI（平台/IP/能力/自定义名） |
| 👤 **用户系统** | 注册/登录、scrypt 密码哈希（N=32768, r=8, p=1）、JWT 风格 token、token TTL（默认 30 天）、单 IP 登录限流 |
| 📱 **设备表管理** | `devices` 表持久化登记每个账号下出现过的设备（角色/平台/自定义名/最近 IP），支持管理员**禁用设备**（禁用后握手即拒） |
| 👟 **强制下线** | 支持按用户踢全部设备、按设备踢单台；重置密码/封禁用户/删除用户时联动踢下线；5 种 kick reason（密码重置/用户封禁/用户删除/设备踢除/设备禁用） |
| 🛡️ **管理接口** | `GET /server-admin/users/{id}/devices`（在线状态以内存 Hub 为准）、设备启停/重命名、全量设备分页搜索、`POST /server-admin/kick` 统一动作入口；Bearer Token 鉴权（常量时间比较） |
| 📨 **Redis Pub/Sub 联动** | 与 ClipSync-Admin 共享 Redis 时通过频道 `clipsync:admin:kick_user` 下发控制指令，HTTP 接口做兜底，双保险 |
| 🧹 **短信 payload 清洗** | 自动剥离 `【+86xxx】` / `[N条]` 前缀、抽出 11 位发件人手机号塞入 `sender`、trim 空白，下游客户端无需再处理各种手机端注入 |
| 🔐 **端到端加密闸门** | `e2ee.require=true` 时拒绝转发任何明文消息，并关闭 `/push` 明文入口；密文服务端只做转发 |
| 📝 **按天切割日志** | 通用日志 `logs/clipsync.log` + 消息推送流水 `logs/message.log`，按天归档到 `logs/clipsync/`、`logs/message/`，可配置保留天数 |
| 🐳 **Docker 原生** | 多阶段构建 → distroless nonroot 镜像（~20MB），支持 host 网络直连宿主机 MySQL/Redis，volume 持久化配置与日志 |

---

## 🏗️ 技术栈

- **语言**：Go 1.23
- **WebSocket**：[gorilla/websocket](https://github.com/gorilla/websocket) v1.5
- **数据库**：MySQL 8（用户/会话/设备持久化，启动自动建表）
- **缓存**：Redis 7（token 缓存 + 在线设备登记 + Pub/Sub 管理通道）
- **密码哈希**：scrypt（`golang.org/x/crypto/scrypt`）
- **配置**：YAML + `go:embed` 内置默认值，支持环境变量覆盖
- **镜像**：`gcr.io/distroless/base-debian12:nonroot`（无 shell、非 root）

---

## 🚀 快速开始

### 方式 1：Docker Compose（推荐）

项目根目录的 `docker-compose.yml` 采用 host 网络，连接宿主机已有的 MySQL/Redis：

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

cp .env.example .env
# 编辑 .env，必填 BOOTSTRAP_PASSWORD，按需调整 TOKEN_TTL_HOURS / ALLOW_REGISTER

mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# 编辑 config/config.yaml，把 mysql.password / redis.password 改成真实值

docker compose up -d
docker compose logs -f clipsync
```

启动后：

```bash
curl http://127.0.0.1:28001/health   # 应返回 ok
```

默认监听 `:28001`；初始账号由 `.env` 的 `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` 指定。

### 方式 2：拉取官方镜像

```bash
docker run -d --name clipsync-server \
  --network host \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  -e TZ=Asia/Shanghai \
  ghcr.io/jh-clipsync/clipsync-server:latest
```

镜像地址：[ghcr.io/jh-clipsync/clipsync-server](https://github.com/orgs/JH-Clipsync/packages)

### 方式 3：源码编译

```bash
# 前置：Go 1.23+，本地可达的 MySQL 8 和 Redis
go build -ldflags "-X main.version=1.0.0" -o clipsync-server .
./clipsync-server --print-default-config > config.yaml
# 编辑 config.yaml 后启动
./clipsync-server --config config.yaml
```

---

## ⚙️ 配置详解

所有默认值都写在 [config.default.yaml](config.default.yaml) 里，通过 `go:embed` 编译进二进制，代码中**零硬编码**。

```bash
# 导出一份带完整注释的 YAML 作为起点
./clipsync-server --print-default-config > config.yaml
```

配置查找顺序：`--config 参数` → `CLIPSYNC_CONFIG` 环境变量 → `./config.yaml` → `/etc/clipsync/config.yaml`。文件不存在不报错，直接走内置默认值。

| 字段 | 说明 | 默认值 |
|---|---|---|
| `server.addr` | HTTP/WS 监听地址 | `:28001` |
| `server.read_timeout` / `write_timeout` | HTTP 读写超时 | `15s` |
| `server.shutdown_timeout` | 优雅退出等待 | `10s` |
| `server.trust_proxy` | 信任反代头（XFF/XRI） | `false` |
| `server.admin_token` | 管理端调用 `/server-admin/*` 的 Bearer Token；留空则接口一律 401 | 空 |
| `logs.dir` / `level` / `stdout` / `max_age_days` | 日志目录/级别/是否打到 stdout/归档保留天数 | `logs` / `info` / `true` / `0` |
| `websocket.read_limit` | 单条消息上限（剪贴板图片可能大） | `10485760`（10MB） |
| `websocket.ping_interval_sec` | 心跳间隔 | `30` |
| `websocket.send_queue_size` | 每客户端发送队列 | `32` |
| `message_protocol.max_payload_preview` | 日志中消息预览字符数 | `40` |
| `mysql.*` | MySQL 连接（DSN 自动拼装） | `127.0.0.1:3306/clipsync` |
| `redis.addr` / `db` / `key_prefix` / `online_ttl_sec` | Redis 地址/库/Key 前缀/在线 TTL | `127.0.0.1:6379` / `0` / `clipsync:` / `90` |
| `auth.token_ttl_hours` | token 有效期（小时） | `720`（30 天） |
| `auth.allow_register` | 是否开放 `POST /auth/register` | `false` |
| `auth.min_password_len` | 注册最小密码长度 | `8` |
| `auth.bootstrap_user/password` | 启动时自动创建的初始账号 | 空 |
| `auth.login_rate_limit_per_min` | 单 IP 每分钟登录尝试上限 | `10` |
| `e2ee.require` | 拒绝明文消息，关闭 `/push` | `false` |

仅以下少数字段支持环境变量覆盖：`CLIPSYNC_ADDR` / `CLIPSYNC_LOG_DIR` / `CLIPSYNC_LOG_LEVEL` / `CLIPSYNC_TRUST_PROXY` / `CLIPSYNC_WS_READ_LIMIT` / `CLIPSYNC_TOKEN_TTL_HOURS` / `CLIPSYNC_ALLOW_REGISTER` / `CLIPSYNC_BOOTSTRAP_USER` / `CLIPSYNC_BOOTSTRAP_PASSWORD` / `CLIPSYNC_E2EE_REQUIRE`。

> ⚠️ **MySQL/Redis 连接信息只认配置文件**，不提供环境变量覆盖，避免"改了 config.yaml 却被残留环境变量悄悄盖掉"。改完配置需重启进程，不支持热重载。

---

## 🔌 接口一览

### 客户端接口

| 路径 | 方法 | 说明 |
|---|---|---|
| `/ws?token=&device=&role=pc\|mobile&platform=&caps=&name=` | GET(WS) | 客户端长连接，握手时鉴权 + 设备准入 |
| `/auth/register` | POST | 注册（受 `allow_register` 开关控制） |
| `/auth/login` | POST | 用户名密码换 token |
| `/auth/session` | GET | 查询当前会话与在线设备 |
| `/auth/logout` | POST | 登出并清会话 |
| `/auth/change-password` | POST | 修改自己的密码 |
| `/device/name` | POST | 修改当前设备自定义名 |
| `/push?token=` | POST(JSON) | 便捷推送（curl 可测，`e2ee.require` 开启时禁用） |
| `/health` | GET | 健康检查 |

### 管理端接口（`/server-admin/*`，Bearer Token 鉴权）

| 路径 | 方法 | 说明 |
|---|---|---|
| `/server-admin/users` | POST | 管理端创建用户（绕过注册开关） |
| `/server-admin/users/{id}/devices` | GET | 列出用户全部设备（含在线状态，以内存 Hub 为准） |
| `/server-admin/users/{id}/devices/{deviceID}/status` | PUT | 启用/禁用设备（禁用即踢下线） |
| `/server-admin/users/{id}/devices/{deviceID}/name` | PUT | 重命名设备（广播 presence 实时刷新） |
| `/server-admin/devices` | GET | 跨用户分页搜索设备（keyword/disabled/user_id） |
| `/server-admin/kick` | POST | 统一踢下线入口（kick_user / kick_device / disable_device / enable_device） |

### 消息帧示例

```json
{ "type": "notify_pc", "kind": "sms_code", "text": "【某银行】验证码 314159" }
```

```bash
# 推送验证码给所有 PC 端
curl -X POST 'http://127.0.0.1:28001/push?token=<your-token>' \
  -H 'Content-Type: application/json' \
  -d '{"type":"notify_pc","kind":"sms_code","text":"【测试】验证码 314159，5分钟内有效"}'
```

---

## 🔐 在线状态模型

```
       ┌─────────────── 内存 Hub（权威）───────────────┐
       │  userID → set<*Client>  （真实 WebSocket 连接）  │
       └───────────────────┬───────────────────────────┘
                           │ 心跳每 30s 续期
                           ▼
       Redis Hash  clipsync:online:<userID>
                 field=deviceID  value=role
                 TTL = 90s（连接期间按 TTL/3 续期）
```

- **在线判定以内存 Hub 为准**：连接在就一定在，连接不在就不在。
- Redis Hash 主要用于登录时判断"当前用户是否已有客户端在线"，以及管理端跨进程展示。
- 进程被 `kill -9` 时，Redis 记录会在 90s 内自然过期，不留下幽灵在线设备。

---

## 🐳 部署架构

### 与 ClipSync-Admin 的协作

```
        ┌────────────────────┐        Redis Pub/Sub
        │  ClipSync-Admin    │ ───────────────────────▶  ClipSync-Server
        │  （管理后台 :28002）│   clipsync:admin:kick_user  （本服务 :28001）
        └─────────┬──────────┘ ◀───────────────────────  └────────┬─────────┘
                  │  HTTP 兜底（admin_token）                      │
                  └───────────────────────────────────────────────┘
                               共用 MySQL（clipsync 库）
```

- 两个服务共用同一个 MySQL 数据库 `clipsync`；
- Server 持有 `users` / `sessions` / `devices` 表的写权限；
- Admin 通过 Redis Pub/Sub 通知 Server 踢下线/禁用设备，Redis 不通时走 HTTP 兜底；
- 设备在线状态优先调 Server 的 `/server-admin/users/{id}/devices`，失败回退本地 MySQL+Redis。

### 反向代理

生产环境务必前置 Nginx/Caddy 加 TLS。参考 [Caddyfile.example](Caddyfile.example) 和 [deploy/nginx.clipsync.conf](deploy/nginx.clipsync.conf)（位于 Admin 仓库，但反代片段同样适用于 Server）。

关键：**WebSocket 长连接需要 `proxy_read_timeout` 拉长**（建议 3600s 以上），并开启 `server.trust_proxy: true` 才能拿到真实客户端 IP。

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

## 📁 项目结构

```
ClipSync-Server/
├── main.go                 # 路由 + WebSocket Hub + 消息路由 + 优雅退出
├── config.go               # 配置结构 + YAML 加载 + 环境变量覆盖
├── config.default.yaml     # 内置默认配置（go:embed，唯一默认值来源）
├── auth_http.go            # /auth/* 注册/登录/会话/改密 HTTP 接口
├── auth_service.go         # 认证业务：登录换 token、会话复用、限流
├── auth_crypto.go          # scrypt 密码哈希 + token SHA-256
├── admin_http.go           # /server-admin/* 管理端 HTTP 接口
├── device_name_http.go     # 普通用户修改自己设备名
├── store_mysql.go          # MySQL：users / sessions 表
├── store_device.go         # MySQL：devices 表（建档/禁用/重命名/分页搜索）
├── store_redis.go          # Redis：token 缓存 + 在线 Hash + Pub/Sub
├── e2ee.go                 # 端到端加密闸门 + 信封解析
├── logger.go               # 按天切割日志（通用 + 消息分类）
├── Dockerfile              # 多阶段构建 → distroless nonroot
├── docker-compose.yml      # host 网络部署编排
├── Caddyfile.example       # HTTPS 反代示例
├── deploy/
│   ├── config.compose.yaml   # 全 Docker 部署用配置起点
│   ├── config.external.yaml  # 宿主机已有 MySQL/Redis 时用
│   └── mysql/init.sql        # 数据库初始化脚本（服务端启动也会自建）
└── .github/workflows/docker-image.yml  # tag 推送自动构建多架构镜像
```

---

## 🔐 安全说明

| 维度 | 设计 |
|------|------|
| 密码存储 | scrypt（N=32768, r=8, p=1, 32 字节派生密钥，16 字节随机盐） |
| Token 存储 | MySQL 仅存 SHA-256 哈希；明文 token 仅带 TTL 暂存 Redis，拖库也无法复用 |
| 传输加密 | 建议前置 Nginx/Caddy 走 `wss://`；端到端加密由 AES-256-GCM 在客户端完成 |
| 登录防爆破 | 单 IP 每分钟登录尝试上限（默认 10），触发后拒绝 |
| 管理接口 | `admin_token` 使用 `crypto/subtle.ConstantTimeCompare` 常量时间比较，防时序攻击 |
| 客户端伪造 | 服务端强制覆盖消息 `from` 字段，`ping/pong/presence` 控制消息不转发 |
| 镜像安全 | distroless nonroot（uid 65532），无 shell、无包管理器，攻击面最小 |

---

## 🐛 故障排查

| 现象 | 排查 |
|------|------|
| 客户端连不上 | 防火墙放行 28001；反代是否正确转发 Upgrade 头；地址用 `ws://IP:28001/ws` |
| 日志里 IP 全是反代地址 | 开启 `server.trust_proxy: true` 并重启 |
| 管理接口返回 401 | `server.admin_token` 是否配置；客户端是否带 `Authorization: Bearer <token>` |
| 容器重启后日志没了 | 是否挂载 `./logs:/data/logs` |
| 改了配置不生效 | 配置启动时加载，需 `docker compose restart` / `systemctl restart` |
| Redis 里还有在线记录但设备已断 | 90s TTL 自然过期；内存 Hub 是权威，不影响实际转发 |
| `/push` 返回 403 | `e2ee.require=true` 时明文推送接口被禁用 |

日志位置：`logs/clipsync.log`（通用）、`logs/message.log`（消息推送流水），归档按天写入 `logs/clipsync/` 和 `logs/message/`。

---

## 🤝 相关项目

| 项目 | 技术栈 | 链接 |
|------|--------|------|
| 管理后台后端 | Go + Gin + GORM | [JH-Clipsync/ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin) |
| 管理后台前端 | Vue 3 + Element Plus | [JH-Clipsync/ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web) |
| Android 客户端 | Kotlin + OkHttp | [JH-Clipsync/ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android) |
| macOS 客户端 | Swift + SwiftUI | [JH-Clipsync/ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac) |
| Windows 客户端 | .NET 8 + WPF | [JH-Clipsync/ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows) |

---

## 📄 License

个人自用项目，代码可自由参考修改。

---

**Made with ❤️ · 三端全自研 · 隐私归你自己**
