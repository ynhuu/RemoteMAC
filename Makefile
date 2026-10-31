.DEFAULT_GOAL := build

GO ?= go
PNPM ?= pnpm
OUT_DIR ?= bin
VERSION ?= dev
GO_BUILD_FLAGS ?= -trimpath
GO_LDFLAGS ?= -s -w

RELEASE_ZIPS := remotemac-darwin-arm64.zip remotemac-darwin-amd64.zip

.PHONY: help deps frontend check build build-all build-arm64 build-amd64 \
	build-x86_64 build-darwin-arm64 build-darwin-amd64 release \
	release-arm64 release-amd64

help:
	@printf '%s\n' \
	  'make                 构建 macOS Intel 和 Apple Silicon 双架构（默认目标）' \
	  'make build-arm64     构建 macOS arm64（Apple Silicon）' \
	  'make build-amd64     构建 macOS amd64（Intel）' \
	  'make build-x86_64    build-amd64 的兼容别名' \
	  'make build-all       构建 macOS 双架构' \
	  'make release         构建双架构 ZIP 资产及 SHA256SUMS' \
	  'make deps            安装 Go 和前端依赖' \
	  'make frontend        构建并同步前端嵌入资源' \
	  'make check           构建前端、检查安装脚本、运行 Go 测试和 vet' \
	  '可覆盖变量：GO、PNPM、OUT_DIR、VERSION、GO_BUILD_FLAGS、GO_LDFLAGS'

deps:
	$(GO) mod download
	$(PNPM) --dir web install --frozen-lockfile

# Shared prerequisite: even make -j build/release runs the frontend build once.
frontend:
	@test "$$(uname -s)" = Darwin || { echo "macOS CGO builds require macOS and Xcode Command Line Tools."; exit 1; }
	@xcrun --find clang >/dev/null
	$(PNPM) --dir web build

$(OUT_DIR):
	@mkdir -p "$@"

build: build-all

build-all: build-amd64 build-arm64

build-x86_64: build-amd64

build-darwin-amd64: build-amd64

build-darwin-arm64: build-arm64

build-amd64: frontend | $(OUT_DIR)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 CC=clang $(GO) build $(GO_BUILD_FLAGS) -ldflags="$(GO_LDFLAGS) -X main.version=$(VERSION)" -o "$(OUT_DIR)/remotemac-darwin-amd64" ./cmd/remotemac

build-arm64: frontend | $(OUT_DIR)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC=clang $(GO) build $(GO_BUILD_FLAGS) -ldflags="$(GO_LDFLAGS) -X main.version=$(VERSION)" -o "$(OUT_DIR)/remotemac-darwin-arm64" ./cmd/remotemac

release: release-arm64 release-amd64
	@cd "$(OUT_DIR)" && { \
	  if command -v shasum >/dev/null 2>&1; then shasum -a 256 $(RELEASE_ZIPS); \
	  elif command -v sha256sum >/dev/null 2>&1; then sha256sum $(RELEASE_ZIPS); \
	  else printf '%s\n' '需要 shasum 或 sha256sum' >&2; exit 1; fi; \
	} > SHA256SUMS

release-arm64: build-arm64
	@rm -f "$(OUT_DIR)/remotemac-darwin-arm64.zip"
	zip -q -j "$(OUT_DIR)/remotemac-darwin-arm64.zip" "$(OUT_DIR)/remotemac-darwin-arm64"

release-amd64: build-amd64
	@rm -f "$(OUT_DIR)/remotemac-darwin-amd64.zip"
	zip -q -j "$(OUT_DIR)/remotemac-darwin-amd64.zip" "$(OUT_DIR)/remotemac-darwin-amd64"

check: frontend
	sh -n install.sh
	$(GO) test ./...
	$(GO) vet ./...
