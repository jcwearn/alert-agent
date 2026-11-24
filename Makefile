GOLANGCI_VERSION := $(shell cat .golangci-lint-version)
IMAGE ?= ghcr.io/jcwearn/alert-agent

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -o bin/alert-agent ./cmd/alert-agent

.PHONY: run
run: ## Run the controller locally on :8080
	go run ./cmd/alert-agent controller

.PHONY: test
test: ## Run the tests with the race detector
	go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint at the pinned version
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION) run

.PHONY: image
image: ## Build the container image locally
	docker build -t $(IMAGE):dev .
