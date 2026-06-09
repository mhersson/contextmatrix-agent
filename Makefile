.PHONY: build test fmt lint eval
build:
	go build ./...
test:
	go test ./...
fmt:
	gofumpt -w .
lint:
	golangci-lint run
eval:
	go run ./cmd/contextmatrix-agent eval --role all --dry-run
