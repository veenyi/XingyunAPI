# 构建时可通过 --build-arg ALPINE_MIRROR=<base-url> 切换 Alpine 包源，
# 解决部分网络环境下无法连接 dl-cdn.alpinelinux.org 导致 apk 失败的问题。
# 国内示例：--build-arg ALPINE_MIRROR=https://mirrors.aliyun.com/alpine
ARG ALPINE_MIRROR=https://dl-cdn.alpinelinux.org/alpine

FROM golang:1.25-alpine AS builder

ARG ALPINE_MIRROR
RUN sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${ALPINE_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags "-s -w" -o /JoyCode2Api ./cmd/JoyCode2Api

FROM alpine:3.19

ARG ALPINE_MIRROR
RUN sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${ALPINE_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates

COPY --from=builder /JoyCode2Api /usr/local/bin/JoyCode2Api

EXPOSE 34891

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:34891/health || exit 1

ENTRYPOINT ["JoyCode2Api"]
CMD ["serve"]
