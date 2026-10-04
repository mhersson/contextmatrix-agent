.PHONY: build test test-race fmt lint docker-worker docker-worker-variants

# Pinned worker toolchain versions. Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-worker GO_VERSION=1.27.2
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION            ?= 1.27.1
GO_SHA256_AMD64       ?= 63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
GO_SHA256_ARM64       ?= 3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec
NPM_VERSION           ?= 12.2.0
GOLANGCI_LINT_VERSION ?= v2.14.0
GOFUMPT_VERSION       ?= v0.12.0
RUST_VERSION          ?= 1.99.0
RUSTUP_VERSION        ?= 1.29.1
RUSTUP_SHA256_AMD64   ?= dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71
RUSTUP_SHA256_ARM64   ?= 15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433
PYTHON_VERSION        ?= 3.14.8
TY_VERSION            ?= 0.0.84
RUFF_VERSION          ?= 0.16.10

# Build args shared by every worker-image target.
WORKER_BUILD_ARGS = \
	--build-arg GO_VERSION=$(GO_VERSION) \
	--build-arg GO_SHA256_AMD64=$(GO_SHA256_AMD64) \
	--build-arg GO_SHA256_ARM64=$(GO_SHA256_ARM64) \
	--build-arg NPM_VERSION=$(NPM_VERSION) \
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
