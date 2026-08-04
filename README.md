<p align="center">
  <img src="icon.png" width="120" alt="ClipSync"/>
</p>

# ClipSync-Server

<p align="center">
  <b>ClipSync 三端同步体系的中转服务端</b><br/>
  Go + WebSocket 实现，负责把手机端的短信验证码 / 剪贴板内容实时转发给 PC 端。<br/>
  单二进制、零依赖、配置驱动、Docker 原生支持。
</p>

<p align="center">
  <a href="https://github.com/gitwangjiahui/ClipSync-Server/releases">⬇️ 下载 Release</a> ·
  <a href="https://github.com/orgs/gitwangjiahui/packages">📦 Packages (ghcr.io)</a> ·
  <a href="https://github.com/gitwangjiahui/ClipSync-Android">📱 Android 端</a> ·
  <a href="https://github.com/gitwangjiahui/ClipSync-Mac">🖥️ Mac 端</a>
</p>

---

## 一、它是什么 / 解决什么问题

你在手机上收到的**短信验证码**、复制的**文本/图片**，经常需要在电脑上使用。ClipSync 的思路是：

```
┌──────────┐   WebSocket    ┌────────────────┐   WebSocket    ┌──────────┐
│ Android  │ ─────────────▶ │ ClipSync-Server │ ◀───────────── │ Mac (PC) │
│ 手机     │ ◀───────────── │   (本项目)       │ ─────────────▶ │ 电脑     │
└──────────┘                └────────────────┘                └──────────┘
        同一 token 的设备自动配对；手机发的内容广播给所有 PC，反之亦然。
```

服务端本身**不存任何业务数据**（不落库），只做实时路由，天然隐私友好。

## 二、核心特性

- **按 token 分组路由**：同 token 下 `role=phone` 的消息广播给所有 `role=pc`，反之亦然，互不串扰
- **消息类型**：`notify_pc` / `notify_mobile` / `notify_all` / `clipboard` 四种投递语义
- **短信清洗**：识别 `【】` 发件人、剥离 `+86` 前缀、提取 4-8 位验证码
- **YAML 配置 + 环境变量覆盖**：优先级 `环境变量 > 配置文件 > 默认值`
- **Docker 原生**：多阶段构建、distroless 非 root 运行、healthcheck、volume 持久化
- **优雅退出**：SIGINT/SIGTERM → `srv.Shutdown` 等待在途连接
- **日志体系**：按天切割（`logs/xxx-YYYY-MM-DD.log`）+ 通用日志与消息推送日志分文件
- **反代友好**：`trust_proxy` 开关，支持 X-Forwarded-For / X-Real-IP 取真实 IP

## 三、快速开始

### 方式 1：Docker（最推荐）

```bash
# 直接拉官方镜像（ghcr.io Packages）
docker run -d --name clipsync-server -p 8080:8080 \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  ghcr.io/gitwangjiahui/clipsync-server:latest
```

或用 compose。编排里 `name: clipsync` 固定了分组，`mysql` / `redis` / `clipsync`
会挂在同一组下面。其中 `clipsync` 属于 `server` profile，**默认不启动**：

```bash
git clone https://github.com/gitwangjiahui/ClipSync-Server.git
cd ClipSync-Server
cp .env.example .env                        # 改掉里面所有密码

# 只起依赖（mysql + redis），server 自己在宿主机跑 —— 开发常用
docker compose up -d
docker compose ps                           # 等 mysql 变成 healthy
export CLIPSYNC_MYSQL_PASSWORD=<.env 里的 MYSQL_PASSWORD>
export CLIPSYNC_BOOTSTRAP_USER=admin
export CLIPSYNC_BOOTSTRAP_PASSWORD=<初始密码，至少 8 位>
go run .

# 或者三件套一起交给 Docker —— 部署常用
mkdir -p config logs && cp config.example.yaml config/config.yaml
docker compose --profile server up -d --build
docker compose logs -f clipsync
```

启动后 `curl http://localhost:8080/health` 应返回 `ok`。

MySQL / Redis 默认映射到宿主机 `3306` / `6379`，方便容器外的 server 直连；
端口被占用就在 `.env` 里改 `MYSQL_HOST_PORT` / `REDIS_HOST_PORT`。

### 方式 2：下载二进制（免 Docker）

