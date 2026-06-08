.PHONY: build build-all install test lint clean

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.Version=$(VERSION) \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.BuildTime=$(BUILD_TIME)

# 编译当前平台
build:
	go build -ldflags "$(LDFLAGS)" -o token-usage ./cmd/token-usage

# 交叉编译
build-all:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-darwin-arm64 ./cmd/token-usage
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-darwin-amd64 ./cmd/token-usage
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-windows-amd64.exe ./cmd/token-usage

# 安装到本地
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/token-usage

# 运行测试
test:
	go test ./...

# 代码检查
lint:
	golangci-lint run

# 清理构建产物
clean:
	rm -rf dist/
	rm -f token-usage
