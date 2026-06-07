.PHONY: build test fmt lint
build:
	go build ./...
test:
	go test ./...
fmt:
	gofumpt -w .
lint:
	golangci-lint run
