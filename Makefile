#!make
SHELL := /bin/bash

.DEFAULT_GOAL := help

BINARY := edookit-mcp
PDFIUM_DIR := third_party/pdfium

##@ Build

.PHONY: build run install clean

build: pdfium ## Build the binary into target/ (fetches the bundled PDFium first)
	cargo build

release: pdfium ## Build an optimized release binary
	cargo build --release

run: pdfium ## Run the MCP server locally with .env loaded (expects MCP framing on stdin)
	@if [ ! -f .env ]; then echo "missing .env — copy .env.example and fill in credentials"; exit 1; fi
	@set -a; . ./.env; set +a; cargo run --quiet

install: pdfium ## Install the binary into ~/.cargo/bin
	cargo install --path .

clean: ## Remove build artifacts (keeps the fetched PDFium)
	cargo clean

distclean: clean ## Also remove the vendored PDFium
	rm -rf $(PDFIUM_DIR)

##@ PDFium (bundled native lib for attachment rasterization)

.PHONY: pdfium

pdfium: ## Download the prebuilt PDFium library for this host (idempotent)
	@./scripts/fetch-pdfium.sh

##@ Smoke / dev (require .env; functional once the MCP wiring lands)

.PHONY: smoke-login test-messages clear-cookies dump-html

smoke-login: pdfium ## Perform the OIDC login once and exit (set EDOOKIT_HEADLESS_LOGIN=false to watch)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; cargo run --quiet -- --login-test

test-messages: pdfium ## Fetch and print a few inbox + sent messages (smoke for the tools)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; cargo run --quiet -- --test-messages

clear-cookies: ## Delete the cached session cookies (forces re-login on next run)
	@cargo run --quiet -- --clear-cookies

dump-html: ## Dump the rendered landing page HTML to /tmp (selector debugging)
	@if [ ! -f .env ]; then echo "missing .env"; exit 1; fi
	@set -a; . ./.env; set +a; cargo run --quiet -- --dump-html > /tmp/edookit-landing.html
	@echo "wrote /tmp/edookit-landing.html ($$(wc -c < /tmp/edookit-landing.html) bytes)"

##@ Testing & Linting

.PHONY: check pre-push fmt fmt-check lint lint-fix test

check: fmt lint-fix test ## Format, lint-fix and test (mutates files)

pre-push: fmt-check lint test build ## Strict gate before pushing (no mutation)

fmt: ## Format the code (rustfmt)
	cargo fmt

fmt-check: ## Check formatting without modifying
	cargo fmt --check

lint: ## Run clippy with warnings denied
	cargo clippy --all-targets -- -D warnings

lint-fix: ## Run clippy with auto-fix
	cargo clippy --all-targets --fix --allow-dirty --allow-staged

test: pdfium ## Run the test suite
	cargo test

##@ Help

.PHONY: help

help: ## Display this help screen
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
