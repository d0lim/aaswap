# claude-swap Go build.
#
# Toolchain versions are pinned in mise.toml; `mise install` provisions them.
# Every target here assumes go/golangci-lint resolve on PATH (mise shims or
# `mise exec --`).

BINARY := cswap
PKG    := ./cmd/cswap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/realiti4/claude-swap/internal/buildinfo.version=$(VERSION)

.PHONY: all
all: check build

.PHONY: build
build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

.PHONY: test
test:
	go test ./...

.PHONY: race
race:
	go test -race ./...

.PHONY: vet
vet:
	go vet ./...

# Go 1.27's `go fix` carries the modernizers (slices/maps, min/max, range-over-int,
# errors.AsType, wg.Go, strings.Cut*, ...). -diff turns it into a check: it prints
# the patch and exits non-zero when the tree is not already written in modern Go.
.PHONY: modernize
modernize:
	go fix -diff ./...

.PHONY: modernize-fix
modernize-fix:
	go fix ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: check
check: vet modernize lint test

.PHONY: clean
clean:
	rm -f $(BINARY)
	rm -rf dist/
