BINARY := swarmia-cli
PKG    := github.com/swarmia/swarmia-cli

# Version metadata, injected into the binary at build time. Overridable, e.g.
# `make build VERSION=1.2.3`. Defaults derive from git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/cmd.version=$(VERSION) \
	-X $(PKG)/cmd.commit=$(COMMIT) \
	-X $(PKG)/cmd.date=$(DATE)

.PHONY: all build install test vet fmt tidy clean

all: build ## Build the binary (default)

build: ## Compile the binary into ./$(BINARY)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: ## Install the binary into $(go env GOPATH)/bin
	go install -ldflags "$(LDFLAGS)" .

test: ## Run the test suite
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go source in place
	gofmt -l -w .

tidy: ## Tidy go.mod / go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -f $(BINARY)
