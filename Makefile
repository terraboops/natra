# Natra CNI Plugin Makefile

# Get the currently used golang install path
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

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
build: fmt vet build-cni ## Build CNI plugin binary.

CNI_BINARY := bin/natra

.PHONY: build-cni
build-cni: ## Build CNI plugin binary
	@mkdir -p bin
	go build -o $(CNI_BINARY) ./cmd/natra

.PHONY: clean
clean: ## Clean build artifacts
	rm -f $(CNI_BINARY)
	rm -f cover.out

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
