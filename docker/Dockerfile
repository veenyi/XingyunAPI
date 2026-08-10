# 行云API Docker 镜像（多阶段构建：Go 编译 → alpine 精简运行）
# 构建：docker build -t xingyun-api:latest -f docker/Dockerfile .
# 运行：docker compose -f docker/docker-compose.yml up -d
# 面板：http://<主机IP>:34891

FROM golang:1.25-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o /out/xingyun-api ./cmd/JoyCode2Api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/xingyun-api /app/xingyun-api
# HOME 决定数据库路径（/data/.joycode-proxy/），卷挂载到宿主机实现持久化
ENV HOME=/data
VOLUME ["/data"]
EXPOSE 34891
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:34891/api/health || exit 1
ENTRYPOINT ["sh", "-c", "exec /app/xingyun-api serve --skip-validation --tls=false --port ${PORT:-34891}"]
