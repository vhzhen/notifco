.DEFAULT_GOAL := help

BINARY      := notifco
BIN_DIR     := bin
PKG         := ./...
TEST_DB_URL ?= postgres://notifco:notifco@localhost:5432/notifco_test?sslmode=disable
TEST_REDIS  ?= redis://localhost:6379/15

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY) ./cmd/notifco
	@echo "built $(BIN_DIR)/$(BINARY)"

.PHONY: dist-linux
dist-linux: ## Cross-compile a static linux/amd64 binary for prebuilt deploys
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags="-s -w" -o dist/notifco-linux-amd64 ./cmd/notifco
	@ls -lh dist/notifco-linux-amd64 | awk '{print "built dist/notifco-linux-amd64 (" $$5 ")"}'

.PHONY: run
run: ## Run in all-in-one mode (reads .env)
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/notifco all

.PHONY: poll
poll: ## One-shot poll of watched courses, then exit
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/notifco poll

.PHONY: migrate
migrate: ## Apply database migrations
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/notifco migrate

.PHONY: test
test: ## Run unit + integration tests with the race detector
	TEST_DATABASE_URL="$(TEST_DB_URL)" TEST_REDIS_URL="$(TEST_REDIS)" \
		go test -race -count=1 $(PKG)

.PHONY: test-short
test-short: ## Run only tests that need no database
	go test -race -count=1 -short ./internal/source/... ./internal/config/... ./internal/notifier/... ./internal/poller/...

.PHONY: load-test
load-test: ## Run the 10k-subscription load test
	TEST_DATABASE_URL="$(TEST_DB_URL)" TEST_REDIS_URL="$(TEST_REDIS)" \
	LOAD_RESULTS_FILE=load-results.txt \
		go test -tags=load -count=1 -v -timeout=600s ./test/load/

.PHONY: cover
cover: ## Generate a coverage report
	TEST_DATABASE_URL="$(TEST_DB_URL)" TEST_REDIS_URL="$(TEST_REDIS)" \
		go test -count=1 -coverprofile=coverage.out -coverpkg=./internal/... $(PKG)
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if code is not gofmt-clean
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi
	@echo "gofmt clean"

.PHONY: lint
lint: fmt-check vet ## Format check + vet
	@echo "lint OK"

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: verify
verify: lint build test ## Everything CI runs
	@echo "verify OK"

.PHONY: up
up: ## Start Postgres + Redis + notifco via docker-compose
	docker compose up -d --build

.PHONY: down
down: ## Stop the docker-compose stack
	docker compose down

.PHONY: logs
logs: ## Tail docker-compose logs
	docker compose logs -f notifco

.PHONY: dev-infra
dev-infra: ## Start only Postgres + Redis (run the bot with `make run`)
	docker compose up -d postgres redis

.PHONY: clean
clean: ## Remove build and test artefacts
	rm -rf $(BIN_DIR) dist coverage.out load-results.txt
