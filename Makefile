.PHONY: build install history test test-race fmt lint docker-worker docker-worker-variants

# Pinned worker toolchain versions. Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-worker GO_VERSION=1.27.3
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION            ?= 1.27.2
GO_SHA256_AMD64       ?= ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
GO_SHA256_ARM64       ?= 94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8
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
YARN_VERSION          ?= 1.22.22
MAVEN_VERSION         ?= 3.9.16
MAVEN_SHA512          ?= 831a8591fe20c8243b1dbe7d71e3244f31d1665b0804b2e825e38cbbe5ce0cafb8338851f90780735568773e0a6cd07bbec107cda0b896b008b861075358b6f6

# Build info stamped into the binary and read back on /health.
VERSION    ?= $(shell git describe --tags --exact-match 2>/dev/null)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO  := github.com/mhersson/contextmatrix-agent/internal/buildinfo
LDFLAGS    := -X $(BUILDINFO).version=$(VERSION) -X $(BUILDINFO).commit=$(GIT_COMMIT) -X $(BUILDINFO).buildTime=$(BUILD_TIME)
HISTORY    := internal/buildinfo/history/merges.log

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
	--build-arg RUFF_VERSION=$(RUFF_VERSION) \
	--build-arg YARN_VERSION=$(YARN_VERSION) \
	--build-arg MAVEN_VERSION=$(MAVEN_VERSION) \
	--build-arg MAVEN_SHA512=$(MAVEN_SHA512)

# The revision label serve reads back for /health's worker_image_revision.
WORKER_LABELS = --label org.opencontainers.image.revision=$(GIT_COMMIT)

# Rerun on every build because HEAD moves; outside a git repo it leaves an
# empty file, so a build never fails for want of history.
history:
	git log --no-show-signature --first-parent --merges -n 100 --format=%cI%x1f%s%x1f%b%x1e > $(HISTORY) 2>/dev/null || : > $(HISTORY)
build: history
	go build ./...
	go build -trimpath -ldflags "$(LDFLAGS)" -o contextmatrix-agent ./cmd/contextmatrix-agent
install: history
	go install -ldflags "$(LDFLAGS)" ./cmd/contextmatrix-agent
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
		$(WORKER_LABELS) \
		-t contextmatrix-agent-worker:dev \
		.
docker-worker-variants: ## Build the slim worker variants (go-node, python, rust, jvm-node)
	for target in go-node python rust jvm-node; do \
		docker build \
			-f docker/Dockerfile.worker \
			--target $$target \
			$(WORKER_BUILD_ARGS) \
			$(WORKER_LABELS) \
			-t contextmatrix-agent-worker:$$target \
			. || exit 1; \
	done
