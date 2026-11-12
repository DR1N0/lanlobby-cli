# Build and test lanlobby-cli.
GREEN := \033[32m
BLUE := \033[34m
YELLOW := \033[33m
NC := \033[0m
.DEFAULT_GOAL := help

CLITARGET := lanlobby
GO ?= go
LDFLAGS ?= -s -w

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "$(GREEN)Available targets:$(NC)\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  $(BLUE)%-20s$(NC) %s\n", $$1, $$2 } \
		/^##@/ { printf "\n$(YELLOW)%s$(NC)\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: clean
clean: ## Clean up build artifacts
	rm -rf bin/

.PHONY: vendor
vendor: ## Update Go module dependencies
	$(GO) mod tidy
	$(GO) mod vendor

	
##@ Build and Test

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: build
build: ## Build darwin/arm64, linux/amd64, and windows/amd64 binaries
	mkdir -p bin/darwin_arm bin/linux bin/windows
	@printf 'Building binary for darwin/arm64\n'
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/darwin_arm/$(CLITARGET) ./cmd/lanlobby
	@printf 'Building binary for linux/amd64\n'
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/linux/$(CLITARGET) ./cmd/lanlobby
	@printf 'Building binary for windows/amd64\n'
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/windows/$(CLITARGET).exe ./cmd/lanlobby
