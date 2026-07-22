BIN     := pigeon
PKG     := ./cmd/pigeon
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test race fmt vet check clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

install:
	go install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race -count=1 ./...

check: fmt vet race

clean:
	rm -f $(BIN)
	rm -rf dist
