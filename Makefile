VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# CodeGraph release pinned for the bundled MCP server / e2e test. Bump together
# with any change to the integration in internal/codegraph.
CODEGRAPH_VERSION := v0.9.7

.PHONY: build build-web release-web vet fmt test hardware-verify hardware-device-verify windows-package-verify hooks cross clean e2e-codegraph

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/reasonix ./cmd/reasonix
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/reasonix-plugin-example ./cmd/reasonix-plugin-example
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/onecreat-hardware-mcp ./cmd/reasonix-hardware-mcp

# Web 模式:单二进制起本地 HTTP 服务 + 浏览器当 UI(见 docs/Web模式.md)。
# 前端 bundle 内嵌,所以先 pnpm build 再 go build -tags web。纯 Go,无 CGO/WebKit。
build-web:
	cd desktop/frontend && pnpm install --frozen-lockfile && pnpm build
	cd desktop && CGO_ENABLED=0 go build -tags web -ldflags "$(LDFLAGS)" -o ../bin/onecreat-web .

# 主分发形态:一台机器交叉编译全平台 Web 发行包 -> dist/onecreat-web-<os>-<arch>.{tar.gz,zip} + SHA256SUMS
# (含 onecreat-hardware-mcp + README;注入 defaultAccountMode=platform)。单平台:scripts/web-build.sh darwin/arm64 vX.Y.Z
release-web:
	scripts/web-build.sh all $(VERSION)

vet:
	go vet ./...

fmt:
	gofmt -w .

test:
	go test ./...

hardware-verify:
	scripts/hardware-verify.sh

hardware-device-verify:
	scripts/hardware-device-verify.sh $(ARGS)

windows-package-verify:
	scripts/windows-package-verify.sh

hooks:
	@git config core.hooksPath .githooks
	@echo "installed: core.hooksPath -> .githooks (pre-push runs go vet)"

cross:
	@mkdir -p dist
	@for p in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "build $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/reasonix-$$os-$$arch$$ext ./cmd/reasonix; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/onecreat-hardware-mcp-$$os-$$arch$$ext ./cmd/reasonix-hardware-mcp; \
	done

clean:
	rm -rf bin dist

# Fetch the matching CodeGraph bundle into bin/codegraph/ (the distribution
# layout: launcher at bin/codegraph/bin/codegraph beside bin/reasonix) and run the
# gated MCP end-to-end test against it. Requires `gh`. Windows: install via the
# upstream install.ps1 and run the test with REASONIX_CODEGRAPH_BIN set.
e2e-codegraph:
	@os=$$(uname -s | tr 'A-Z' 'a-z'); arch=$$(uname -m); \
	case $$arch in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=x64;; *) echo "unsupported arch $$arch"; exit 1;; esac; \
	asset=codegraph-$$os-$$arch.tar.gz; dest=bin/codegraph; \
	echo "fetching $$asset ($(CODEGRAPH_VERSION)) -> $$dest"; \
	rm -rf $$dest && mkdir -p $$dest; \
	gh release download $(CODEGRAPH_VERSION) -R colbymchenry/codegraph -p $$asset -O /tmp/$$asset; \
	tar -xzf /tmp/$$asset -C $$dest --strip-components=1; \
	REASONIX_CODEGRAPH_E2E=1 REASONIX_CODEGRAPH_BIN=$$PWD/$$dest/bin/codegraph \
		go test ./internal/codegraph/ -run E2E -v -count=1
