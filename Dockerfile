# syntax=docker/dockerfile:1

# 构建阶段固定在构建机原生平台（BUILDPLATFORM）运行，通过 GOARCH 交叉编译目标平台二进制，
# 避免为每个架构拉取/模拟对应平台的 Go 工具链 —— 更快、且不依赖 QEMU。
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETARCH
ARG TARGETOS=linux
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 静态链接；GOOS/GOARCH 跟随 buildx 注入的目标平台（linux/amd64、linux/arm64…）
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/wb2api /app/wb2api
# 默认配置占位：运行时请通过挂载（见 docker-compose.yml）覆盖为真实 config.json
COPY config.example.json /app/config.json
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
