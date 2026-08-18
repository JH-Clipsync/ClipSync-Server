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

不依赖任何第三方推送服务，所有流量走你自己的 WebSocket 通道；端到端加密可选，隐私自主可控。默认监听 **28001** 端口。

---

## ✨ 核心功能

| 模块 | 说明 |
|------|------|
| 🔄 **WebSocket 实时转发** | 按 `userID` 分组路由，支持 `notify_pc` / `notify_mobile` / `notify_all` / `clipboard` 四种投递语义；`pc` 与 `mobile` 角色互发，永不回环 |
| 👥 **在线设备管理** | 内存 Hub 权威维护真实 WebSocket 连接；Redis Hash（`clipsync:online:<userID>`，TTL 90s）记录在线态，30s 心跳续期，连接断开自动清理，进程被 kill 也不留幽灵记录 |
| 📡 **Presence 实时推送** | 设备上下线时主动向同组所有连接推送 `presence` 消息，客户端实时刷新在线设备 UI（平台 / IP / 能力位 / 自定义名称） |
| 👤 **用户系统** | 注册 / 登录、scrypt 密码哈希（N=32768, r=8, p=1）、随机 token（32 字节，仅存 SHA-256 哈希）、token TTL（默认 720 小时 / 30 天）、单 IP 登录限流 |
| 📱 **设备表管理** | `devices` 表持久化登记每个账号下出现过的设备（角色 / 平台 / 自定义名 / 最近 IP），首次握手自动建档；支持管理员**禁用设备**，禁用后握手即拒 |
| 👟 **强制下线** | 支持按用户踢全部设备、按设备踢单台；重置密码 / 封禁用户 / 删除用户时联动踢下线；5 种 kick reason（密码重置 / 用户封禁 / 用户删除 / 设备踢除 / 设备禁用） |
| 🛡️ **管理接口** | `GET /server-admin/users/{id}/devices`（在线状态以内存 Hub 为准 + Redis 补充）、设备启停 / 重命名、全量设备分页搜索、`POST /server-admin/kick` 统一动作入口；Bearer Token 常量时间比较鉴权 |
| 📨 **Redis Pub/Sub 联动** | 与 ClipSync-Admin 共享 Redis 时通过频道 `clipsync:admin:kick_user` 下发控制指令，HTTP 接口做兜底，双保险 |
| 🧹 **短信 payload 清洗** | 自动剥离 `【+86xxx】` / `[N条]` 前缀、抽出 11 位发件人手机号塞入 `sender`、trim 空白，下游客户端无需再处理手机端注入 |
| 🔐 **端到端加密闸门** | `e2ee.require=true` 时拒绝转发任何明文消息，并关闭 `/push` 明文入口；密文服务端只做转发，看不到内容 |
| 📝 **按天切割日志** | 通用日志 `logs/clipsync.log` + 消息推送流水 `logs/message.log`，按天归档到 `logs/clipsync/`、`logs/message/` 子目录，可配置保留天数 |
| 🐳 **Docker 原生** | 多阶段构建 → distroless nonroot 镜像（无 shell、非 root、攻击面最小），支持 host 网络直连宿主机 MySQL/Redis，volume 持久化配置与日志 |

---

## 🚀 快速开始

### 方式一：Docker Compose（推荐）

仓库自带的 `docker-compose.yml` 使用 **host 网络**直接连接宿主机已有的 MySQL / Redis，不会另起容器：

```bash
# 1. 准备配置
mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# 编辑 config/config.yaml，填入 mysql.password / redis.password / admin_token

# 2. 准备 .env（决定初始账号密码等）
cp .env.example .env
vim .env   # 至少修改 BOOTSTRAP_PASSWORD

# 3. 启动
docker compose up -d
docker compose logs -f clipsync
```

启动成功后：

- 服务监听 `:28001`（host 网络直接绑定宿主机端口）
- 初始账号由 `.env` 中 `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` 指定
- 配置文件挂载在 `./config/config.yaml`，日志写入 `./logs/`

> 若希望用 Compose 一次性拉起 MySQL + Redis + Server，可参考 `deploy/config.compose.yaml` 自行扩展服务定义。

### 方式二：Docker 一行命令

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

### 方式三：二进制 + systemd

