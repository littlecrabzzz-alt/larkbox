BINARY_NAME := larkbox
BUILD_DIR   := bin

.PHONY: help fmt vet lint test test-live test-blackbox build ui-install ui-test ui-build clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

fmt: ## Format Go source files
	gofmt -s -w .

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

test: ## Run unit and official SDK integration tests (offline, race detector on)
	go test -race -v ./...

test-live: ## Run differential tests locally or with FEISHU_LIVE=1 against a Feishu test tenant
	go test -tags=live -count=1 -v ./tests/live_test/...

test-blackbox: build ## Run the standalone CLI and webhook recipe (Python 3)
	python3 examples/blackbox.py --binary $(BUILD_DIR)/$(BINARY_NAME)

build: ## Build a pure-Go single binary (CGO_ENABLED=0)
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/larkbox

ui-install: ## Install frontend dependencies from the lockfile
	pnpm --dir web install --frozen-lockfile

ui-test: ## Type-check and test the Web Inspector
	pnpm --dir web run typecheck
	pnpm --dir web run test

ui-build: ## Regenerate the committed embedded Web Inspector
	pnpm --dir web run build

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR) coverage.txt coverage.html
