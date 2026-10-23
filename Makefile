.PHONY: build lint test

GOLANGCI_LINT_VERSION ?= v1.64.8

# Version stamped into the binary; container images built from it are
# tagged with the same string.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)

build:
	go build ./...
	go build -ldflags "-X main.version=$(VERSION)" -o coreshift ./cmd/coreshift

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		go run github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run; \
	fi

test:
	go test ./...
