# Natra CNI Plugin Makefile

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Host detection. On macOS, every layer that needs a Linux kernel
# (L2, L3, L4, L5) goes through scripts/run-in-docker.sh, which runs
# them inside a single Docker container on colima's LinuxKit VM —
# one shared kernel across all test containers. No kernel isolation
# locally; see docs/test-environments.md for what that doesn't cover
# and what the next-step environments would add.
UNAME_S := $(shell uname -s)

# clang for BPF: Apple clang lacks the bpf target, so prefer Homebrew llvm
# if present. Override with BPF_CLANG=/path/to/clang on the command line.
ifeq ($(UNAME_S),Darwin)
BPF_CLANG ?= /opt/homebrew/opt/llvm/bin/clang
else
BPF_CLANG ?= clang
endif

CNI_BINARY := bin/natra
BPF_OBJS := bpf/natra.bpf.o bpf/placeholder.bpf.o bpf/vanilla.bpf.o
# Intentionally-invalid programs used by the L3 chaos suite. They MUST
# build (clang accepts them) but FAIL to load (verifier rejects). Listed
# here so `make build-bpf` produces them alongside the real ones.
BPF_TESTDATA_OBJS := bpf/testdata/invalid_oob_packet_access.bpf.o

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

.PHONY: fmt-check
fmt-check: ## Verify code is gofmt'd; non-zero exit if not. Read-only.
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt issues — run 'make fmt':"; \
		echo "$$out"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: license-scan
license-scan: ## Two-stage license scan (go-licenses + scancode-toolkit). Fails on GPL family.
	@bash scripts/license-scan.sh

.PHONY: license-scan-fast
license-scan-fast: ## Quick license check — Go module deps only (~5s).
	@bash scripts/license-scan.sh go-only

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: check
check: fmt vet lint ## Run all code quality checks

# pre-commit and pre-push are the targets the git hooks invoke. CI's
# `lint` job runs `make pre-commit` too, so what catches issues
# locally is exactly what catches them in CI — no drift possible.
.PHONY: pre-commit
pre-commit: fmt-check vet lint ## Fast checks the pre-commit hook runs (~20s). Run by CI's lint job.
	@go build ./...

.PHONY: pre-push
pre-push: pre-commit test-unit ## Pre-commit + L1 unit tests + a short fuzz (~1-2 min). Run by the pre-push hook.
	@go test -fuzz=FuzzParseBandwidthAnnotation -fuzztime=10s -test.timeout=60s -run=^$$ ./pkg/cni/config/...

.PHONY: hooks-install
hooks-install: ## Point git at .githooks/ so commit/push run the same checks CI does.
	@git config core.hooksPath ./.githooks
	@echo "Hooks installed: core.hooksPath = $$(git config core.hooksPath)"
	@echo "  pre-commit  → make pre-commit  (fmt-check, vet, lint, build)"
	@echo "  pre-push    → make pre-push    (pre-commit + L1 unit + 10s fuzz)"

.PHONY: hooks-uninstall
hooks-uninstall: ## Restore git's default hooks path.
	@git config --unset core.hooksPath || true
	@echo "Hooks uninstalled: core.hooksPath now $$(git config core.hooksPath || echo '(default)')"

##@ Build

.PHONY: build
build: fmt vet build-cni ## Build CNI plugin binary with BPF embedded (Linux ELF).

# build-cni first compiles bpf/natra.bpf.o and copies it into pkg/bpf/
# (where go:embed in loader.go reads it), then go-builds the natra
# binary with the BPF object embedded as a []byte.
#
# On Linux this is a straight invocation. On macOS it runs inside the
# Docker wrapper because BPF compile + Linux Go build aren't host-native.
.PHONY: build-cni
build-cni: ## Build natra binary with BPF embedded.
ifeq ($(UNAME_S),Linux)
	@$(MAKE) -s build-cni-inner
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq clang llvm make libbpf-dev linux-libc-dev >/dev/null && make -s build-cni-inner"
endif

# Inner build target — assumes Linux + clang already available. Don't
# call this directly on Mac; use `make build-cni` which wraps it in
# Docker.
.PHONY: build-cni-inner
build-cni-inner:
	@$(MAKE) -s build-bpf
	@mkdir -p bin pkg/bpf
	@cp bpf/natra.bpf.o pkg/bpf/natra.bpf.o
	@GOOS=linux CGO_ENABLED=0 go build -buildvcs=false -o $(CNI_BINARY) ./cmd/natra
	@echo "Built $(CNI_BINARY) ($$(file $(CNI_BINARY) | cut -d',' -f2))"

.PHONY: build-bpf
build-bpf: $(BPF_OBJS) $(BPF_TESTDATA_OBJS) ## Compile BPF C sources (incl. chaos testdata) to bytecode (.o).

# Pattern rule for both bpf/*.bpf.o and bpf/testdata/*.bpf.o. Single
# rule keeps build flags consistent — chaos-testdata programs MUST use
# the same toolchain as the real ones for the verifier-rejection
# assertions to be meaningful.
define BPF_COMPILE_RECIPE
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
	HOST_ARCH=$$(uname -m); \
	BPF_ARCH=$$(echo $$HOST_ARCH | sed 's/x86_64/x86/;s/aarch64/arm64/'); \
	GNU_TRIPLE=$$(echo $$HOST_ARCH | sed 's/aarch64/aarch64-linux-gnu/;s/x86_64/x86_64-linux-gnu/'); \
	$(BPF_CLANG) -O2 -g -Wall -Werror -target bpf -mcpu=v3 \
		-D__TARGET_ARCH_$$BPF_ARCH \
		-I/usr/include/$$GNU_TRIPLE \
		-c $< -o $@; \
	echo "Built $@"