在 [Releases](https://github.com/JH-Clipsync/ClipSync-Server/releases) 下载对应平台的 tar 包（`linux-amd64` / `linux-arm64` / `darwin-arm64`），解压后执行：

```bash
sudo ./install.sh
```

脚本会：

1. 安装到 `/opt/clipsync-server/`
2. 注册并启动 `clipsync-server.service`（systemd）
3. 设置开机自启、失败自动重启
4. 日志输出到 `/opt/clipsync-server/logs/clipsync.log`

常用命令：

```bash
sudo systemctl status clipsync-server
sudo systemctl restart clipsync-server
tail -f /opt/clipsync-server/logs/clipsync.log
```

### 方式四：源码编译

需要 Go 1.23+：

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

# 直接运行
go run .

# 或编译静态二进制
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=1.0.0" -o clipsync-server .
./clipsync-server --config config.yaml
```

查看版本 / 导出完整默认配置：

```bash
./clipsync-server --version
./clipsync-server --print-default-config > config.yaml
```

---

## ⚙️ 配置详解

配置通过 `go:embed` 把 [config.default.yaml](config.default.yaml) 编译进二进制，作为唯一的默认值来源。配置优先级：

```
内置默认值  <  config.yaml  <  环境变量
```

配置文件查找顺序：

1. `--config` 命令行参数
2. `CLIPSYNC_CONFIG` 环境变量
3. `./config.yaml`
4. `/etc/clipsync/config.yaml`

文件不存在不报错，直接用默认值。完整字段清单可运行 `clipsync-server --print-default-config` 查看。

### 主要配置段

| 配置段 | 关键项 | 说明 |
|--------|--------|------|
| `server` | `addr: ":28001"` | 监听地址 |
| | `trust_proxy: false` | 反代场景置 `true`，才会从 `X-Forwarded-For` 取真实 IP |
| | `admin_token: ""` | `/server-admin/*` 接口的 Bearer Token；留空则管理接口全部返回 503。建议用 `openssl rand -hex 32` 生成 |
| `logs` | `dir` / `level` / `stdout` / `max_age_days` | 日志目录、级别（debug/info/warn/error）、是否同时输出到 stdout、归档保留天数（0=永久） |
| `websocket` | `read_limit: 10MB` | 单条消息上限，剪贴板图片可能很大 |
| | `ping_interval_sec: 30` | 服务端 Ping 间隔 |
| | `read_deadline_sec: 60` | 读超时，决定死连接被清理的窗口 |
| | `send_queue_size: 32` | 每客户端发送队列长度 |
| `mysql` | `host/port/user/password/database` | 用户 / 会话 / 设备持久化；启动自动建表 |
| | `max_open_conns / max_idle_conns` | 连接池调优 |
| `redis` | `addr / password / db` | token 缓存 + 在线设备登记 + Pub/Sub |
| | `key_prefix: "clipsync:"` | 所有 key 统一前缀 |
| | `online_ttl_sec: 90` | 在线登记 TTL；连接期间按 TTL/3（约 30s）心跳续期 |
| `auth` | `token_ttl_hours: 720` | token 有效期（30 天） |
| | `allow_register: false` | 是否开放 `POST /auth/register`；默认关闭，账号由管理员创建 |
| | `min_password_len: 8` | 最小密码长度 |
| | `bootstrap_user / bootstrap_password` | 启动时自动创建的初始账号（已存在则跳过） |
| | `login_rate_limit_per_min: 10` | 单 IP 每分钟登录尝试上限，防暴力破解（0=不限） |
| `e2ee` | `require: false` | 为 `true` 时拒绝转发任何明文消息，并关闭 `/push` 明文接口 |
| `message_protocol` | `check_origin: true` | 是否允许任意 Origin 的 WebSocket 握手（生产建议改白名单逻辑） |
| | `max_payload_preview: 40` | 日志里 payload 预览截断字符数 |

### 环境变量覆盖

少量关键运行参数支持环境变量覆盖：

| 环境变量 | 对应字段 |
|----------|----------|
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

> MySQL / Redis 连接信息有意不提供环境变量覆盖，改配置文件 + 重启即可，避免"改了配置却被残留环境变量悄悄盖掉"。

---

## 📡 接口说明

### 客户端接口（走用户 token 鉴权）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/ws` | WebSocket 升级端点，query 参数 `token` / `device` / `role`（pc/mobile，兼容旧值 phone） / `platform` / `caps` / `name` |
| POST | `/auth/register` | 注册（受 `auth.allow_register` 开关控制） |
| POST | `/auth/login` | 登录，返回 token；已有客户端在线时复用同一 token，返回 `reused=true` |
| POST | `/auth/logout` | 作废当前 token |
| GET | `/auth/session` | 查询当前会话状态与在线设备 |
| POST | `/auth/change-password` | 修改密码，旧 token 立即失效并踢全部设备，返回新 token |
| POST | `/push` | 便捷 HTTP 推送入口（curl 调试用，`e2ee.require=true` 时禁用） |
| POST | `/device/name` | 用户重命名自己的设备（实时广播 presence） |
| GET | `/health` | 健康检查，返回 `ok` |

### 管理接口（走 `admin_token` 鉴权）

所有 `/server-admin/*` 接口需要请求头：

```
Authorization: Bearer <server.admin_token>
```

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/server-admin/users` | 管理端创建用户（不受 `allow_register` 限制） |
| GET | `/server-admin/users/{id}/devices` | 列出某用户全部设备，**在线状态以内存 Hub 为准，Redis 补充** |
| GET | `/server-admin/devices` | 跨用户分页搜索设备，支持 `keyword` / `disabled` / `user_id` / `page` / `page_size` |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/status` | 启用 / 禁用设备，禁用后立即踢下线 |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/name` | 重命名设备，并向在线连接广播 presence |
| POST | `/server-admin/kick` | 统一动作入口，body 支持 `kick_user` / `kick_device` / `disable_device` / `enable_device` |

### WebSocket 消息类型

| `type` | 投递范围 | 典型场景 |
|--------|----------|----------|
| `notify_pc` | 同组所有 `pc` 角色 | 短信验证码同步到电脑 |
| `notify_mobile` | 同组所有 `mobile` 角色 | 从电脑推通知到手机 |
| `notify_all` | 同组所有设备（除自己） | 通用广播 |
| `clipboard` | 同组所有设备（除自己） | 剪贴板文本 / 图片，接收方按开关决定是否自动写入 |
| `presence` | 仅服务端下发 | 在线设备列表变更通知 |
| `server_kick` | 仅服务端下发 | 被强制下线，带 `reason` 字段 |

### Redis Pub/Sub 频道

- 频道名：`{redis.key_prefix}admin:kick_user`（默认 `clipsync:admin:kick_user`）
- 消息体为 JSON：`{"action":"kick_user|kick_device|disable_device|enable_device","user_id":1,"device_id":"...","reason":"..."}`
- 兼容纯数字 `userID`（等价于 `kick_user`）
- Admin 是发布方，Server 订阅并自动重连（指数退避，最长 30s）

---

## 🏗️ 项目架构

```
┌──────────────┐  WebSocket   ┌──────────────────────────────┐
│  PC/Mobile   │ ──────────▶  │      ClipSync-Server         │
│   Clients    │ ◀──────────  │  ┌────────────────────────┐  │
└──────────────┘   presence   │  │  Hub（内存）           │  │
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

### 数据模型

- **users**：账号，密码只存 scrypt 哈希，`disabled` 控制封禁
- **sessions**：一个用户最多一条活跃会话（`user_id` 主键），所有设备共享同一 token；token 只存 SHA-256 哈希
- **devices**：`(user_id, device_id)` 复合主键，记录角色 / 平台 / 自定义名 / 最近 IP / 禁用状态 / 最后在线时间

### 代码结构

| 文件 | 职责 |
|------|------|
| [main.go](main.go) | Hub / Client / WebSocket 路由 / presence 广播 / 心跳 / HTTP 路由入口 / 优雅退出 |
| [config.go](config.go) / [config.default.yaml](config.default.yaml) | 配置结构、加载顺序、环境变量覆盖 |
| [auth_service.go](auth_service.go) / [auth_http.go](auth_http.go) / [auth_crypto.go](auth_crypto.go) | 登录 / 注册 / 会话 / scrypt + token 哈希 / 限流 |
| [store_mysql.go](store_mysql.go) | 用户 / 会话持久化与自动建表 |
| [store_device.go](store_device.go) | 设备表 CRUD、分页搜索、禁用状态 |
| [store_redis.go](store_redis.go) | token 缓存、在线登记、Pub/Sub 管理通道 |
| [admin_http.go](admin_http.go) | `/server-admin/*` 接口实现 |
| [e2ee.go](e2ee.go) | 端到端加密策略闸门 |
| [logger.go](logger.go) | 按天切割 + 归档保留 + 通用 / 消息双日志 |
| [device_name_http.go](device_name_http.go) | 用户自助重命名设备 |

---

## 🔐 安全说明

- **密码存储**：scrypt（N=32768, r=8, p=1, 32 字节派生密钥，16 字节随机盐），格式 `scrypt$N$r$p$salt$dk`，参数随哈希存储，未来可平滑升级
- **Token 存储**：32 字节随机 token，MySQL / Redis 里只落 SHA-256 哈希；明文 token 仅在 Redis 带 TTL 暂存，用于"同账号多设备复用同一会话"
- **登录限流**：单 IP 滑动窗口，默认 10 次 / 分钟；用户名或密码错误返回同一错误，避免用户名枚举
- **管理接口鉴权**：`admin_token` 留空即 503；对比走 `subtle.ConstantTimeCompare`，防时序攻击
- **反代信任**：`trust_proxy=false` 时忽略 `X-Forwarded-For`，避免被伪造头欺骗
- **WebSocket Origin**：生产环境建议把 `message_protocol.check_origin` 改为 `false` 并按白名单校验
- **端到端加密**：三端都升级到支持加密的版本后，把 `e2ee.require` 改为 `true`，服务端将拒绝任何明文消息
- **容器安全**：distroless nonroot 镜像，无 shell、无包管理器、uid 65532 非 root 运行
- **传输安全**：生产环境务必在前面加 Nginx / Caddy 终止 TLS（见下文示例）

### Nginx 反代示例

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

记得在 `config.yaml` 里把 `server.trust_proxy` 置为 `true`。

### Caddy 反代示例

仓库提供了 [Caddyfile.example](Caddyfile.example)：

```
clipsync.example.com {
    reverse_proxy 127.0.0.1:28001
}
```

Caddy 会自动申请并续期 Let's Encrypt 证书。

---

## 🐛 故障排查

| 现象 | 排查方向 |
|------|----------|
| 客户端握手返回 401 | token 无效或已过期；检查 Redis 里 `clipsync:token:*` 是否存在；服务端时间是否准确 |
| 客户端握手返回 403「设备已被管理员禁用」 | `devices.disabled=1`，到管理后台解禁设备 |
| 客户端连接后立即断开 | 检查 `websocket.read_deadline_sec` 是否过小；反代是否吞掉了 Ping/Pong 帧；Nginx 是否设置了 `proxy_read_timeout` |
| 在线状态不准 | 内存 Hub 是权威源；Redis 在线 TTL 默认 90s，每 30s 续期。若 Redis 被清空，重连后会自动恢复；进程异常退出后等 90s key 自然过期 |
| 管理接口返回 503 | `server.admin_token` 未配置，配置后重启 |
| 管理接口返回 401 | 请求头 `Authorization: Bearer <token>` 与服务端 `admin_token` 不一致 |
| 登录提示「尝试过于频繁」 | 触发了单 IP 限流，等待 1 分钟或调大 `auth.login_rate_limit_per_min` |
| 反代后日志里 IP 全是 127.0.0.1 | 把 `server.trust_proxy` 改成 `true`，并确保反代设置了 `X-Forwarded-For` |
| 剪贴板图片收不到 | 检查 `websocket.read_limit`（默认 10MB）和反代的 `client_max_body_size` |
| Docker 容器连不上 MySQL / Redis | host 网络下应填 `127.0.0.1:<宿主机端口>`；确认 MySQL 允许来自 `127.0.0.1` 的连接、端口未被防火墙拦截 |
| 日志文件没生成 | 检查 `logs.dir` 目录权限；distroless nonroot 镜像里 uid 65532 需要日志目录可写，compose 里已 `user: "0:0"` 兜底 |

### 日志位置

- 通用日志：`logs/clipsync.log`（当天）+ `logs/clipsync/clipsync-YYYY-MM-DD.log`（归档）
- 消息推送流水：`logs/message.log`（当天）+ `logs/message/message-YYYY-MM-DD.log`（归档）

消息日志独立成文件，记录每条消息的收 / 发 / 丢弃 / 业务分类（短信 / 剪贴板 / 通知）/ 内容格式（文本 / 图片）/ 推送范围，便于审计与排障。

---

## 🤝 相关项目

- [ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin)：管理后台后端（Go + Gin + GORM）
- [ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web)：管理后台前端（Vue 3 + Element Plus）
- [ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows)：Windows 客户端
- [ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac)：macOS 客户端
- [ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android)：Android 客户端
