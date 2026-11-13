GO      ?= go
BIN     := bin/fishplate
PKG     := github.com/StevenGann/fishplate/internal/version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG).Version=$(VERSION) \
	-X $(PKG).Commit=$(COMMIT) \
	-X $(PKG).Date=$(DATE)

.PHONY: build test vet run check clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/fishplate

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

run:
	$(GO) run -ldflags '$(LDFLAGS)' ./cmd/fishplate serve

check: vet test build

clean:
	rm -rf bin
