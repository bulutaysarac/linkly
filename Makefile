BINARY := linkly
IMAGE  := linkly:local
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: help build run test race vet fmt cover docker docker-run compose smoke clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/$(BINARY)

run: build ## Build and run locally on :8080
	./bin/$(BINARY)

test: ## Run the test suite
	go test ./...

race: ## Run tests with the race detector
	go test -race ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt -w
	gofmt -w .

cover: ## Test coverage summary
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: docker ## Build and run the container on :8080
	docker run --rm -p 8080:8080 --name linkly $(IMAGE)

compose: ## Bring the stack up with docker compose
	docker compose up --build

smoke: ## Run the end-to-end smoke test against a running instance
	./scripts/smoke.sh

clean:
	rm -rf bin coverage.out
