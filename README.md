<p align="center">
  <img src="icon.png" width="120" alt="ClipSync"/>
</p>

# ClipSync-Server

Go + WebSocket 中转服务，负责把手机推送的验证码/剪贴板内容转发给 PC 端。

## 运行

```bash
cd ClipSync-Server
go mod tidy
go run .
```

默认监听 `:8080`。

## 接口

- `GET /ws?token=xxx&device=yyy&role=phone|pc` — WebSocket 连接
- `GET /health` — 健康检查

## 消息协议

见根目录 README。同 token 下：
- `phone` 发的消息广播给所有 `pc`
- `pc` 发的消息广播给所有 `phone`（比如设置项）

## 部署

```bash
GOOS=linux GOARCH=amd64 go build -o clipsync-server
./clipsync-server
```

配合 nginx 反代加 wss:// 上 TLS。
