.PHONY: build test test-race fmt lint docker-worker docker-worker-variants

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
RUST_VERSION          ?= 1.97.0
RUSTUP_VERSION        ?= 1.29.0
RUSTUP_SHA256_AMD64   ?= 4acc9acc76d5079515b46346a485974457b5a79893cfb01112423c89aeb5aa10
RUSTUP_SHA256_ARM64   ?= 9732d6c5e2a098d3521fca8145d826ae0aaa067ef2385ead08e6feac88fa5792
PYTHON_VERSION        ?= 3.14.6
TY_VERSION            ?= 0.0.57
RUFF_VERSION          ?= 0.15.21

# Build args shared by every worker-image target.
WORKER_BUILD_ARGS = \
	--build-arg GO_VERSION=$(GO_VERSION) \
	--build-arg GO_SHA256_AMD64=$(GO_SHA256_AMD64) \
	--build-arg GO_SHA256_ARM64=$(GO_SHA256_ARM64) \
	--build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) \
	--build-arg GOFUMPT_VERSION=$(GOFUMPT_VERSION) \
	--build-arg RUST_VERSION=$(RUST_VERSION) \
	--build-arg RUSTUP_VERSION=$(RUSTUP_VERSION) \
	--build-arg RUSTUP_SHA256_AMD64=$(RUSTUP_SHA256_AMD64) \
	--build-arg RUSTUP_SHA256_ARM64=$(RUSTUP_SHA256_ARM64) \
	--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
	--build-arg TY_VERSION=$(TY_VERSION) \
	--build-arg RUFF_VERSION=$(RUFF_VERSION)

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
docker-worker: ## Build the default (full) worker image
	docker build \
		-f docker/Dockerfile.worker \
		--target full \
		$(WORKER_BUILD_ARGS) \
		-t contextmatrix-agent-worker:dev \
		.
docker-worker-variants: ## Build the slim worker variants (go-node, python, rust)
	for target in go-node python rust; do \
		docker build \
			-f docker/Dockerfile.worker \
			--target $$target \
			$(WORKER_BUILD_ARGS) \
			-t contextmatrix-agent-worker:$$target \
			. || exit 1; \
	done
