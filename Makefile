.PHONY: build build-all install test lint clean release-build release-verify

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.Version=$(VERSION) \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/YuLaiZ/token-usage/internal/buildinfo.BuildTime=$(BUILD_TIME)

# tag 合同正则（与 internal/update/version.go 的 ParseVersion 接受集逐字等价）：
# vMAJOR.MINOR.PATCH[-rc.N]，各数值段无前导零，rc.N 中 N>=1。
TAG_REGEX := ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$$

# 跨平台 SHA256 工具：优先 GNU coreutils 的 sha256sum（Linux CI），回退 shasum（macOS）。
# 两者默认（文本）模式输出均为 "<小写 hex>  <文件名>"，且在 Unix 上文本模式不转换字节，
# 故 hash 与 crypto/sha256 一致。
SHA256_CMD := $(shell command -v sha256sum 2>/dev/null)
ifeq ($(SHA256_CMD),)
SHA256_CMD := shasum -a 256
endif

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

# 正式发布构建：产出一个可重复、可校验的 dist/ 发布物。
# 要求非空且符合 tag 合同的 VERSION（禁止 dev），以及非空 COMMIT、BUILD_TIME。
release-build:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "release-build 要求非空且非 dev 的 VERSION（当前 VERSION='$(VERSION)'）" >&2; exit 1; \
	fi
	@if ! printf '%s' "$(VERSION)" | grep -Eq '$(TAG_REGEX)'; then \
		echo "release-build: VERSION '$(VERSION)' 不符合 tag 合同（须 vMAJOR.MINOR.PATCH[-rc.N]，无前导零）" >&2; exit 1; \
	fi
	@if [ -z "$(COMMIT)" ]; then echo "release-build 要求非空 COMMIT" >&2; exit 1; fi
	@if [ -z "$(BUILD_TIME)" ]; then echo "release-build 要求非空 BUILD_TIME" >&2; exit 1; fi
	@echo "release-build: VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME)"
	@rm -rf dist
	@mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-darwin-arm64 ./cmd/token-usage
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-darwin-amd64 ./cmd/token-usage
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/token-usage-windows-amd64.exe ./cmd/token-usage
	@# 生成严格排序的 SHA256SUMS（资产名 ASCII 升序：darwin-amd64 < darwin-arm64 < windows-amd64.exe）。
	@# 两个空格分隔、小写 hex、LF 结尾——与 internal/update/manifest.go 的 ParseManifest 合同逐字一致。
	@cd dist && $(SHA256_CMD) token-usage-darwin-amd64 token-usage-darwin-arm64 token-usage-windows-amd64.exe > SHA256SUMS
	@echo "release-build: 已生成 dist/SHA256SUMS"

# 校验 release-build 产出的 dist/：经 updater 的真实代码路径（ParseManifest/AssetName）
# 验证清单格式、hash 一致性，并在本机为受支持平台时验证注入的 VERSION。
release-verify:
	@if [ -z "$(VERSION)" ]; then echo "release-verify 要求 VERSION（用于本机 --version 校验）" >&2; exit 1; fi
	go run ./cmd/release-verify -dist dist -version "$(VERSION)"
