# SmolFaaS Makefile

BINARY_NAME=smolfaas
GO=go
GOFLAGS=-ldflags="-s -w"

.PHONY: all build clean test run up down check help

all: build

## Build the smolfaas binary
build:
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -o $(BINARY_NAME) .

## Clean build artifacts
clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-linux-amd64
	rm -rf .smolfaas/

## Run tests
test:
	$(GO) test -v ./...

## Run smolfaas up
up: build
	./$(BINARY_NAME) up

## Run smolfaas down
down: build
	./$(BINARY_NAME) down

## Run smolfaas down with force cleanup
down-force: build
	./$(BINARY_NAME) down --force

## Run smolfaas check
check: build
	./$(BINARY_NAME) check

## Install binary to GOPATH/bin
install:
	CGO_ENABLED=1 $(GO) install $(GOFLAGS) .

## Download dependencies
deps:
	$(GO) mod download
	$(GO) mod tidy

## Show help
help:
	@echo "SmolFaaS - A Simple Function-as-a-Service Platform"
	@echo ""
	@echo "Usage:"
	@echo "  make build       - Build the smolfaas binary"
	@echo "  make clean       - Clean build artifacts and state"
	@echo "  make test        - Run tests"
	@echo "  make up          - Build and run 'smolfaas up'"
	@echo "  make down        - Build and run 'smolfaas down'"
	@echo "  make down-force  - Build and run 'smolfaas down --force'"
	@echo "  make check       - Build and run 'smolfaas check'"
	@echo "  make install     - Install to GOPATH/bin"
	@echo "  make deps        - Download and tidy dependencies"
	@echo "  make help        - Show this help message"