endef

bpf/testdata/%.bpf.o: bpf/testdata/%.bpf.c
	$(BPF_COMPILE_RECIPE)

bpf/%.bpf.o: bpf/%.bpf.c
	$(BPF_COMPILE_RECIPE)

.PHONY: clean
clean: ## Clean build artifacts.
	rm -f $(CNI_BINARY) $(BPF_OBJS) $(BPF_TESTDATA_OBJS) cover.out

##@ Testing

.PHONY: test
test: test-unit ## Default test target — Layer 1 only (fast).

.PHONY: test-unit
test-unit: ## Layer 1a — Ginkgo unit tests.
	go test ./pkg/... ./internal/... -coverprofile cover.out

.PHONY: test-fuzz
test-fuzz: ## Layer 1b — 30s fuzz against the bandwidth annotation parser.
	go test -fuzz=FuzzParseBandwidthAnnotation -fuzztime=30s -test.timeout=2m -run=^$$ ./pkg/cni/config/...

.PHONY: test-bench
test-bench: ## Layer 1c — Hot-path benchmarks (no regression check; CI does that).
	go test -bench=. -benchmem -benchtime=1s -run=^$$ ./pkg/...

# ----- Linux-only layers (gated on uname -s) -----

.PHONY: test-cni
test-cni: ## Layer 2 — CNI protocol tests (Linux native or Mac via Docker).
ifeq ($(UNAME_S),Linux)
	sudo go test -tags=$(TAGS_INTEGRATION) ./test/cni/...
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq iproute2 >/dev/null && go test -tags=$(TAGS_INTEGRATION) ./test/cni/..."
endif

.PHONY: test-bpf
test-bpf: ## Layer 3 — BPF dataplane (Linux native; macOS: via Docker).
ifeq ($(UNAME_S),Linux)
	sudo -E env "PATH=$$PATH" go test -tags=$(TAGS_BPF) -v ./test/bpf/...
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq clang llvm make libbpf-dev linux-libc-dev >/dev/null && make build-bpf && go test -tags=bpf -v ./test/bpf/..."
endif

.PHONY: test-e2e
test-e2e: ## Layer 4 — k3d end-to-end (works on Mac with Docker; Linux native too).
	@if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then \
		echo "Layer 4 needs Docker. Start the daemon and retry."; \
	else \
		go test -tags=$(TAGS_E2E) ./test/e2e/...; \
	fi

.PHONY: test-perf
test-perf: ## Layer 5 — perf scenarios (Linux native; macOS: via Docker).
ifeq ($(UNAME_S),Linux)
	sudo -E env "PATH=$$PATH" KERNEL=local go test -tags=$(TAGS_PERF) -v ./test/perf/...
else
	@bash scripts/run-in-docker.sh "apt-get update -qq >/dev/null && apt-get install -y -qq clang llvm make libbpf-dev linux-libc-dev >/dev/null && make build-bpf && go test -tags=perf -v ./test/perf/..."
endif

.PHONY: perf-vs-vanilla
perf-vs-vanilla: ## Real-cluster head-to-head: natra vs upstream bandwidth plugin (~18-22 min; three k3d phases).
	@bash scripts/perf-vs-vanilla.sh

.PHONY: test-vm
test-vm: ## Layer 4 (kernel-isolated): two-VM k3s cluster via lima, real cross-kernel pod traffic. macOS needs socket_vmnet.
	@go run ./cmd/vm-rig all

.PHONY: perf-vs-vanilla-vm
perf-vs-vanilla-vm: ## natra vs upstream bandwidth on two real kernels (lima), flannel host-gw CNI. ~40 min. Fresh cluster per phase. Canonical two-kernel headline numbers.
	@go run ./cmd/vm-rig perfvsvanilla

.PHONY: perf-vs-vanilla-vm-cilium
perf-vs-vanilla-vm-cilium: ## Same as perf-vs-vanilla-vm but with cilium as the CNI (cni.exclusive=false, KPR off). Proxies AWS NPA; exercises bpf_mprog coexistence at pod TCX. ~50 min.
	@VMRIG_CNI=cilium go run ./cmd/vm-rig perfvsvanilla

.PHONY: perf-vs-vanilla-vm-cilium-kpr
perf-vs-vanilla-vm-cilium-kpr: ## Same as perf-vs-vanilla-vm-cilium but with KPR on (cilium replaces kube-proxy, bpf_redirect_peer fast-path). Exercises natra coexistence with cilium's full production config. ~50 min.
	@VMRIG_CNI=cilium-kpr go run ./cmd/vm-rig perfvsvanilla

.PHONY: test-all
test-all: test-unit test-fuzz test-cni test-bpf test-e2e test-perf ## All layers.

.PHONY: ci
ci: ## Run every layer end-to-end + lint + license scan. Keeps going past failures; reports per-layer status; exits nonzero if any failed.
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
	run_layer "lint"      $(MAKE) -s lint; \
	run_layer "licenses"  $(MAKE) -s license-scan-fast; \
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
# v2.5.0+ is built against go 1.25 (matching natra's go.mod). v2.3.0
# was built against go 1.24 and rejects the config when run against
# 1.25-targeted code — see ci.yml for the same constraint.
GOLANGCI_LINT_VERSION ?= v2.5.0

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
