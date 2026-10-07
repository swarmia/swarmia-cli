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

# npm platform packages: <dir>:<GOOS>:<GOARCH>. The <dir> uses Node's
# platform-arch naming (process.platform-process.arch), which the launcher in
# npm/cli/bin/swarmia-cli maps to the @swarmia/cli-<dir> package.
# Windows is not shipped yet. It compiles (cmd/lock_windows.go uses LockFileEx)
# and CI cross-builds it, but it is untested.
NPM_PLATFORMS := \
	darwin-arm64:darwin:arm64 \
	darwin-x64:darwin:amd64 \
	linux-arm64:linux:arm64 \
	linux-x64:linux:amd64

.PHONY: all build install test vet fmt tidy clean npm-binaries e2e

all: build ## Build the binary (default)

build: ## Compile the binary into ./$(BINARY)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: ## Install the binary into $(go env GOPATH)/bin
	go install -ldflags "$(LDFLAGS)" .

test: ## Run the test suite
	go test ./...

vet: ## Run go vet
	go vet ./...
	go vet -tags e2e ./e2e/

e2e: ## Run the end-to-end suite with a real coding agent (see docs/e2e.md)
	go test -tags e2e -count=1 -v -timeout 45m ./e2e/

fmt: ## Format all Go source in place
	gofmt -l -w .

tidy: ## Tidy go.mod / go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -f $(BINARY)
	rm -rf npm/platforms/*/bin

npm-binaries: ## Cross-compile per-platform binaries into the npm packages
	@for entry in $(NPM_PLATFORMS); do \
		dir=$${entry%%:*}; rest=$${entry#*:}; os=$${rest%%:*}; arch=$${rest##*:}; \
		out=npm/platforms/$$dir/bin/$(BINARY); \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "  $$dir ($$os/$$arch) -> $$out"; \
		mkdir -p npm/platforms/$$dir/bin; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done
