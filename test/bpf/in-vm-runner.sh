#!/usr/bin/env bash
# Executed inside the lvh qemu VM. Builds the BPF object, runs the Layer 3
# Go tests with the `bpf` build tag.

set -euo pipefail

KERNEL="${1:-unknown}"
echo "natra Layer 3 — kernel=${KERNEL} ($(uname -r))"

cd /workspace

make build-bpf
exec go test -tags=bpf -v ./test/bpf/...