到 [Releases](https://github.com/gitwangjiahui/ClipSync-Server/releases) 下载对应平台包：

| 包 | 适用 |
|---|---|
| `clipsync-server-linux-amd64.tar.gz` | 常见 x86_64 云服务器 |
| `clipsync-server-linux-arm64.tar.gz` | ARM 服务器 / 树莓派 |
| `clipsync-server-darwin-arm64.tar.gz` | macOS Apple 芯片 |

```bash
tar xzf clipsync-server-linux-amd64.tar.gz
cd clipsync-server-linux-amd64
sudo ./install.sh        # 一键安装为 systemd 服务并启动
# 改配置后：sudo systemctl restart clipsync-server
```

### 方式 3：源码编译

```bash
go build -o clipsync-server .
# 注入版本号：
go build -ldflags "-X main.version=1.1.0" -o clipsync-server .
# 生产交叉编译：
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o clipsync-server .
```

## 四、配置详解

配置查找顺序：`--config 参数` → `CLIPSYNC_CONFIG` 环境变量 → `./config.yaml` → `/etc/clipsync/config.yaml`。
找不到文件不报错，直接走默认值。完整示例见 [config.example.yaml](config.example.yaml)。

| 字段 | 说明 | 默认值 |
|---|---|---|
| `server.addr` | 监听地址 | `:8080` |
| `server.read_timeout` / `write_timeout` | HTTP 读/写超时 | `15s` |
| `server.shutdown_timeout` | 优雅退出等待 | `10s` |
| `server.trust_proxy` | 信任反代头（取真实 IP） | `false` |
| `logs.dir` | 日志根目录 | `logs` |
| `logs.level` | 日志级别 | `info` |
| `logs.stdout` | 同时输出 stdout（容器建议 true） | `true` |
| `logs.max_age_days` | 归档保留天数（0=不限） | `0` |
| `websocket.read_limit` | 单条消息上限（图片大） | `10485760` (10MB) |
| `websocket.read_deadline_sec` | 读超时 | `60` |
| `websocket.write_deadline_sec` | 写超时 | `10` |
| `websocket.ping_interval_sec` | 心跳间隔 | `30` |
| `websocket.send_queue_size` | 每客户端发送队列 | `32` |
| `message_protocol.check_origin` | 允许任意 Origin 跨域 | `true` |
| `message_protocol.max_payload_preview` | 日志预览字符数 | `40` |

环境变量覆盖（不想改文件时用）：`CLIPSYNC_ADDR` / `CLIPSYNC_LOG_DIR` / `CLIPSYNC_LOG_LEVEL` / `CLIPSYNC_TRUST_PROXY` / `CLIPSYNC_WS_READ_LIMIT`。

⚠️ 改完配置需 `docker compose restart` / `systemctl restart`，当前不支持热重载。

## 五、接口与协议

| 路径 | 方法 | 说明 |
|---|---|---|
| `/ws?token=xxx&device=yyy&role=phone\|pc` | GET(WS) | 客户端长连接 |
| `/health` | GET | 健康检查，返回 `ok` |
| `/push?token=xxx` | POST(JSON) | 便捷推送（curl 可测） |

消息帧（JSON）：

```json
{ "type": "notify_pc", "kind": "sms_code", "text": "【某银行】验证码 314159" }
```

```bash
# 推送验证码给所有 PC 端
curl -X POST 'http://localhost:8080/push?token=test123' \
  -H 'Content-Type: application/json' \
  -d '{"type":"notify_pc","kind":"sms_code","text":"【测试】验证码 314159"}'

# 同步剪贴板文本给所有端
curl -X POST 'http://localhost:8080/push?token=test123' \
  -H 'Content-Type: application/json' \
  -d '{"type":"clipboard","kind":"text","text":"你好世界"}'
```

## 六、生产部署（HTTPS / 反代）

1. 拷贝 [Caddyfile.example](Caddyfile.example) 为 `Caddyfile`，改好域名；
2. 取消 `docker-compose.yml` 里 `caddy` 服务注释，`docker compose up -d`，Caddy 自动申请证书；
3. 反代在前面时**务必** `server.trust_proxy: true`，否则日志 IP 全是反代地址。

Nginx 反代片段（注意 WebSocket 长连接超时）：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 86400s;
}
```

## 七、项目结构

```
├── main.go              # HTTP 路由 + WebSocket 消息处理 + 优雅退出
├── config.go            # YAML 配置加载 + 环境变量覆盖 + 默认值
├── logger.go            # 按天切割日志 Writer（通用 + 消息分类）
├── Dockerfile           # 多阶段构建 → distroless nonroot（~20MB）
├── docker-compose.yml   # 一键启动 + 可选 Caddy 反代
├── config.example.yaml  # 完整配置示例
├── Caddyfile.example    # HTTPS 反代示例
└── .github/workflows/docker.yml  # 打 tag 自动推多架构镜像到 ghcr.io
```

## 八、安全说明

- token 由客户端自带，服务端只用作分组键；不要把 token 写进公开文档
- `message_protocol.check_origin` 生产建议 `false`（走白名单）
- 镜像以非 root（uid 65532）运行，攻击面最小

## 九、常见问题

| 问题 | 排查 |
|---|---|
| 客户端连不上 | 防火墙放行 8080；地址用 `ws://服务器IP:8080` |
| 日志 IP 全是 127.0.0.1 | 开 `trust_proxy` 并重启 |
| 容器重启后日志没了 | 挂 volume：`-v ./logs:/data/logs` |
| 改了配置不生效 | 配置是启动时加载，需 restart |
