#!/usr/bin/env bash
# Print a uniform skip message for Linux-only Makefile targets when invoked
# on macOS. Used by `make test-bpf` and `make test-perf` (Layers 3 and 5)
# whose lvh + KVM dependency makes Docker Desktop infeasible.
#
# Usage:
#   scripts/skip-message.sh "Layer 3" "BPF dataplane matrix"

set -euo pipefail

LAYER="${1:-Layer ?}"
WHAT="${2:-this layer}"

cat <<EOF
${LAYER} (${WHAT}) needs lvh + qemu + KVM.
Nested KVM via Docker Desktop is unreliable, so Mac runs skip by default.
GH Actions runs the full kernel matrix on every push.

To iterate locally: install lima or orbstack, create a Linux VM, and run
the matching make target inside it. See TODO_LINUX.md.
EOF
