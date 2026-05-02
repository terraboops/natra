# Natra CNI Plugin Makefile

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Host detection. Layers 2 and 4 invoke Docker on macOS; Layers 3 and 5
# require lvh + KVM and skip on macOS by default (see TODO_LINUX.md for
# the lima/orbstack escape hatch).
UNAME_S := $(shell uname -s)

# clang for BPF: Apple clang lacks the bpf target, so prefer Homebrew llvm
# if present. Override with BPF_CLANG=/path/to/clang on the command line.
ifeq ($(UNAME_S),Darwin)
BPF_CLANG ?= /opt/homebrew/opt/llvm/bin/clang
else
BPF_CLANG ?= clang
endif

CNI_BINARY := bin/natra
BPF_OBJ := bpf/placeholder.bpf.o
BPF_SRC := bpf/placeholder.bpf.c

# All Linux-test build tags. Each test file uses one of these.
TAGS_INTEGRATION := integration
TAGS_BPF := bpf
TAGS_E2E := e2e
TAGS_PERF := perf

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: check
check: fmt vet lint ## Run all code quality checks

##@ Build

.PHONY: build
build: fmt vet build-cni ## Build CNI plugin binary (Linux ELF, host arch).

.PHONY: build-cni
build-cni: ## Build natra binary for Linux (cross-compiles from macOS).
	@mkdir -p bin
	GOOS=linux CGO_ENABLED=0 go build -o $(CNI_BINARY) ./cmd/natra

.PHONY: build-bpf
build-bpf: $(BPF_OBJ) ## Compile BPF C source to bytecode (.o).

$(BPF_OBJ): $(BPF_SRC)
	@if [ ! -x "$(BPF_CLANG)" ] && ! command -v "$(BPF_CLANG)" >/dev/null 2>&1; then \
		echo ""; \
		echo "BPF compile needs LLVM clang ($(BPF_CLANG))."; \
		if [ "$(UNAME_S)" = "Darwin" ]; then \
			echo "Apple clang doesn't support -target bpf. Install with:"; \
			echo "  brew install llvm"; \
			echo "Or set BPF_CLANG=/path/to/clang on the make command."; \
			echo "Or skip — CI compiles BPF in the bpf workflow."; \
		fi; \
		echo ""; \
		exit 0; \
	fi; \
	$(BPF_CLANG) -O2 -g -target bpf -c $(BPF_SRC) -o $(BPF_OBJ); \
	echo "Built $(BPF_OBJ)"

.PHONY: clean
clean: ## Clean build artifacts.
	rm -f $(CNI_BINARY) $(BPF_OBJ) cover.out

##@ Testing

.PHONY: test
test: test-unit ## Default test target — Layer 1 only (fast).

.PHONY: test-unit
test-unit: ## Layer 1a — Ginkgo unit tests.
	go test ./pkg/... -coverprofile cover.out

.PHONY: test-fuzz
test-fuzz: ## Layer 1b — 30s fuzz against the bandwidth annotation parser.
	go test -fuzz=FuzzParseBandwidthAnnotation -fuzztime=30s -run=^$$ ./pkg/cni/config/...

.PHONY: test-bench
test-bench: ## Layer 1c — Hot-path benchmarks (no regression check; CI does that).
	go test -bench=. -benchmem -benchtime=1s -run=^$$ ./pkg/...

# ----- Linux-only layers (gated on uname -s) -----

.PHONY: test-cni
test-cni: ## Layer 2 — CNI protocol tests (Linux native or Mac via Docker).
ifeq ($(UNAME_S),Linux)
	sudo go test -tags=$(TAGS_INTEGRATION) ./test/cni/...
else
	@bash scripts/run-in-docker.sh "go test -tags=$(TAGS_INTEGRATION) ./test/cni/..."
endif

.PHONY: test-bpf
test-bpf: ## Layer 3 — BPF dataplane tests (Linux: lvh; Mac: colima kernel via Docker).
ifeq ($(UNAME_S),Linux)
	@KERNEL=$${KERNEL:-6.6}; bash test/bpf/run-in-vm.sh $$KERNEL
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq clang llvm make >/dev/null && make build-bpf && go test -tags=bpf -v ./test/bpf/..."
endif

.PHONY: test-bpf-all
test-bpf-all: ## Layer 3 — BPF tests across the kernel matrix (Linux only; Mac runs single colima kernel).
ifeq ($(UNAME_S),Linux)
	@for k in 5.15 6.6 bpf-next; do \
		echo "=== kernel $$k ==="; \
		bash test/bpf/run-in-vm.sh $$k || exit $$?; \
	done
