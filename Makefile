.DEFAULT_GOAL := help
BIN := bin/bot

.PHONY: help build run test lint fmt tidy docker docker-run

help: ## Show this help
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/bot

run: ## Run the bot (reads env vars; see .env.example)
	go run ./cmd/bot

test: ## Run tests with race detector
	go test -race -cover ./...

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format code
	golangci-lint fmt

tidy: ## Tidy go.mod
	go mod tidy

docker: ## Build the Docker image
	docker build -t anydloadbot .

docker-run: ## Run the image, passing env vars from .env
	docker run --rm --init --env-file .env --name anydloadbot anydloadbot
