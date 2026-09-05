BINARY  := wb2api
VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-amd64 build-arm64 build-all docker docker-multi test tidy

## 本地原生构建（当前所在平台）
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/server

## 交叉编译 amd64 二进制
build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY)-amd64 ./cmd/server

## 交叉编译 arm64 二进制（树莓派 / Mac M 系 / ARM 服务器 / 飞牛等）
build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY)-arm64 ./cmd/server

## 同时产出 amd64 + arm64 二进制
build-all: build-amd64 build-arm64

## 本地 docker 构建（当前架构）
docker:
	docker build -t $(BINARY):local .

## 多平台 docker 构建（需已启用 buildx + QEMU）
docker-multi:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(BINARY):local --load .

## 运行测试
test:
	go test ./... -count=1

## 整理依赖
tidy:
	go mod tidy
