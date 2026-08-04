# ===== 阶段 1: 构建 =====
FROM golang:1.23-alpine AS builder

# 国内网络环境可解开下面两行加速（任选一个镜像源）
# RUN go env -w GOPROXY=https://goproxy.cn,direct
# RUN go env -w GOSUMDB=sum.golang.google.cn

WORKDIR /src

# 先复制依赖文件，最大化缓存命中
COPY go.mod go.sum ./
RUN go mod download

# 复制源码并编译
COPY . .
# 静态链接（不需要 glibc），适合 scratch / distroless 镜像
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.version=$(date -u +%Y%m%d)" \
    -o /out/clipsync-server .

# ===== 阶段 2: 运行 =====
# 用 distroless 基础镜像（含 ca-certificates、tzdata、wget、/etc/passwd）；
# 没有 shell / 没有包管理器，攻击面最小。
FROM gcr.io/distroless/base-debian12:nonroot

# 时区数据：让日志里 time.Local 走 UTC（distroless 默认 UTC）
# 如需改成北京时间：再装一份 tzdata 即可
ENV TZ=UTC

# 用非 root 用户（distroless nonroot tag 自带 uid 65532）
USER nonroot:nonroot

WORKDIR /app
# 仅拷贝编译产物和示例配置（配置本身应通过 volume 挂载进来）
COPY --from=builder /out/clipsync-server /app/clipsync-server
# 默认配置已通过 go:embed 编译进二进制，容器内可用
#   clipsync-server --print-default-config
# 导出一份完整带注释的 YAML 作为 config.yaml 起点。
COPY --from=builder /src/config.default.yaml /app/config.default.yaml

# 暴露端口（与 config.yaml 默认值保持一致；docker run -p 可覆盖）
EXPOSE 8080

# 数据 / 日志 / 配置目录（容器里用 volume 挂到这里就能持久化）
VOLUME ["/data/logs", "/data/store"]

# 启动时默认从 /data/config/config.yaml 加载；找不到就用代码默认值
# 启动时把 logs.dir 也指向 /data/logs，方便宿主机直接看
ENV CLIPSYNC_LOG_DIR=/data/logs
ENV CLIPSYNC_CONFIG=/data/config/config.yaml

ENTRYPOINT ["/app/clipsync-server"]
# 默认参数：加载 /data/config/config.yaml（找不到时无害，直接走默认）
CMD ["--config", "/data/config/config.yaml"]