else
	@$(MAKE) test-bpf
endif

.PHONY: test-e2e
test-e2e: ## Layer 4 — kind end-to-end tests (Linux native or Mac via Docker Desktop).
	@if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then \
		echo "Layer 4 needs Docker (Docker Desktop on macOS). Start it and retry, or see TODO_LINUX.md. Skipping."; \
	else \
		go test -tags=$(TAGS_E2E) ./test/e2e/...; \
	fi

.PHONY: test-perf
test-perf: ## Layer 5 — perf vs vanilla (Linux: lvh; Mac: colima kernel via Docker).
ifeq ($(UNAME_S),Linux)
	@KERNEL=$${KERNEL:-6.6}; bash test/perf/run.sh $$KERNEL && \
		go test -tags=$(TAGS_PERF) ./test/perf/...
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq clang llvm make >/dev/null && make build-bpf && go test -tags=perf -v ./test/perf/..."
endif

.PHONY: test-perf-all
test-perf-all: ## Layer 5 — full kernel matrix (Linux only).
ifeq ($(UNAME_S),Linux)
	@for k in 5.15 6.6 bpf-next; do \
		echo "=== kernel $$k ==="; \
		KERNEL=$$k $(MAKE) test-perf || exit $$?; \
	done
else
	@$(MAKE) test-perf
endif

.PHONY: perf-baseline
perf-baseline: ## Refresh perf baselines (run on main, commit in dedicated PR).
ifeq ($(UNAME_S),Linux)
	@KERNEL=$${KERNEL:-6.6}; \
		echo "Phase 1: regenerate test/perf/baselines/$$KERNEL.json"; \
		echo "Then commit in a dedicated PR titled: perf: refresh baselines for kernel $$KERNEL"
else
	@echo "perf-baseline must run on Linux. See TODO_LINUX.md."
endif

.PHONY: test-all
test-all: test-unit test-fuzz test-cni test-bpf test-e2e test-perf ## All layers (auto-skips on Mac for L3/L5).

.PHONY: ci
ci: ## Run every layer end-to-end. Keeps going past failures; reports per-layer status; exits nonzero if any failed.
	@FAIL=0; \
	declare -a RESULTS; \
	run_layer() { \
		local name="$$1"; shift; \
		echo ""; \
		echo "==================== $$name ===================="; \
		if "$$@"; then \
			RESULTS+=("PASS  $$name"); \
		else \
			RESULTS+=("FAIL  $$name"); \
			FAIL=$$((FAIL + 1)); \
		fi; \
	}; \
	run_layer "L1  unit"  $(MAKE) -s test-unit; \
	run_layer "L1  fuzz"  $(MAKE) -s test-fuzz; \
	run_layer "L1  bench" $(MAKE) -s test-bench; \
	run_layer "L2  cni"   $(MAKE) -s test-cni; \
	run_layer "L3  bpf"   $(MAKE) -s test-bpf; \
	run_layer "L4  e2e"   $(MAKE) -s test-e2e; \
	run_layer "L5  perf"  $(MAKE) -s test-perf; \
	echo ""; \
	echo "==================== ci summary ===================="; \
	for line in "$${RESULTS[@]}"; do echo "  $$line"; done; \
	echo ""; \
	if [ "$$FAIL" -gt 0 ]; then \
		echo "$$FAIL layer(s) failed"; \
		exit 1; \
	else \
		echo "all layers passed"; \
	fi

##@ Deployment

KUBECTL ?= kubectl

.PHONY: deploy
deploy: ## Deploy CNI plugin to the K8s cluster specified in ~/.kube/config.
	$(KUBECTL) apply -f deploy/cni-installer.yaml

.PHONY: undeploy
undeploy: ## Remove CNI plugin from the K8s cluster specified in ~/.kube/config.
	$(KUBECTL) delete -f deploy/cni-installer.yaml --ignore-not-found=true

##@ Docker

CNI_IMG ?= ghcr.io/terraboops/natra:latest

.PHONY: docker-build
docker-build: ## Build CNI plugin Docker image
	$(CONTAINER_TOOL) build -t $(CNI_IMG) -f deploy/docker/Dockerfile.cni .

.PHONY: docker-push
docker-push: ## Push CNI plugin Docker image
	$(CONTAINER_TOOL) push $(CNI_IMG)

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.3.0

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef

##@ Utilities

.PHONY: verify-kernel
verify-kernel: ## Verify kernel version and tcx support
	./scripts/verify-kernel.sh

.PHONY: generate-vmlinux
generate-vmlinux: ## Generate vmlinux.h from kernel BTF
	./scripts/generate-vmlinux.sh
