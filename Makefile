#!make
MIN_MAKE_VERSION := 3.81

ifneq ($(MIN_MAKE_VERSION),$(firstword $(sort $(MAKE_VERSION) $(MIN_MAKE_VERSION))))
$(error GNU Make $(MIN_MAKE_VERSION) or higher required)
endif

SHELL := /bin/bash

.DEFAULT_GOAL := help

BINARY := edookit-mcp

# Pinned tool versions — kept in sync with go.mod `tool` directives via `make update-local-dep`.
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.3.0

##@ Build

.PHONY: build run install clean tidy

build: format vet ## Build the binary into bin/
	go build -o bin/$(BINARY) .

run: ## Run the MCP server locally with .env loaded (expects MCP framing on stdin)
	@if [ ! -f .env ]; then echo "missing .env — copy .env.example and fill in credentials"; exit 1; fi
	@set -a; . ./.env; set +a; go run .

smoke-login: ## Perform the OIDC login once and exit, logging the cookie count and verifying the dashboard probe (set EDOOKIT_HEADLESS_LOGIN=false to watch)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; go run . -login-test

dump-html: ## Dump the rendered landing page HTML to stdout (for selector debugging)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; go run . -dump-html > /tmp/edookit-landing.html
	@echo "wrote /tmp/edookit-landing.html ($$(wc -c < /tmp/edookit-landing.html) bytes)"

clear-cookies: ## Delete the cached session cookies (forces re-login on next run)
	@go run . -clear-cookies

test-messages: ## Fetch and print a few inbox + sent messages (smoke test for the MCP tools)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; go run . -test-messages

smoke-message: ## (dev) Dump the raw JSON of full-message endpoints for one message — usage: make smoke-message MSG=m-NNNNNN
	@if [ -z "$(MSG)" ]; then echo "usage: make smoke-message MSG=m-NNNNNN (try 'make find-message-with-attachments' to pick one)"; exit 1; fi
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; go run . -dump-message "$(MSG)"

find-message-with-attachments: ## (dev) Scan the inbox and print IDs of messages that have at least one attachment
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; go run . -test-messages 2>&1 | awk '/attachments=[1-9]/ {print}'

install: ## Install the binary into $GOBIN (or $GOPATH/bin)
	go install .

clean: ## Remove build artifacts and coverage output
	rm -rf bin/
	rm -f coverage.txt coverage.html

tidy: ## Run go mod tidy
	go mod tidy

##@ Testing & Linting

.PHONY: check pre-push format vet lint lint-fix test test-cover govulncheck

check: format vet lint-fix test ## Run format, vet, lint-fix and tests (mutates files)

pre-push: lint test govulncheck build ## Strict checks before pushing (no mutation)

format: ## Run formatters configured in .golangci.yml (gofumpt + goimports)
	go tool golangci-lint fmt

vet: ## Run go vet
	go vet ./...

lint: ## Run linters locally (requires `make tools` once after clone)
	go tool golangci-lint run -v

lint-fix: ## Run linters with auto-fix
	go tool golangci-lint run --fix -v

test: ## Run tests with race detector and coverage
	go test -race -count=1 -cover -coverprofile=coverage.txt ./...

test-cover: test ## Generate HTML coverage report from latest test run
	go tool cover -html=coverage.txt -o coverage.html
	@echo "Coverage report: coverage.html"

govulncheck: ## Run Go vulnerability checker
	go tool govulncheck ./...

##@ Tools

.PHONY: tools update-local-dep golangci-lint-version

tools: update-local-dep ## Install tool dependencies into go.mod (run once after clone)

update-local-dep: ## Update local Go tool dependencies to versions pinned in this Makefile
	@echo "Updating golangci-lint@$(GOLANGCI_LINT_VERSION)" && \
		go get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@echo "Updating govulncheck@$(GOVULNCHECK_VERSION)" && \
		go get -tool golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@go mod tidy

golangci-lint-version: ## Print pinned golangci-lint version
	@echo $(GOLANGCI_LINT_VERSION)

##@ Help

.PHONY: help

help: ## Display this help screen
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
