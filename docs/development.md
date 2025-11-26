# Development Guide

## Prerequisites

### Required Tools

- **Go 1.22+**: Language runtime
- **Docker**: Container builds
- **kubectl**: Kubernetes CLI
- **kind** or **minikube**: Local Kubernetes cluster

### Recommended Tools

- **golangci-lint**: Linting (auto-installed by Make)
- **bpftool**: eBPF debugging and vmlinux.h generation
- **clang/llvm**: eBPF compilation

## Local Development Setup

### 1. Clone Repository

```bash
git clone https://github.com/terraboops/natra.git
cd natra
```

### 2. Verify Kernel Requirements

```bash
./scripts/verify-kernel.sh
```

Should show:
- Kernel 5.x+ (minimum)
- tcx support (kernel 6.6+, preferred)
- BTF available

### 3. Install Development Dependencies

#### Arch Linux

```bash
yay -S bpftool clang llvm
```

#### Ubuntu/Debian

```bash
sudo apt-get install clang llvm linux-tools-common linux-tools-generic
```

## Development Workflow

### Building

```bash
# Build CNI plugin
make build-cni

# Build Docker image
make docker-build
```

### Testing

```bash
# Unit tests
make test
```

### Code Quality

```bash
# Format code
make fmt

# Vet code
make vet

# Lint
make lint

# All checks
make check
```

### Local Deployment

```bash
# Create Kind cluster
kind create cluster --name natra-dev

# Build and load image
make docker-build
kind load docker-image ghcr.io/terraboops/natra:latest --name natra-dev

# Deploy CNI installer
kubectl apply -f deploy/cni-installer.yaml

# Check status
kubectl get pods -n kube-system -l app=natra
```

## Project Structure

```
natra/
├── cmd/
│   └── natra/              # CNI plugin entry point
├── pkg/
│   └── cni/
│       └── config/         # Configuration parsing
├── deploy/
│   ├── docker/             # Dockerfiles
│   ├── cni-installer.yaml  # DaemonSet installer
│   └── cni-config.json     # Example CNI config
├── scripts/
│   ├── verify-kernel.sh    # Kernel check
│   └── generate-vmlinux.sh # vmlinux.h generation
└── docs/                   # Documentation
```

## Debugging

### CNI Plugin

```bash
# Test CNI plugin manually (requires CNI_* env vars)
echo '{"cniVersion":"0.4.0","name":"test"}' | CNI_COMMAND=VERSION ./bin/natra

# Check CNI logs (written to stderr, captured by kubelet)
journalctl -u kubelet | grep natra
```

### Testing with a Pod

```bash
# Create test pod with bandwidth annotation
kubectl run test --image=nginx --annotations="kubernetes.io/ingress-bandwidth=10M"

# Check pod status
kubectl get pod test -o yaml
```

## Contributing

### PR Workflow

1. Create feature branch
2. Make changes (keep PRs under 200 lines)
3. Run `make check test` locally
4. Create PR
5. CI runs automatically
6. Merge after approval
