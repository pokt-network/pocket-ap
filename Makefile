.PHONY: build run tidy test lint clean release-snapshot dist docker

BIN := bin/pocket-ap
CONFIG_PATH ?= local/config.yaml

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Same flags as dist and the Dockerfile, deliberately. This tree links cosmos-sdk,
# cometbft and go-ethereum, so an unstripped binary is ~153 MB against ~102 MB
# stripped — and a 50 MB gap between what you test locally and what ships is the
# kind of surprise that surfaces at release time. -s -w drops the symbol table and
# DWARF, so `dlv` cannot debug this binary; panic stack traces are unaffected,
# since Go builds those from the pclntab, which stripping keeps. For a debugger,
# build without ldflags: go build -o bin/pocket-ap ./cmd/pocket-ap
build: ## Build the binary (CGO disabled, static, stripped + version-stamped)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/pocket-ap

dist: ## Cross-compile stripped release binaries for every target
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" \
			-o dist/pocket-ap-$$os-$$arch$$ext ./cmd/pocket-ap || exit 1; \
	done
	@ls -lh dist/

release-snapshot: ## Dry-run the full release locally (no tag, no publish, no remote needed)
	goreleaser release --snapshot --clean

docker: ## Build the container image
	docker build -t pocket-ap:$(VERSION) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

run: ## Run from source: make run CONFIG_PATH=path/to/config.yaml
	go run ./cmd/pocket-ap -config $(CONFIG_PATH)

tidy: ## Sync go.mod/go.sum
	go mod tidy

test: ## Unit tests
	go test ./... -count=1 -race

lint: ## Lint (requires golangci-lint)
	golangci-lint run --timeout 5m

clean:
	rm -rf bin dist
