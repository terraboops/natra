# Natra - Network Guardian Spirits

[![CI](https://github.com/terraboops/natra/workflows/CI/badge.svg)](https://github.com/terraboops/natra/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/terraboops/natra)](https://goreportcard.com/report/github.com/terraboops/natra)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

> Drop-in CNI plugin replacement for Kubernetes bandwidth limiting with intelligent heavy hitter detection using eBPF.

## Overview

**Natra** (Nätrå - Network-Rå) protects your Kubernetes workloads from network traffic overload using:
- **Count-Min Sketch** for memory-efficient heavy hitter detection
- **Token Bucket** rate limiting for precise traffic control
- **tcx (TC eXpress)** for qdisc-less eBPF attachment that coexists with AWS VPC CNI

Unlike standard bandwidth plugins that rate limit ALL traffic uniformly, Natra detects heavy hitters within a Pod's flows and only throttles those - letting legitimate traffic flow freely.

## Status

**Active Development** - Phase 0 Complete (CNI Architecture)

## Quick Start

```bash
# Deploy CNI plugin installer to cluster
kubectl apply -f deploy/cni-installer.yaml

# Create a Pod with bandwidth annotation
kubectl run test --image=nginx --annotations="kubernetes.io/ingress-bandwidth=10M"
```

## Building

```bash
# Build CNI plugin
make build-cni

# Build Docker image
make docker-build

# Run tests
make test
```

## Requirements

- Linux kernel 6.6+ (for tcx support) or 5.x+ (clsact fallback)
- Go 1.22+
- clang/llvm (for eBPF compilation)
- Docker
- Kubernetes cluster (for deployment)

## Documentation

- [Architecture](docs/ARCHITECTURE.md) - System design and technical decisions
- [CNI Specification](docs/cni-spec.md) - CNI compliance documentation
- [Development Guide](docs/development.md) - Local setup and development workflow

## License

Apache License 2.0 - see [LICENSE](LICENSE) for details.
