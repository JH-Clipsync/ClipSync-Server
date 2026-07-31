<p align="center">
  <img src="icon.png" width="120" alt="ClipSync"/>
</p>

# ClipSync-Server

Go + WebSocket 中转服务，负责把手机推送的验证码/剪贴板内容转发给 PC 端。

## 快速开始

### 方式 1：本地直接跑

```bash
go run .
# 默认监听 :8080
```

### 方式 2：Docker Compose（推荐）

```bash
# 1. 准备配置
cp config.example.yaml config/config.yaml
# 按需修改 config/config.yaml

# 2. 启动
docker compose up -d

# 3. 看日志
docker compose logs -f

# 4. 重启（改完配置后）
docker compose restart
```

启动后访问 http://localhost:8080/health 应返回 `ok`。

### 方式 3：纯 Docker

```bash
docker build -t clipsync-server .
docker run -d --name clipsync-server -p 8080:8080 \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  clipsync-server
```

## 配置文件

支持 YAML 配置文件，**优先级：环境变量 > 配置文件 > 代码默认值**。

查找顺序（启动时）：
1. 命令行 `--config /path/to/config.yaml`
2. 环境变量 `CLIPSYNC_CONFIG`
3. 当前目录 `config.yaml`
4. `/etc/clipsync/config.yaml`

找不到时不报错，直接走代码默认值。

完整示例见 [config.example.yaml](config.example.yaml)，可覆盖的字段：

| 字段 | 说明 | 默认值 |
|---|---|---|
| `server.addr` | 监听地址 | `:8080` |
| `server.read_timeout` | HTTP 读超时 | `15s` |
| `server.write_timeout` | HTTP 写超时 | `15s` |
| `server.shutdown_timeout` | 优雅退出等待时间 | `10s` |
| `server.trust_proxy` | 是否信任 X-Forwarded-For 等头 | `false` |
| `logs.dir` | 日志根目录 | `logs` |
| `logs.level` | 日志级别 | `info` |
| `logs.stdout` | 是否同时输出到 stdout | `true` |
| `logs.max_age_days` | 归档保留天数（0=不限） | `0` |
| `websocket.read_limit` | 单条消息最大字节 | `10485760` (10MB) |
| `websocket.read_deadline_sec` | 读超时 | `60` |
| `websocket.write_deadline_sec` | 写超时 | `10` |
| `websocket.ping_interval_sec` | 心跳间隔 | `30` |
| `websocket.send_queue_size` | 每客户端发送队列长度 | `32` |
| `message_protocol.check_origin` | 是否允许任意 Origin 跨域 | `true` |
| `message_protocol.max_payload_preview` | payload 预览最大字符数 | `40` |

### 常用环境变量覆盖

不想改配置文件时，临时用环境变量覆盖：

```bash
CLIPSYNC_ADDR=":9000" \
CLIPSYNC_TRUST_PROXY=true \
CLIPSYNC_LOG_DIR=/var/log/clipsync \
./clipsync-server
```

Docker 场景下直接在 `docker-compose.yml` 的 `environment:` 块加。

### 配置热更新

⚠️ 当前 **不支持**配置热重载；改完配置后需要 `docker compose restart` 让服务重新加载。

## 接口

| 路径 | 方法 | 说明 |
|---|---|---|
| `/ws?token=xxx&device=yyy&role=phone\|pc` | GET (WebSocket) | 客户端连接 |
| `/health` | GET | 健康检查（返回 `ok`） |
| `/push?token=xxx` | POST (JSON) | 便捷推送接口（详见下） |

### `/push` 示例

```bash
# 推送短信验证码给 PC 端
curl -X POST 'http://localhost:8080/push?token=test123' \
  -H 'Content-Type: application/json' \
  -d '{"type":"notify_pc","kind":"sms_code","text":"【测试】您的验证码是 314159，5分钟内有效"}'

# 同步剪贴板文本给所有端
curl -X POST 'http://localhost:8080/push?token=test123' \
  -H 'Content-Type: application/json' \
  -d '{"type":"clipboard","kind":"text","text":"你好世界"}'
```

## 消息协议

见根目录 README。同 token 下：
- `phone` 发的消息广播给所有 `pc`
- `pc` 发的消息广播给所有 `phone`（比如设置项）

## 部署到生产

### 加上 HTTPS（WSS）

拷贝 [Caddyfile.example](Caddyfile.example) 为 `Caddyfile`，改好域名，然后取消 `docker-compose.yml` 里 `caddy` 服务的注释，`docker compose up -d` 即可。Caddy 会自动申请 Let's Encrypt 证书。

### 反代注意

反代（Nginx / Caddy / Traefik）在前面时，**务必在 config.yaml 里把 `server.trust_proxy: true`**，否则日志里的客户端 IP 全是反代 IP。

Nginx 反代配置示例：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 86400s;  # WebSocket 长连接
}
```

## 从源码编译

```bash
# 当前平台
go build -o clipsync-server .

# 注入版本号
go build -ldflags "-X main.version=1.0.0" -o clipsync-server .

# Linux 交叉编译（生产部署常用）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o clipsync-server .
```

## 项目结构

```
ClipSync-Server/
├── main.go              # HTTP 路由 + WebSocket 消息处理
├── config.go            # YAML 配置文件加载 + 环境变量覆盖
├── logger.go            # 按天切割的日志 Writer
├── Dockerfile           # 多阶段构建，最终镜像 ~20MB
├── docker-compose.yml   # 一键启动 + 反代
├── config.example.yaml  # 配置示例
└── Caddyfile.example    # HTTPS 反代示例
```
