BIN     := pigeon
PKG     := ./cmd/pigeon
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Every platform the code is expected to compile for. Release artifacts are a
# subset (see .goreleaser.yaml): plugin monitors only exist on Unix, so Windows
# is checked but not shipped.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 \
             windows/amd64 freebsd/amd64 openbsd/amd64

.PHONY: help build install test race cover fmt vet check cross snapshot clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into ./pigeon
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

install: ## Install pigeon into GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

test: ## Run tests
	go test ./...

race: ## Run tests with the race detector
	go test -race -count=1 ./...

cover: ## Report coverage per function
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

fmt: ## Format
	gofmt -w .

vet: ## Vet
	go vet ./...

cross: ## Verify every supported platform compiles
	@set -e; for t in $(PLATFORMS); do \
		printf '  %-16s' "$$t"; \
		GOOS=$${t%/*} GOARCH=$${t#*/} go build ./... && echo ok; \
	done

check: fmt vet race cross ## Everything CI runs

snapshot: ## Build a local release snapshot (requires goreleaser)
	goreleaser release --snapshot --clean

clean: ## Remove build artifacts
	rm -f $(BIN) coverage.out
	rm -rf dist
