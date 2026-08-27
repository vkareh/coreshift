.PHONY: build lint test

GOLANGCI_LINT_VERSION ?= v1.64.8

build:
	go build ./...
	go build -o coreshift ./cmd/coreshift

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		go run github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run; \
	fi

test:
	go test ./...
