.PHONY: build test test-race fmt lint docker-worker

# Pinned worker toolchain versions. Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-worker GO_VERSION=1.26.4
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION            ?= 1.26.4
GO_SHA256_AMD64       ?= 1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f
GO_SHA256_ARM64       ?= ef758ae7c6cf9267c9c0ef080b8965f453d89ab2d25d9eb22de4405925238768
GOLANGCI_LINT_VERSION ?= v2.12.2
GOFUMPT_VERSION       ?= v0.10.0

build:
	go build ./...
	go build -trimpath -o contextmatrix-agent ./cmd/contextmatrix-agent
install:
	go install ./cmd/contextmatrix-agent
test:
	go test ./...
test-race:
	CGO_ENABLED=1 go test -race ./...
fmt:
	gofumpt -w .
lint:
	golangci-lint run
docker-worker: ## Build the worker image
	docker build \
		-f docker/Dockerfile.worker \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--build-arg GO_SHA256_AMD64=$(GO_SHA256_AMD64) \
		--build-arg GO_SHA256_ARM64=$(GO_SHA256_ARM64) \
		--build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) \
		--build-arg GOFUMPT_VERSION=$(GOFUMPT_VERSION) \
		-t contextmatrix-agent-worker:dev \
		.
