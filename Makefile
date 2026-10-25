.PHONY: build install history test test-race test-docker fmt lint docker-worker docker-worker-variants

# Pinned worker toolchain versions. Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-worker GO_VERSION=1.27.4
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION            ?= 1.27.3
GO_SHA256_AMD64       ?= 65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
GO_SHA256_ARM64       ?= a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
NPM_VERSION           ?= 12.2.1
GOLANGCI_LINT_VERSION ?= v2.14.0
GOFUMPT_VERSION       ?= v0.12.0
RUST_VERSION          ?= 1.99.0
RUSTUP_VERSION        ?= 1.29.1
RUSTUP_SHA256_AMD64   ?= dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71
RUSTUP_SHA256_ARM64   ?= 15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433
PYTHON_VERSION        ?= 3.14.9
PIP_VERSION           ?= 26.2.2
TY_VERSION            ?= 0.0.84
RUFF_VERSION          ?= 0.16.10
YARN_VERSION          ?= 1.22.22
MAVEN_VERSION         ?= 3.9.16
MAVEN_SHA512          ?= 831a8591fe20c8243b1dbe7d71e3244f31d1665b0804b2e825e38cbbe5ce0cafb8338851f90780735568773e0a6cd07bbec107cda0b896b008b861075358b6f6
YQ_VERSION            ?= 4.54.1
YQ_SHA256_AMD64       ?= 8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f
YQ_SHA256_ARM64       ?= 189088da0c6429ec5178dfaab1a114805f6cab0b61b165ab236efedf1d57a71b
TASK_VERSION          ?= 3.54.0
TASK_SHA256_AMD64     ?= 680859dbb4d881a9c72d4d9a8f510825450849af8567deacd7302c01124416fb
TASK_SHA256_ARM64     ?= d7189d439d4a6058e39e1b2acd7c8b27acd4856691b408f23b242e4294e1df5b

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
	--build-arg PIP_VERSION=$(PIP_VERSION) \
	--build-arg TY_VERSION=$(TY_VERSION) \
	--build-arg RUFF_VERSION=$(RUFF_VERSION) \
	--build-arg YARN_VERSION=$(YARN_VERSION) \
	--build-arg MAVEN_VERSION=$(MAVEN_VERSION) \
	--build-arg MAVEN_SHA512=$(MAVEN_SHA512) \
	--build-arg YQ_VERSION=$(YQ_VERSION) \
	--build-arg YQ_SHA256_AMD64=$(YQ_SHA256_AMD64) \
	--build-arg YQ_SHA256_ARM64=$(YQ_SHA256_ARM64) \
	--build-arg TASK_VERSION=$(TASK_VERSION) \
	--build-arg TASK_SHA256_AMD64=$(TASK_SHA256_AMD64) \
	--build-arg TASK_SHA256_ARM64=$(TASK_SHA256_ARM64)

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
# Needs a Docker daemon and pulls alpine. It force-removes every agent-labelled
# container on the daemon, so never run it next to a live serve.
test-docker:
	CMX_TEST_DOCKER=1 go test -count=1 -run '^TestIntegration_' ./internal/executor/
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
