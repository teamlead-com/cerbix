GO ?= go
BINARY ?= bin/cerbix
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
PKG := github.com/teamlead-com/cerbix
LDFLAGS := -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)

.PHONY: build test race lint version clean

build:
	@mkdir -p bin $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cerbix

test:
	@mkdir -p $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

race:
	@mkdir -p $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test -race ./...

lint:
	golangci-lint run ./...

version: build
	$(BINARY) version

clean:
	rm -f $(BINARY)
