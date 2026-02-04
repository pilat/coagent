.PHONY: help build test lint fmt all

.DEFAULT_GOAL := help

help:
	@echo "  build    compile the binary"
	@echo "  test     go test ./..."
	@echo "  lint     golangci-lint"
	@echo "  fmt      gofmt the tree"
	@echo "  all      fmt + build + lint + test"

all: fmt build lint test

build:
	go build -o coagent ./cmd/coagent

test:
	go test ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w ./cmd ./internal
