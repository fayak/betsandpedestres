BIN ?= betsandpedestres
CMD ?= ./cmd/betsandpedestres
BINCLI ?= bap
CMDCLI ?= ./cmd/bap
PKG ?= ./...
GOLANGCI_LINT ?= golangci-lint
TEST_EXTRA_ARG ?= 

.PHONY: build test fmt vet lint clean crd check

build: ## Build the binary
	mkdir -p bin
	go build -o bin/$(BIN) $(CMD)
	go build -o bin/$(BINCLI) $(CMDCLI)

fmt: ## Format code
	gofmt -s -w .

vet: ## go vet
	go vet $(PKG)

lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

clean: ## Remove build artifacts
	rm -rf bin

docker:
	docker build -t betsandpedestres:dev .

run-db:
	docker compose up db -d
	docker compose up adminer -d

run: build
	bin/betsandpedestres

check: fmt vet lint
